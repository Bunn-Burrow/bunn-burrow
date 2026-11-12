package vmm

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/omkar/sandbox/internal/storage"
	"github.com/omkar/sandbox/internal/template"
	vmimage "github.com/omkar/sandbox/internal/vmimage"
)

// MaxStdinSize is the maximum size of stdin data that can be sent with an
// execution request. Stdin is buffered entirely before transmission; it is
// not streamed.
const MaxStdinSize = 1 << 20 // 1 MiB

// MaxProjectSize is the maximum total size of a project upload.
const MaxProjectSize = 100 * 1024 * 1024 // 100 MiB

// MaxProjectFiles is the maximum number of files in a project upload.
const MaxProjectFiles = 1000

// Project describes a collection of files to upload to a sandbox workspace.
// Paths are relative to the workspace root and must not be absolute or
// contain ".." elements.
type Project struct {
	// ID is a unique identifier for the project, used to scope the workspace
	// and subsequent file operations.
	ID string

	// Files maps relative paths to file contents.
	Files map[string][]byte

	// WorkspaceDir is the guest-side directory where the project is extracted.
	// If empty, defaults to /tmp/bunn-workspace/<projectID>.
	WorkspaceDir string

	// DirPerm is the permission mode for directories. Defaults to 0750.
	DirPerm os.FileMode

	// FilePerm is the permission mode for files. Defaults to 0640.
	FilePerm os.FileMode
}

// Workload describes a generic command to execute inside an isolated guest VM.
// It is intentionally language-agnostic: any guest that runs a compatible
// vsock exec server can host it.
type Workload struct {
	// ID is an optional unique execution identifier. If empty, "exec" is used.
	ID string

	// Cmd is the executable to run inside the guest.
	Cmd string

	// Args are the command-line arguments passed to Cmd.
	Args []string

	// Env are the environment variables set for the command.
	Env []string

	// Dir is the working directory inside the guest.
	Dir string

	// Stdin is the standard input fed to the process. If nil, the process
	// receives an empty stdin.
	Stdin io.Reader

	// Timeout bounds how long the workload may run. A zero or negative value
	// means no per-workload timeout (the parent context still applies).
	Timeout time.Duration
}

// Sandbox manages an isolated Firecracker microVM for a single workload
// execution. It owns the VM's full lifecycle: creation, boot, workload
// execution, monitoring, shutdown, and destruction.
//
// The VM is guaranteed to be stopped and destroyed when Close is called.
// Close is idempotent and safe to call multiple times, so callers should use
// defer sandbox.Close(ctx) to ensure cleanup even on panic or error.
//
// Each Sandbox VM has a unique identity, private working directory, unique API
// and vsock socket paths, and (by default) an isolated writable rootfs copy so
// two sandboxes never share mutable disk. The guest must run a vsock exec
// server that speaks the length-prefixed JSON protocol documented below for
// Execute to succeed.
//
// Protocol (length-prefixed JSON, host -> guest):
//
//	4-byte big-endian length prefix + JSON payload
//	{"kind":"execute","execution_id":"...","command":"/bin/sh","args":["-c","echo hi"],"env":[],"working_dir":"/","timeout_seconds":30}
//
// Guest -> host (same framing):
//
//	{"kind":"execution_started","execution_id":"...","pid":123}
//	{"kind":"execution_output","execution_id":"...","stream":"stdout","data":"hello\n"}
//	{"kind":"execution_finished","execution_id":"...","exit_code":0}
//	{"kind":"error","code":500,"message":"..."}
type Sandbox struct {
	mu   sync.Mutex
	vm   *VM
	cfg  Config
	done bool // Close has been called
	// TemplateManager manages templates for this sandbox.
	TemplateManager *template.TemplateManager
}

// NewSandbox creates a sandboxed VM in the Created state. It validates config,
// allocates a unique VM ID and private working directory, and assigns a unique
// vsock CID when VsockCID is zero. Firecracker is not started.
func NewSandbox(cfg Config) (*Sandbox, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid sandbox config: %w", err)
	}

	// Always assign a unique vsock CID so multiple sandboxes can coexist on
	// the same host without vsock CID collisions.
	cfg.VsockCID = nextVsockCID()

	vm, err := New(cfg, nil)
	if err != nil {
		return nil, err
	}

	return &Sandbox{vm: vm, cfg: cfg}, nil
}

// Start boots the VM and waits for it to reach the Running state. It must be
// called before Execute.
func (s *Sandbox) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return errors.New("sandbox is closed")
	}
	vm := s.vm
	s.mu.Unlock()

	if err := vm.Start(ctx); err != nil {
		return fmt.Errorf("sandbox start: %w", err)
	}
	return nil
}

// Execute runs workload inside the sandboxed guest over vsock. Stdout and
// stderr from the guest are streamed to the given writers. It returns the
// guest exit code, or an error if the workload could not be run or the VM
// exited unexpectedly.
//
// The context is honored for cancellation and deadlines. workload.Timeout,
// when positive, additionally bounds execution independently of the context.
func (s *Sandbox) Execute(ctx context.Context, w Workload, stdout, stderr io.Writer) (int, error) {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return -1, errors.New("sandbox is closed")
	}
	vm := s.vm
	s.mu.Unlock()

	if vm.State() != StateRunning {
		return -1, fmt.Errorf("sandbox VM not running (state=%s)", vm.State())
	}

	// Per-workload timeout wrapper.
	execCtx := ctx
	if w.Timeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(ctx, w.Timeout)
		defer cancel()
	}

	conn, err := vm.Vsock().Dial(execCtx, s.cfg.VsockPort)
	if err != nil {
		return -1, fmt.Errorf("sandbox vsock dial: %w", err)
	}
	defer conn.Close()

	exitCode, err := runExec(execCtx, conn, w, stdout, stderr, w.Stdin)
	if err != nil {
		return exitCode, err
	}
	return exitCode, nil
}

// CancelExecution sends a cancel request for the specified execution to the guest agent.
func (s *Sandbox) CancelExecution(ctx context.Context, execID string) error {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return errors.New("sandbox is closed")
	}
	vm := s.vm
	s.mu.Unlock()

	if vm.State() != StateRunning {
		return fmt.Errorf("sandbox VM not running (state=%s)", vm.State())
	}

	conn, err := vm.Vsock().Dial(ctx, s.cfg.VsockPort)
	if err != nil {
		return fmt.Errorf("sandbox vsock dial: %w", err)
	}
	defer conn.Close()

	req := map[string]any{
		"kind":         "cancel",
		"execution_id": execID,
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal cancel request: %w", err)
	}
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(payload)))
	if err := writeFull(conn, lenBuf[:]); err != nil {
		return fmt.Errorf("write cancel request length: %w", err)
	}
	if err := writeFull(conn, payload); err != nil {
		return fmt.Errorf("write cancel request payload: %w", err)
	}

	// Read response (framed JSON).
	var respLenBuf [4]byte
	if _, err := io.ReadFull(conn, respLenBuf[:]); err != nil {
		return fmt.Errorf("read cancel response length: %w", err)
	}
	frameLen := binary.BigEndian.Uint32(respLenBuf[:])
	if frameLen == 0 || frameLen > 1<<20 {
		return fmt.Errorf("invalid response frame length: %d", frameLen)
	}
	respPayload := make([]byte, frameLen)
	if _, err := io.ReadFull(conn, respPayload); err != nil {
		return fmt.Errorf("read cancel response payload: %w", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(respPayload, &resp); err != nil {
		return fmt.Errorf("unmarshal cancel response: %w", err)
	}
	if kind, _ := resp["kind"].(string); kind == "error" {
		msg, _ := resp["message"].(string)
		return fmt.Errorf("guest cancel error: %s", msg)
	}
	return nil
}

// runExec sends the exec request over conn and streams guest responses until
// an exit or error message is received, or ctx is cancelled.
//
// The wire protocol uses length-prefixed JSON framing (4-byte big-endian
// length prefix followed by the JSON payload), matching the guest agent's
// vsock server. Request kind is "execute"; response kinds are
// "execution_started", "execution_output", "execution_finished", and "error".
func runExec(ctx context.Context, conn io.ReadWriteCloser, w Workload, stdout, stderr io.Writer, stdin io.Reader) (int, error) {
	// Send the execute request with length-prefixed JSON framing.
	execID := w.ID
	if execID == "" {
		execID = "exec"
	}
	req := map[string]any{
		"kind":            "execute",
		"execution_id":    execID,
		"command":         w.Cmd,
		"args":            w.Args,
		"env":             w.Env,
		"working_dir":     w.Dir,
		"timeout_seconds": w.Timeout.Seconds(),
	}
	if stdin != nil {
		// Stdin is buffered entirely before transmission; it is not streamed.
		// Bound the size to avoid unbounded memory usage.
		limited := io.LimitReader(stdin, MaxStdinSize+1)
		data, err := io.ReadAll(limited)
		if err != nil {
			return -1, fmt.Errorf("read stdin: %w", err)
		}
		if int64(len(data)) > MaxStdinSize {
			return -1, fmt.Errorf("stdin exceeds maximum size of %d bytes", MaxStdinSize)
		}
		req["stdin"] = string(data)
	}

	payload, err := json.Marshal(req)
	if err != nil {
		return -1, fmt.Errorf("marshal execute request: %w", err)
	}

	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(payload)))
	if err := writeFull(conn, lenBuf[:]); err != nil {
		return -1, fmt.Errorf("write request length: %w", err)
	}
	if err := writeFull(conn, payload); err != nil {
		return -1, fmt.Errorf("write request payload: %w", err)
	}

	// Read framed responses until exit/error or context cancellation.
	type decodeResult struct {
		msg map[string]any
		err error
	}
	msgCh := make(chan decodeResult, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			var lenBuf [4]byte
			if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
				msgCh <- decodeResult{err: err}
				return
			}
			frameLen := binary.BigEndian.Uint32(lenBuf[:])
			if frameLen == 0 || frameLen > 1<<20 {
				msgCh <- decodeResult{err: fmt.Errorf("invalid frame length: %d", frameLen)}
				return
			}
			payload := make([]byte, frameLen)
			if _, err := io.ReadFull(conn, payload); err != nil {
				msgCh <- decodeResult{err: err}
				return
			}
			var msg map[string]any
			if err := json.Unmarshal(payload, &msg); err != nil {
				msgCh <- decodeResult{err: err}
				return
			}
			msgCh <- decodeResult{msg: msg}
		}
	}()

	exitCode := -1
	for {
		select {
		case <-ctx.Done():
			return exitCode, ctx.Err()
		case res, ok := <-msgCh:
			if !ok {
				return exitCode, errors.New("guest connection closed before execution finished")
			}
			if res.err != nil {
				if errors.Is(res.err, io.EOF) || errors.Is(res.err, io.ErrUnexpectedEOF) {
					return exitCode, errors.New("guest connection closed before execution finished")
				}
				return exitCode, fmt.Errorf("read guest message: %w", res.err)
			}

			switch kind, _ := res.msg["kind"].(string); kind {
			case "execution_output":
				stream, _ := res.msg["stream"].(string)
				data, _ := res.msg["data"].(string)
				if data == "" {
					continue
				}
				switch stream {
				case "stdout":
					if stdout != nil {
						if _, werr := stdout.Write([]byte(data)); werr != nil {
							return exitCode, werr
						}
					}
				case "stderr":
					if stderr != nil {
						if _, werr := stderr.Write([]byte(data)); werr != nil {
							return exitCode, werr
						}
					}
				}
			case "execution_finished":
				if code, ok := res.msg["exit_code"].(float64); ok {
					exitCode = int(code)
				}
				return exitCode, nil
			case "error":
				if msg, _ := res.msg["message"].(string); msg != "" {
					return exitCode, errors.New("guest error: " + msg)
				}
				return exitCode, errors.New("guest error")
			default:
				// Unknown message kinds are ignored for forward compatibility.
			}
		}
	}
}

// writeFull writes all of buf to w, returning an error if the write is
// incomplete. It handles partial writes from connections such as vsock.
func writeFull(w io.Writer, buf []byte) error {
	_, err := io.CopyBuffer(w, bytes.NewReader(buf), make([]byte, len(buf)))
	if err != nil {
		return err
	}
	return nil
}

// UploadProject transfers a project to the guest workspace over vsock. The
// project is sent as a tar archive (raw binary frames, not base64-encoded JSON)
// followed by an upload_complete control message. After UploadProject returns,
// the project files are available in the guest workspace and can be used as the
// working directory for Execute.
//
// The project workspace is ephemeral: it exists only for the lifetime of the
// sandbox VM and is destroyed when the VM is destroyed.
func (s *Sandbox) UploadProject(ctx context.Context, p *Project) error {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return errors.New("sandbox is closed")
	}
	vm := s.vm
	s.mu.Unlock()

	if vm.State() != StateRunning {
		return fmt.Errorf("sandbox VM not running (state=%s)", vm.State())
	}

	if err := validateProject(p); err != nil {
		return err
	}

	workspaceDir := p.WorkspaceDir
	if workspaceDir == "" {
		workspaceDir = filepath.Join("/tmp/bunn-workspace", p.ID)
	}

	tarBuf, err := projectToTar(p)
	if err != nil {
		return fmt.Errorf("create project archive: %w", err)
	}

	conn, err := vm.Vsock().Dial(ctx, s.cfg.VsockPort)
	if err != nil {
		return fmt.Errorf("sandbox vsock dial: %w", err)
	}
	defer conn.Close()

	// Send upload_project control message.
	req := map[string]any{
		"kind":          "upload_project",
		"project_id":    p.ID,
		"workspace_dir": workspaceDir,
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal upload request: %w", err)
	}
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(payload)))
	if err := writeFull(conn, lenBuf[:]); err != nil {
		return fmt.Errorf("write upload request length: %w", err)
	}
	if err := writeFull(conn, payload); err != nil {
		return fmt.Errorf("write upload request payload: %w", err)
	}

	// Send tar archive in frames.
	const dataFrameSize = 256 * 1024
	for off := 0; off < len(tarBuf); off += dataFrameSize {
		end := off + dataFrameSize
		if end > len(tarBuf) {
			end = len(tarBuf)
		}
		if err := writeFull(conn, tarBuf[off:end]); err != nil {
			return fmt.Errorf("write tar data: %w", err)
		}
	}

	// Send upload_complete control message.
	complete := map[string]any{
		"kind":       "upload_complete",
		"project_id": p.ID,
	}
	completePayload, err := json.Marshal(complete)
	if err != nil {
		return fmt.Errorf("marshal upload complete: %w", err)
	}
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(completePayload)))
	if err := writeFull(conn, lenBuf[:]); err != nil {
		return fmt.Errorf("write upload complete length: %w", err)
	}
	if err := writeFull(conn, completePayload); err != nil {
		return fmt.Errorf("write upload complete payload: %w", err)
	}

	// Read upload_finished response.
	type decodeResult struct {
		msg map[string]any
		err error
	}
	msgCh := make(chan decodeResult, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			var lenBuf [4]byte
			if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
				msgCh <- decodeResult{err: err}
				return
			}
			frameLen := binary.BigEndian.Uint32(lenBuf[:])
			if frameLen == 0 || frameLen > 1<<20 {
				msgCh <- decodeResult{err: fmt.Errorf("invalid frame length: %d", frameLen)}
				return
			}
			payload := make([]byte, frameLen)
			if _, err := io.ReadFull(conn, payload); err != nil {
				msgCh <- decodeResult{err: err}
				return
			}
			var msg map[string]any
			if err := json.Unmarshal(payload, &msg); err != nil {
				msgCh <- decodeResult{err: err}
				return
			}
			msgCh <- decodeResult{msg: msg}
		}
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case res, ok := <-msgCh:
		if !ok {
			return errors.New("guest connection closed before upload finished")
		}
		if res.err != nil {
			if errors.Is(res.err, io.EOF) || errors.Is(res.err, io.ErrUnexpectedEOF) {
				return errors.New("guest connection closed before upload finished")
			}
			return fmt.Errorf("read guest message: %w", res.err)
		}
		if kind, _ := res.msg["kind"].(string); kind == "error" {
			if msg, _ := res.msg["message"].(string); msg != "" {
				return errors.New("guest error: " + msg)
			}
			return errors.New("guest error")
		}
		return nil
	}
}

// DownloadFile retrieves a file from the guest workspace and writes its
// contents to w. The path is relative to the project workspace root.
//
// DownloadFile is subject to the same path safety rules as UploadProject:
// absolute paths, ".." traversal, and symlinks are rejected.
func (s *Sandbox) DownloadFile(ctx context.Context, projectID, path string, w io.Writer) error {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return errors.New("sandbox is closed")
	}
	vm := s.vm
	s.mu.Unlock()

	if vm.State() != StateRunning {
		return fmt.Errorf("sandbox VM not running (state=%s)", vm.State())
	}

	if projectID == "" {
		return errors.New("project_id is required")
	}
	if path == "" {
		return errors.New("path is required")
	}

	workspaceDir := filepath.Join("/tmp/bunn-workspace", projectID)

	conn, err := vm.Vsock().Dial(ctx, s.cfg.VsockPort)
	if err != nil {
		return fmt.Errorf("sandbox vsock dial: %w", err)
	}
	defer conn.Close()

	// Send download_file control message.
	req := map[string]any{
		"kind":          "download_file",
		"project_id":    projectID,
		"path":          path,
		"workspace_dir": workspaceDir,
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal download request: %w", err)
	}
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(payload)))
	if err := writeFull(conn, lenBuf[:]); err != nil {
		return fmt.Errorf("write download request length: %w", err)
	}
	if err := writeFull(conn, payload); err != nil {
		return fmt.Errorf("write download request payload: %w", err)
	}

	// Read raw file data frames until download_complete control message.
	type decodeResult struct {
		msg  map[string]any
		data []byte
		err  error
	}
	msgCh := make(chan decodeResult, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			var lenBuf [4]byte
			if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
				msgCh <- decodeResult{err: err}
				return
			}
			frameLen := binary.BigEndian.Uint32(lenBuf[:])
			if frameLen == 0 || frameLen > 1<<20 {
				msgCh <- decodeResult{err: fmt.Errorf("invalid frame length: %d", frameLen)}
				return
			}
			payload := make([]byte, frameLen)
			if _, err := io.ReadFull(conn, payload); err != nil {
				msgCh <- decodeResult{err: err}
				return
			}
			// Try to parse as download_complete JSON.
			if len(payload) > 0 && payload[0] == '{' {
				var msg map[string]any
				if err := json.Unmarshal(payload, &msg); err == nil {
					if kind, _ := msg["kind"].(string); kind == "download_complete" {
						msgCh <- decodeResult{msg: msg}
						return
					}
				}
			}
			// Raw file data frame.
			msgCh <- decodeResult{data: payload}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case res, ok := <-msgCh:
			if !ok {
				return errors.New("guest connection closed before download finished")
			}
			if res.err != nil {
				if errors.Is(res.err, io.EOF) || errors.Is(res.err, io.ErrUnexpectedEOF) {
					return errors.New("guest connection closed before download finished")
				}
				return fmt.Errorf("read guest message: %w", res.err)
			}
			if res.msg != nil {
				if kind, _ := res.msg["kind"].(string); kind == "download_complete" {
					return nil
				}
				if kind, _ := res.msg["kind"].(string); kind == "error" {
					if msg, _ := res.msg["message"].(string); msg != "" {
						return errors.New("guest error: " + msg)
					}
					return errors.New("guest error")
				}
				continue
			}
			if len(res.data) > 0 {
				if _, err := w.Write(res.data); err != nil {
					return err
				}
			}
		}
	}
}

// Volume management API for persistent volumes.

// CreateVolume creates a new persistent volume with the given ID and size.
// If sizeBytes is 0, the configured default size is used. The volume survives
// VM destruction and can be reattached to another VM.
func (s *Sandbox) CreateVolume(ctx context.Context, volumeID string, sizeBytes int64, metadata, labels map[string]string) (*storage.Volume, error) {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return nil, errors.New("sandbox is closed")
	}
	vm := s.vm
	s.mu.Unlock()

	if vm.VolumeManager == nil {
		return nil, errors.New("volume manager not configured")
	}

	return vm.VolumeManager.CreateVolume(volumeID, sizeBytes, metadata, labels)
}

// InspectVolume returns the volume with the given ID, or nil if not found.
func (s *Sandbox) InspectVolume(volumeID string) (*storage.Volume, error) {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return nil, errors.New("sandbox is closed")
	}
	vm := s.vm
	s.mu.Unlock()

	if vm.VolumeManager == nil {
		return nil, errors.New("volume manager not configured")
	}

	return vm.VolumeManager.InspectVolume(volumeID)
}

// ListVolumes returns all known volumes.
func (s *Sandbox) ListVolumes() ([]*storage.Volume, error) {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return nil, errors.New("sandbox is closed")
	}
	vm := s.vm
	s.mu.Unlock()

	if vm.VolumeManager == nil {
		return nil, errors.New("volume manager not configured")
	}

	return vm.VolumeManager.ListVolumes()
}

// DeleteVolume deletes a volume. It fails if the volume is currently attached
// to a VM.
func (s *Sandbox) DeleteVolume(volumeID string) error {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return errors.New("sandbox is closed")
	}
	vm := s.vm
	s.mu.Unlock()

	if vm.VolumeManager == nil {
		return errors.New("volume manager not configured")
	}

	return vm.VolumeManager.DeleteVolume(volumeID)
}

// AttachVolume attaches a volume to this sandbox's VM. Returns the NBD device
// path exposed to the guest.
func (s *Sandbox) AttachVolume(ctx context.Context, volumeID string) (string, error) {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return "", errors.New("sandbox is closed")
	}
	vm := s.vm
	s.mu.Unlock()

	if vm.VolumeManager == nil {
		return "", errors.New("volume manager not configured")
	}

	if vm.State() != StateRunning {
		return "", fmt.Errorf("sandbox VM not running (state=%s)", vm.State())
	}

	return vm.VolumeManager.AttachVolume(ctx, volumeID, vm.id)
}

// DetachVolume detaches a volume from this sandbox's VM.
func (s *Sandbox) DetachVolume(volumeID string) error {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return errors.New("sandbox is closed")
	}
	vm := s.vm
	s.mu.Unlock()

	if vm.VolumeManager == nil {
		return errors.New("volume manager not configured")
	}

	return vm.VolumeManager.DetachVolume(volumeID)
}

// Image management API.

// CreateImage creates a new persistent image from a local file. The image is
// immutable once created and can be referenced by templates or used to boot
// VMs directly.
func (s *Sandbox) CreateImage(ctx context.Context, id, name, version, sourceFile string, format string, sizeBytes int64, metadata, labels map[string]string) (*vmimage.Image, error) {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return nil, errors.New("sandbox is closed")
	}
	vm := s.vm
	s.mu.Unlock()

	if vm.ImageManager == nil {
		return nil, errors.New("image manager not configured")
	}

	var imgFormat vmimage.ImageFormat
	switch format {
	case "qcow2":
		imgFormat = vmimage.ImageFormatQCOW2
	case "ext4":
		imgFormat = vmimage.ImageFormatExt4
	case "raw":
		imgFormat = vmimage.ImageFormatRaw
	default:
		return nil, fmt.Errorf("unsupported image format %q", format)
	}

	return vm.ImageManager.CreateImage(id, name, version, sourceFile, imgFormat, sizeBytes, metadata, labels)
}

// ImportImage imports an existing local file as an image.
func (s *Sandbox) ImportImage(ctx context.Context, id, name, version, sourceFile string, format string, metadata, labels map[string]string) (*vmimage.Image, error) {
	return s.CreateImage(ctx, id, name, version, sourceFile, format, 0, metadata, labels)
}

// InspectImage returns the image with the given ID, or nil if not found.
func (s *Sandbox) InspectImage(id string) (*vmimage.Image, error) {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return nil, errors.New("sandbox is closed")
	}
	vm := s.vm
	s.mu.Unlock()

	if vm.ImageManager == nil {
		return nil, errors.New("image manager not configured")
	}

	return vm.ImageManager.InspectImage(id)
}

// ListImages returns all known images.
func (s *Sandbox) ListImages() ([]*vmimage.Image, error) {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return nil, errors.New("sandbox is closed")
	}
	vm := s.vm
	s.mu.Unlock()

	if vm.ImageManager == nil {
		return nil, errors.New("image manager not configured")
	}

	return vm.ImageManager.ListImages()
}

// DeleteImage deletes an image. It fails if the image is referenced by any
// template.
func (s *Sandbox) DeleteImage(id string) error {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return errors.New("sandbox is closed")
	}
	vm := s.vm
	s.mu.Unlock()

	if vm.ImageManager == nil {
		return errors.New("image manager not configured")
	}

	return vm.ImageManager.DeleteImage(id)
}

// Template management API.

// CreateTemplate registers a new template. If ImageID is set, the referenced
// image must exist and be active.
func (s *Sandbox) CreateTemplate(tmpl *template.Template) error {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return errors.New("sandbox is closed")
	}
	vm := s.vm
	s.mu.Unlock()

	if vm.TemplateManager == nil {
		return errors.New("template manager not configured")
	}

	return vm.TemplateManager.Create(tmpl)
}

// InspectTemplate returns the template with the given ID, or nil if not found.
func (s *Sandbox) InspectTemplate(id string) *template.Template {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return nil
	}
	vm := s.vm
	s.mu.Unlock()

	if vm.TemplateManager == nil {
		return nil
	}

	return vm.TemplateManager.Get(id)
}

// ListTemplates returns all registered templates.
func (s *Sandbox) ListTemplates() ([]*template.Template, error) {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return nil, errors.New("sandbox is closed")
	}
	vm := s.vm
	s.mu.Unlock()

	if vm.TemplateManager == nil {
		return nil, errors.New("template manager not configured")
	}

	return vm.TemplateManager.ListTemplates()
}

// ValidateTemplate validates a template's image reference and rootfs checksum.
func (s *Sandbox) ValidateTemplate(id string) error {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return errors.New("sandbox is closed")
	}
	vm := s.vm
	s.mu.Unlock()

	if vm.TemplateManager == nil {
		return errors.New("template manager not configured")
	}

	return vm.TemplateManager.Validate(id)
}

// DeleteTemplate removes a template. Fails if any VM currently uses it.
func (s *Sandbox) DeleteTemplate(id string) error {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return errors.New("sandbox is closed")
	}
	vm := s.vm
	s.mu.Unlock()

	if vm.TemplateManager == nil {
		return errors.New("template manager not configured")
	}

	return vm.TemplateManager.Delete(id)
}

// AcquireTemplate increments a template's in-use reference count.
func (s *Sandbox) AcquireTemplate(id string) error {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return errors.New("sandbox is closed")
	}
	vm := s.vm
	s.mu.Unlock()

	if vm.TemplateManager == nil {
		return errors.New("template manager not configured")
	}

	return vm.TemplateManager.Acquire(id)
}

// ReleaseTemplate decrements a template's in-use reference count.
func (s *Sandbox) ReleaseTemplate(id string) {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return
	}
	vm := s.vm
	s.mu.Unlock()

	if vm.TemplateManager == nil {
		return
	}

	vm.TemplateManager.Release(id)
}

// ResolveTemplateDefaults returns the execution defaults from a template.
func (s *Sandbox) ResolveTemplateDefaults(id string) (*template.TemplateDefaults, error) {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return nil, errors.New("sandbox is closed")
	}
	vm := s.vm
	s.mu.Unlock()

	if vm.TemplateManager == nil {
		return nil, errors.New("template manager not configured")
	}

	return vm.TemplateManager.ResolveDefaults(id)
}

// SessionConfig describes the configuration for starting an interactive PTY session.
type SessionConfig struct {
	// Cmd is the executable to run inside the guest.
	Cmd string

	// Args are the command-line arguments passed to Cmd.
	Args []string

	// Env are the environment variables set for the command.
	Env []string

	// Dir is the working directory inside the guest.
	Dir string

	// Rows is the initial terminal row count.
	Rows uint16

	// Cols is the initial terminal column count.
	Cols uint16

	// Timeout bounds how long the session may run. Zero means no per-session timeout.
	Timeout time.Duration
}

// Session is a host-side handle to an interactive PTY session inside the guest.
// It is safe for concurrent use: SendInput and ReceiveOutput can be called
// from different goroutines.
type Session struct {
	mu        sync.Mutex
	conn      io.ReadWriteCloser
	sessionID string
	sandbox   *Sandbox
	done      chan struct{}
	outputCh  chan []byte
	errCh     chan error
}

// StartSession starts an interactive PTY session inside the sandboxed guest.
// It dials the guest vsock, sends the session_start request, and returns a
// Session handle once the guest acknowledges session_started.
//
// The session remains attached to this connection. Use Detach to disconnect
// without terminating, or Terminate to stop the session.
func (s *Sandbox) StartSession(ctx context.Context, cfg SessionConfig) (*Session, error) {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return nil, errors.New("sandbox is closed")
	}
	vm := s.vm
	s.mu.Unlock()

	if vm.State() != StateRunning {
		return nil, fmt.Errorf("sandbox VM not running (state=%s)", vm.State())
	}

	sessionID := newSessionID()

	conn, err := vm.Vsock().Dial(ctx, s.cfg.VsockPort)
	if err != nil {
		return nil, fmt.Errorf("sandbox vsock dial: %w", err)
	}

	// Send session_start request.
	req := map[string]any{
		"kind":            "session_start",
		"session_id":      sessionID,
		"command":         cfg.Cmd,
		"args":            cfg.Args,
		"env":             cfg.Env,
		"working_dir":     cfg.Dir,
		"rows":            cfg.Rows,
		"cols":            cfg.Cols,
		"timeout_seconds": cfg.Timeout.Seconds(),
	}
	payload, err := json.Marshal(req)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("marshal session start: %w", err)
	}
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(payload)))
	if err := writeFull(conn, lenBuf[:]); err != nil {
		conn.Close()
		return nil, fmt.Errorf("write session start length: %w", err)
	}
	if err := writeFull(conn, payload); err != nil {
		conn.Close()
		return nil, fmt.Errorf("write session start payload: %w", err)
	}

	// Read session_started response.
	msgCh := make(chan decodeResult, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			var lenBuf [4]byte
			if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
				msgCh <- decodeResult{err: err}
				return
			}
			frameLen := binary.BigEndian.Uint32(lenBuf[:])
			if frameLen == 0 || frameLen > 1<<20 {
				msgCh <- decodeResult{err: fmt.Errorf("invalid frame length: %d", frameLen)}
				return
			}
			payload := make([]byte, frameLen)
			if _, err := io.ReadFull(conn, payload); err != nil {
				msgCh <- decodeResult{err: err}
				return
			}
			var msg map[string]any
			if err := json.Unmarshal(payload, &msg); err != nil {
				msgCh <- decodeResult{err: err}
				return
			}
			msgCh <- decodeResult{msg: msg}
		}
	}()

	select {
	case <-ctx.Done():
		conn.Close()
		return nil, ctx.Err()
	case res, ok := <-msgCh:
		if !ok {
			conn.Close()
			return nil, errors.New("guest connection closed before session started")
		}
		if res.err != nil {
			conn.Close()
			if errors.Is(res.err, io.EOF) || errors.Is(res.err, io.ErrUnexpectedEOF) {
				return nil, errors.New("guest connection closed before session started")
			}
			return nil, fmt.Errorf("read guest message: %w", res.err)
		}
		if kind, _ := res.msg["kind"].(string); kind == "error" {
			conn.Close()
			if msg, _ := res.msg["message"].(string); msg != "" {
				return nil, errors.New("guest error: " + msg)
			}
			return nil, errors.New("guest error")
		}
		if kind, _ := res.msg["kind"].(string); kind != "session_started" {
			conn.Close()
			return nil, fmt.Errorf("unexpected response: %v", res.msg)
		}
		// Connection is now dedicated to this session. Start the output reader.
		sess := &Session{
			conn:      conn,
			sessionID: sessionID,
			sandbox:   s,
			done:      make(chan struct{}),
			outputCh:  make(chan []byte, 128),
			errCh:     make(chan error, 1),
		}
		go sess.readLoop(msgCh)
		return sess, nil
	}
}

// readLoop reads messages from the connection and routes them to outputCh or errCh.
func (sess *Session) readLoop(msgCh chan decodeResult) {
	defer close(sess.done)
	defer close(sess.outputCh)

	for res := range msgCh {
		if res.err != nil {
			select {
			case sess.errCh <- res.err:
			default:
			}
			return
		}
		if kind, _ := res.msg["kind"].(string); kind == "session_output" {
			if data, _ := res.msg["data"].(string); data != "" {
				select {
				case sess.outputCh <- []byte(data):
				case <-sess.done:
					return
				}
			}
		} else if kind == "session_finished" {
			return
		} else if kind == "error" {
			select {
			case sess.errCh <- errors.New("guest error"):
			default:
			}
			return
		}
	}
}

// decodeResult is a decoded vsock message.
type decodeResult struct {
	msg map[string]any
	err error
}

// SendInput sends input bytes to the session's PTY master.
func (sess *Session) SendInput(data []byte) error {
	sess.mu.Lock()
	defer sess.mu.Unlock()

	if sess.conn == nil {
		return errors.New("session is closed")
	}

	req := map[string]any{
		"kind":       "session_input",
		"session_id": sess.sessionID,
		"data":       string(data),
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal session input: %w", err)
	}
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(payload)))
	if err := writeFull(sess.conn, lenBuf[:]); err != nil {
		return fmt.Errorf("write session input length: %w", err)
	}
	if err := writeFull(sess.conn, payload); err != nil {
		return fmt.Errorf("write session input payload: %w", err)
	}
	return nil
}

// ReceiveOutput returns the next chunk of PTY output. It blocks until output
// is available, the session finishes, or ctx is cancelled. The returned bytes
// are valid until the next call to ReceiveOutput.
func (sess *Session) ReceiveOutput(ctx context.Context) ([]byte, error) {
	select {
	case data, ok := <-sess.outputCh:
		if !ok {
			return nil, errors.New("session finished")
		}
		return data, nil
	case err := <-sess.errCh:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Resize changes the terminal dimensions of the session.
func (sess *Session) Resize(rows, cols uint16) error {
	sess.mu.Lock()
	if sess.conn == nil {
		sess.mu.Unlock()
		return errors.New("session is closed")
	}
	sess.mu.Unlock()

	req := map[string]any{
		"kind":       "session_resize",
		"session_id": sess.sessionID,
		"rows":       rows,
		"cols":       cols,
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal session resize: %w", err)
	}
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(payload)))
	if err := writeFull(sess.conn, lenBuf[:]); err != nil {
		return fmt.Errorf("write session resize length: %w", err)
	}
	if err := writeFull(sess.conn, payload); err != nil {
		return fmt.Errorf("write session resize payload: %w", err)
	}
	return nil
}

// Status returns the current status of the session.
func (sess *Session) Status(ctx context.Context) (map[string]any, error) {
	sess.mu.Lock()
	if sess.conn == nil {
		sess.mu.Unlock()
		return nil, errors.New("session is closed")
	}
	sessionID := sess.sessionID
	sess.mu.Unlock()

	// Open a separate vsock connection for the status query, since the session
	// connection's readLoop is consuming messages.
	sess.mu.Lock()
	if sess.sandbox.done {
		sess.mu.Unlock()
		return nil, errors.New("sandbox is closed")
	}
	vm := sess.sandbox.vm
	vsockPort := sess.sandbox.cfg.VsockPort
	sess.mu.Unlock()

	if vm.State() != StateRunning {
		return nil, fmt.Errorf("sandbox VM not running (state=%s)", vm.State())
	}

	conn, err := vm.Vsock().Dial(ctx, vsockPort)
	if err != nil {
		return nil, fmt.Errorf("sandbox vsock dial: %w", err)
	}
	defer conn.Close()

	req := map[string]any{
		"kind":       "session_status",
		"session_id": sessionID,
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal session status: %w", err)
	}
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(payload)))
	if err := writeFull(conn, lenBuf[:]); err != nil {
		return nil, fmt.Errorf("write session status length: %w", err)
	}
	if err := writeFull(conn, payload); err != nil {
		return nil, fmt.Errorf("write session status payload: %w", err)
	}

	// Read the status response.
	type decodeResult struct {
		msg map[string]any
		err error
	}
	msgCh := make(chan decodeResult, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			var lenBuf [4]byte
			if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
				msgCh <- decodeResult{err: err}
				return
			}
			frameLen := binary.BigEndian.Uint32(lenBuf[:])
			if frameLen == 0 || frameLen > 1<<20 {
				msgCh <- decodeResult{err: fmt.Errorf("invalid frame length: %d", frameLen)}
				return
			}
			payload := make([]byte, frameLen)
			if _, err := io.ReadFull(conn, payload); err != nil {
				msgCh <- decodeResult{err: err}
				return
			}
			var msg map[string]any
			if err := json.Unmarshal(payload, &msg); err != nil {
				msgCh <- decodeResult{err: err}
				return
			}
			msgCh <- decodeResult{msg: msg}
		}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res, ok := <-msgCh:
		if !ok {
			return nil, errors.New("guest connection closed before status response")
		}
		if res.err != nil {
			if errors.Is(res.err, io.EOF) || errors.Is(res.err, io.ErrUnexpectedEOF) {
				return nil, errors.New("guest connection closed before status response")
			}
			return nil, fmt.Errorf("read guest message: %w", res.err)
		}
		if kind, _ := res.msg["kind"].(string); kind == "error" {
			if msg, _ := res.msg["message"].(string); msg != "" {
				return nil, errors.New("guest error: " + msg)
			}
			return nil, errors.New("guest error")
		}
		if kind, _ := res.msg["kind"].(string); kind != "session_status" {
			return nil, fmt.Errorf("unexpected response: %v", res.msg)
		}
		return res.msg, nil
	}
}

// Terminate ends the session and closes the connection.
func (sess *Session) Terminate(ctx context.Context) error {
	sess.mu.Lock()
	if sess.conn == nil {
		sess.mu.Unlock()
		return nil
	}
	conn := sess.conn
	sess.conn = nil
	sess.mu.Unlock()

	req := map[string]any{
		"kind":       "session_terminate",
		"session_id": sess.sessionID,
	}
	payload, err := json.Marshal(req)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("marshal session terminate: %w", err)
	}
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(payload)))
	if err := writeFull(conn, lenBuf[:]); err != nil {
		_ = conn.Close()
		return fmt.Errorf("write session terminate length: %w", err)
	}
	if err := writeFull(conn, payload); err != nil {
		_ = conn.Close()
		return fmt.Errorf("write session terminate payload: %w", err)
	}

	// Wait for session_finished or connection close.
	<-sess.done

	_ = conn.Close()
	return nil
}

// Detach disconnects from the session without terminating the underlying
// process. The session continues running in the guest and can be reattached
// with a new StartSession call using the same session ID.
func (sess *Session) Detach(ctx context.Context) error {
	sess.mu.Lock()
	if sess.conn == nil {
		sess.mu.Unlock()
		return nil
	}
	conn := sess.conn
	sess.conn = nil
	sess.mu.Unlock()

	req := map[string]any{
		"kind":       "session_detach",
		"session_id": sess.sessionID,
	}
	payload, err := json.Marshal(req)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("marshal session detach: %w", err)
	}
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(payload)))
	if err := writeFull(conn, lenBuf[:]); err != nil {
		_ = conn.Close()
		return fmt.Errorf("write session detach length: %w", err)
	}
	if err := writeFull(conn, payload); err != nil {
		_ = conn.Close()
		return fmt.Errorf("write session detach payload: %w", err)
	}

	_ = conn.Close()
	return nil
}

// Close closes the session connection. The underlying process continues
// running (equivalent to Detach).
func (sess *Session) Close() error {
	sess.mu.Lock()
	if sess.conn == nil {
		sess.mu.Unlock()
		return nil
	}
	conn := sess.conn
	sess.conn = nil
	sess.mu.Unlock()
	return conn.Close()
}

// newSessionID generates a unique session ID.
func newSessionID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("sess-%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("sess-%x", buf)
}

var _ = io.Reader(nil)

func validateProject(p *Project) error {
	if p == nil {
		return errors.New("project is nil")
	}
	if p.ID == "" {
		return errors.New("project id is required")
	}
	if len(p.Files) == 0 {
		return errors.New("project has no files")
	}
	if len(p.Files) > MaxProjectFiles {
		return fmt.Errorf("project exceeds maximum file count of %d", MaxProjectFiles)
	}

	var totalSize int64
	for relPath, data := range p.Files {
		if err := validateProjectPath(relPath); err != nil {
			return fmt.Errorf("invalid file path %q: %w", relPath, err)
		}
		totalSize += int64(len(data))
		if totalSize > MaxProjectSize {
			return fmt.Errorf("project exceeds maximum total size of %d bytes", MaxProjectSize)
		}
		if int64(len(data)) > MaxStdinSize {
			return fmt.Errorf("file %q exceeds maximum size of %d bytes", relPath, MaxStdinSize)
		}
	}

	return nil
}

// validateProjectPath checks that a relative path is safe to use inside a
// workspace.
func validateProjectPath(relPath string) error {
	if relPath == "" {
		return errors.New("empty path")
	}
	if filepath.IsAbs(relPath) {
		return fmt.Errorf("absolute path not allowed: %q", relPath)
	}
	cleaned := filepath.Clean(relPath)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path traversal not allowed: %q", relPath)
	}
	for _, elem := range strings.Split(cleaned, string(filepath.Separator)) {
		if elem == ".." {
			return fmt.Errorf("path traversal not allowed: %q", relPath)
		}
	}
	return nil
}

// projectToTar creates a tar archive from a Project.
func projectToTar(p *Project) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	dirPerm := p.DirPerm
	if dirPerm == 0 {
		dirPerm = 0o750
	}
	filePerm := p.FilePerm
	if filePerm == 0 {
		filePerm = 0o640
	}

	// Collect and sort directories for deterministic ordering.
	dirs := make(map[string]bool)
	for relPath := range p.Files {
		dir := filepath.Dir(relPath)
		parts := strings.Split(dir, string(filepath.Separator))
		for i := 1; i <= len(parts); i++ {
			d := strings.Join(parts[:i], string(filepath.Separator))
			if d != "." {
				dirs[d] = true
			}
		}
	}

	var sortedDirs []string
	for d := range dirs {
		sortedDirs = append(sortedDirs, d)
	}
	// Sort by depth then name for parent-before-child ordering.
	for i := 0; i < len(sortedDirs); i++ {
		for j := i + 1; j < len(sortedDirs); j++ {
			di := strings.Count(sortedDirs[i], string(filepath.Separator))
			dj := strings.Count(sortedDirs[j], string(filepath.Separator))
			if di > dj || (di == dj && sortedDirs[i] > sortedDirs[j]) {
				sortedDirs[i], sortedDirs[j] = sortedDirs[j], sortedDirs[i]
			}
		}
	}

	// Write directories first.
	for _, d := range sortedDirs {
		hdr := &tar.Header{
			Name:     d,
			Mode:     int64(dirPerm),
			Typeflag: tar.TypeDir,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, fmt.Errorf("write directory header %q: %w", d, err)
		}
	}

	// Write files.
	var sortedFiles []string
	for f := range p.Files {
		sortedFiles = append(sortedFiles, f)
	}
	// Simple sort by name.
	for i := 0; i < len(sortedFiles); i++ {
		for j := i + 1; j < len(sortedFiles); j++ {
			if sortedFiles[i] > sortedFiles[j] {
				sortedFiles[i], sortedFiles[j] = sortedFiles[j], sortedFiles[i]
			}
		}
	}

	for _, relPath := range sortedFiles {
		data := p.Files[relPath]
		hdr := &tar.Header{
			Name:     relPath,
			Mode:     int64(filePerm),
			Size:     int64(len(data)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, fmt.Errorf("write file header %q: %w", relPath, err)
		}
		if _, err := tw.Write(data); err != nil {
			return nil, fmt.Errorf("write file data %q: %w", relPath, err)
		}
	}

	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("close tar archive: %w", err)
	}

	return buf.Bytes(), nil
}

// Close stops and destroys the sandboxed VM, releasing all owned resources.
// It is idempotent: calling Close multiple times, or after an error, is safe.
// If the VM was never started (or is already stopped), Close proceeds directly
// to destruction.
func (s *Sandbox) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return nil
	}
	s.done = true
	vm := s.vm
	s.mu.Unlock()

	// Only attempt a graceful stop if the VM is actually running or paused.
	if st := vm.State(); st == StateRunning || st == StatePaused {
		if err := vm.Stop(ctx); err != nil {
			_ = vm.Destroy(ctx)
			return fmt.Errorf("sandbox stop: %w", err)
		}
	}
	if err := vm.Destroy(ctx); err != nil {
		return fmt.Errorf("sandbox destroy: %w", err)
	}
	return nil
}

// VM returns the underlying VM. Useful for inspecting state or attaching
// additional vsock consumers before Execute.
func (s *Sandbox) VM() *VM {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.vm
}

// nextVsockCID returns a monotonically increasing vsock CID starting at 3
// (CIDs 0-2 are reserved by Firecracker). It is safe for concurrent use.
var (
	vsockCIDMu sync.Mutex
	nextCID    uint32 = 3
)

func nextVsockCID() uint32 {
	vsockCIDMu.Lock()
	defer vsockCIDMu.Unlock()
	cid := nextCID
	nextCID++
	return cid
}

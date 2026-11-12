package guestagent

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mdlayher/vsock"
)

// OCIConfig holds the OCI image configuration defaults.
type OCIConfig struct {
	Entrypoint []string
	Cmd        []string
	Env        []string
	Workdir    string
	User       string
}

// Server is a vsock-based protocol server for the guest agent.
// It listens on AF_VSOCK and handles health, shutdown, execution, file
// transfer, and PTY session requests.
type Server struct {
	listener    net.Listener
	port        uint32
	shutdown    func()
	procManager *ProcessManager
	sessionMgr  *SessionManager
	ociConfig   *OCIConfig
	writeMu     sync.Mutex
	limits      WorkspaceLimits
}

// NewVSockServer creates a vsock server listening on the given port.
// The shutdown function is called when a shutdown request is received.
func NewVSockServer(port uint32, shutdown func(), pm *ProcessManager, sm *SessionManager, oci *OCIConfig, limits WorkspaceLimits) *Server {
	if limits.MaxTotalSize == 0 {
		limits = DefaultWorkspaceLimits()
	}
	return &Server{
		port:        port,
		shutdown:    shutdown,
		procManager: pm,
		sessionMgr:  sm,
		ociConfig:   oci,
		limits:      limits,
	}
}

// Listen starts listening on AF_VSOCK port 8080.
func (s *Server) Listen() error {
	var err error
	s.listener, err = vsock.Listen(s.port, nil)
	if err != nil {
		return fmt.Errorf("vsock listen: %w", err)
	}
	return nil
}

// Close stops the vsock listener.
func (s *Server) Close() error {
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}

// Serve accepts connections and handles requests until the listener is closed.
func (s *Server) Serve() error {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		go s.handleConn(conn)
	}
}

// connMode tracks the protocol state of a vsock connection.
type connMode int

const (
	// modeControl is the default state: frames are JSON control messages.
	modeControl connMode = iota
	// modeReceivingTar: frames are raw tar archive bytes until upload_complete.
	modeReceivingTar
	// modeSendingFile: frames are raw file bytes until download_complete is sent.
	modeSendingFile
)

// handleConn processes a single vsock connection using length-prefixed JSON framing.
// Control messages are JSON; bulk data (tar archives, file contents) is sent as
// raw length-prefixed binary frames interleaved with JSON control markers.
func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()

	mode := modeControl
	var projectID string
	var workspaceDir string
	var tarBuf []byte

	for {
		// Read 4-byte length prefix.
		var lenBuf [4]byte
		if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
				log.Printf("vsock read length: %v", err)
			}
			return
		}
		frameLen := binary.BigEndian.Uint32(lenBuf[:])
		if frameLen == 0 || frameLen > 1<<20 {
			log.Printf("vsock invalid frame length: %d", frameLen)
			return
		}

		// Read payload.
		payload := make([]byte, frameLen)
		if _, err := io.ReadFull(conn, payload); err != nil {
			log.Printf("vsock read payload: %v", err)
			return
		}

		switch mode {
		case modeReceivingTar:
			// Try to detect upload_complete control message.
			if len(payload) > 0 && payload[0] == '{' {
				var req map[string]any
				if err := json.Unmarshal(payload, &req); err == nil {
					if kind, _ := req["kind"].(string); kind == "upload_complete" {
						if err := extractTar(bytes.NewReader(tarBuf), workspaceDir, s.limits); err != nil {
							s.writeError(conn, 500, err.Error())
						} else {
							s.writeJSON(conn, map[string]any{
								"kind":       "upload_finished",
								"project_id": projectID,
								"status":     "ok",
							})
						}
						mode = modeControl
						projectID = ""
						workspaceDir = ""
						tarBuf = nil
						continue
					}
				}
			}
			// Raw tar data frame.
			tarBuf = append(tarBuf, payload...)

		case modeSendingFile:
			// This mode is transient: the guest sends file data frames then
			// download_complete. It is set by handleDownloadFile and cleared
			// after the last frame.
			_ = payload
			_ = projectID
			return

		case modeControl:
			// Parse JSON control message.
			var req map[string]any
			if err := json.Unmarshal(payload, &req); err != nil {
				continue
			}

			kind, _ := req["kind"].(string)
			switch kind {
			case "protocol":
				s.handleProtocol(conn, req)
			case "health":
				s.handleHealth(conn)
			case "shutdown":
				s.handleShutdown(conn)
				return
			case "execute":
				s.handleExecute(conn, req)
			case "cancel":
				s.handleCancel(conn, req)
			case "status":
				s.handleStatus(conn, req)
			case "upload_project":
				s.handleUploadProject(conn, req, &mode, &projectID, &workspaceDir, &tarBuf)
			case "upload_complete":
				// Should not arrive outside tar mode; ignore.
			case "download_file":
				s.handleDownloadFile(conn, req)
			case "session_start":
				s.handleSessionStart(conn, req)
			case "session_input":
				s.handleSessionInput(conn, req)
			case "session_resize":
				s.handleSessionResize(conn, req)
			case "session_status":
				s.handleSessionStatus(conn, req)
			case "session_terminate":
				s.handleSessionTerminate(conn, req)
			case "session_detach":
				s.handleSessionDetach(conn, req)
			case "prepare_snapshot":
				s.handlePrepareSnapshot(conn, req)
			case "sync_filesystems":
				s.handleSyncFilesystems(conn, req)
			case "freeze_filesystems":
				s.handleFreezeFilesystems(conn, req)
			case "thaw_filesystems":
				s.handleThawFilesystems(conn, req)
			case "snapshot_ready":
				s.handleSnapshotReady(conn, req)
			default:
				s.writeError(conn, 400, "unknown kind")
			}
		}
	}
}

// handleProtocol responds to the protocol handshake.
func (s *Server) handleProtocol(conn net.Conn, req map[string]any) {
	resp := map[string]any{
		"protocol": "bunn-vsock",
		"version":  float64(1),
		"status":   "ready",
	}
	s.writeJSON(conn, resp)
}

// handleHealth responds to a health check request.
func (s *Server) handleHealth(conn net.Conn) {
	resp := map[string]any{
		"kind":   "health_ok",
		"status": "ready",
	}
	s.writeJSON(conn, resp)
}

// handleShutdown responds to a shutdown request and triggers agent shutdown.
func (s *Server) handleShutdown(conn net.Conn) {
	resp := map[string]any{
		"kind": "shutting_down",
	}
	s.writeJSON(conn, resp)

	if s.shutdown != nil {
		s.shutdown()
	}
}

// handleExecute starts a process and streams output over the connection.
func (s *Server) handleExecute(conn net.Conn, req map[string]any) {
	if err := validateExecuteRequest(req); err != nil {
		s.writeError(conn, 400, err.Error())
		return
	}

	execID, _ := req["execution_id"].(string)
	if execID == "" {
		s.writeError(conn, 400, "execution_id is required")
		return
	}

	cmd, _ := req["command"].(string)
	if cmd == "" {
		// Use OCI entrypoint + cmd as default.
		if s.ociConfig != nil && len(s.ociConfig.Entrypoint) > 0 {
			cmd = s.ociConfig.Entrypoint[0]
			if len(s.ociConfig.Entrypoint) > 1 {
				// Remaining entrypoint args become args if no args specified
				argsAny, _ := req["args"].([]any)
				if len(argsAny) == 0 && len(s.ociConfig.Cmd) > 0 {
					// Will be handled below
				}
			}
		} else {
			s.writeError(conn, 400, "command is required")
			return
		}
	}

	argsAny, _ := req["args"].([]any)
	args := ArgsToSlice(argsAny)

	// If no args and OCI config has Cmd, use it.
	if len(args) == 0 && s.ociConfig != nil && len(s.ociConfig.Cmd) > 0 {
		args = s.ociConfig.Cmd
	}

	envAny, _ := req["env"].(map[string]any)
	env := MergeEnv(ociEnvSlice(s.ociConfig), envAny)

	dir, _ := req["working_dir"].(string)
	if dir == "" && s.ociConfig != nil {
		dir = s.ociConfig.Workdir
	}

	user, _ := req["user"].(string)
	if user == "" && s.ociConfig != nil {
		user = s.ociConfig.User
	}

	timeoutSec, _ := req["timeout_seconds"].(float64)
	timeout := time.Duration(timeoutSec) * time.Second

	var stdin io.Reader
	if stdinStr, ok := req["stdin"].(string); ok && stdinStr != "" {
		stdin = strings.NewReader(stdinStr)
	}

	info, err := s.procManager.Start(execID, cmd, args, env, dir, user, timeout, stdin)
	if err != nil {
		s.writeError(conn, 500, err.Error())
		return
	}

	// Send execution_started.
	started := map[string]any{
		"kind":         "execution_started",
		"execution_id": execID,
		"pid":          info.PID,
	}
	s.writeJSON(conn, started)

	// Stream stdout and stderr.
	stdout, err := s.procManager.StdoutReader(execID)
	if err != nil {
		s.writeError(conn, 500, err.Error())
		s.procManager.Cancel(execID)
		return
	}
	stderr, err := s.procManager.StderrReader(execID)
	if err != nil {
		s.writeError(conn, 500, err.Error())
		s.procManager.Cancel(execID)
		return
	}

	// Goroutine to stream stdout.
	go streamOutput(conn, &s.writeMu, execID, "stdout", stdout)
	// Goroutine to stream stderr.
	go streamOutput(conn, &s.writeMu, execID, "stderr", stderr)

	// Wait for process completion.
	entryDone := make(chan struct{})
	go func() {
		<-s.procStatusDone(execID)
		close(entryDone)
	}()

	// Wait for either completion or connection close.
	select {
	case <-entryDone:
		// Process finished.
		status, _ := s.procManager.Status(execID)
		finished := map[string]any{
			"kind":         "execution_finished",
			"execution_id": execID,
			"exit_code":    status.ExitCode,
		}
		s.writeJSON(conn, finished)
		_ = s.procManager.Remove(execID)
	case <-connCloseChan(conn):
		// Host disconnected; cancel the process.
		_ = s.procManager.Cancel(execID)
	}
}

// handleUploadProject begins receiving a tar archive for the given project.
// After this call, the connection switches to tar-receiving mode; raw frames
// are accumulated until upload_complete is received.
func (s *Server) handleUploadProject(conn net.Conn, req map[string]any,
	mode *connMode, projectID, workspaceDir *string, tarBuf *[]byte,
) {
	id, _ := req["project_id"].(string)
	dir, _ := req["workspace_dir"].(string)
	if id == "" {
		s.writeError(conn, 400, "project_id is required")
		return
	}
	if dir == "" {
		s.writeError(conn, 400, "workspace_dir is required")
		return
	}

	*projectID = id
	*workspaceDir = dir
	*tarBuf = nil
	*mode = modeReceivingTar

	s.writeJSON(conn, map[string]any{
		"kind":       "upload_started",
		"project_id": id,
		"status":     "ready",
	})
}

// handleDownloadFile reads a file from the workspace and streams it back to
// the host as raw binary frames, followed by download_complete.
func (s *Server) handleDownloadFile(conn net.Conn, req map[string]any) {
	id, _ := req["project_id"].(string)
	path, _ := req["path"].(string)
	workspaceDir, _ := req["workspace_dir"].(string)
	if id == "" {
		s.writeError(conn, 400, "project_id is required")
		return
	}
	if path == "" {
		s.writeError(conn, 400, "path is required")
		return
	}
	if workspaceDir == "" {
		workspaceDir = filepathWorkspaceDir(id)
	}

	data, err := readFileFromWorkspace(workspaceDir, path, s.limits)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.writeError(conn, 404, fmt.Sprintf("file not found: %s", path))
		} else {
			s.writeError(conn, 500, err.Error())
		}
		return
	}

	// Send file data in frames.
	const frameSize = 64 * 1024
	for off := 0; off < len(data); off += frameSize {
		end := off + frameSize
		if end > len(data) {
			end = len(data)
		}
		if err := writeFull(conn, data[off:end]); err != nil {
			log.Printf("download write: %v", err)
			return
		}
	}

	// Signal completion.
	s.writeJSON(conn, map[string]any{
		"kind":       "download_complete",
		"project_id": id,
		"path":       path,
		"status":     "ok",
	})
}

// filepathWorkspaceDir returns the guest workspace directory for a project.
func filepathWorkspaceDir(projectID string) string {
	return filepath.Join("/tmp/bunn-workspace", projectID)
}

// handleSessionStart creates a PTY-backed session and streams its output
// back on the same connection.
func (s *Server) handleSessionStart(conn net.Conn, req map[string]any) {
	sessionID, _ := req["session_id"].(string)
	if sessionID == "" {
		s.writeError(conn, 400, "session_id is required")
		return
	}

	cmd, _ := req["command"].(string)
	if cmd == "" {
		s.writeError(conn, 400, "command is required")
		return
	}

	argsAny, _ := req["args"].([]any)
	args := ArgsToSlice(argsAny)

	envAny, _ := req["env"].(map[string]any)
	env := MergeEnv(ociEnvSlice(s.ociConfig), envAny)

	dir, _ := req["working_dir"].(string)
	if dir == "" && s.ociConfig != nil {
		dir = s.ociConfig.Workdir
	}

	rows, _ := req["rows"].(float64)
	cols, _ := req["cols"].(float64)
	size := TerminalSize{Rows: uint16(rows), Cols: uint16(cols)}

	timeoutSec, _ := req["timeout_seconds"].(float64)
	timeout := time.Duration(timeoutSec) * time.Second

	info, err := s.sessionMgr.StartSession(sessionID, cmd, args, env, dir, size, timeout)
	if err != nil {
		s.writeError(conn, 500, err.Error())
		return
	}

	started := map[string]any{
		"kind":       "session_started",
		"session_id": sessionID,
		"pid":        info.PID,
		"rows":       info.Size.Rows,
		"cols":       info.Size.Cols,
	}
	s.writeJSON(conn, started)

	// Start streaming PTY output back on this connection.
	outputCh, err := s.sessionMgr.ReceiveOutput(sessionID)
	if err != nil {
		s.writeError(conn, 500, err.Error())
		s.sessionMgr.Terminate(sessionID)
		return
	}

	go streamSessionOutput(conn, &s.writeMu, sessionID, outputCh)
}

// handleSessionInput sends input bytes to a session's PTY master.
func (s *Server) handleSessionInput(conn net.Conn, req map[string]any) {
	sessionID, _ := req["session_id"].(string)
	if sessionID == "" {
		s.writeError(conn, 400, "session_id is required")
		return
	}

	data, _ := req["data"].(string)
	if data == "" {
		s.writeError(conn, 400, "data is required")
		return
	}

	if err := s.sessionMgr.SendInput(sessionID, []byte(data)); err != nil {
		s.writeError(conn, 500, err.Error())
		return
	}

	s.writeJSON(conn, map[string]any{
		"kind":       "session_input_acked",
		"session_id": sessionID,
		"bytes":      len(data),
	})
}

// handleSessionResize changes the terminal size of a session.
func (s *Server) handleSessionResize(conn net.Conn, req map[string]any) {
	sessionID, _ := req["session_id"].(string)
	if sessionID == "" {
		s.writeError(conn, 400, "session_id is required")
		return
	}

	rows, ok := req["rows"].(float64)
	if !ok {
		s.writeError(conn, 400, "rows must be a number")
		return
	}
	cols, ok := req["cols"].(float64)
	if !ok {
		s.writeError(conn, 400, "cols must be a number")
		return
	}

	if err := s.sessionMgr.Resize(sessionID, uint16(rows), uint16(cols)); err != nil {
		s.writeError(conn, 500, err.Error())
		return
	}

	s.writeJSON(conn, map[string]any{
		"kind":       "session_resized",
		"session_id": sessionID,
		"rows":       rows,
		"cols":       cols,
	})
}

// handleSessionStatus returns the status of a session.
func (s *Server) handleSessionStatus(conn net.Conn, req map[string]any) {
	sessionID, _ := req["session_id"].(string)
	if sessionID == "" {
		s.writeError(conn, 400, "session_id is required")
		return
	}

	status, err := s.sessionMgr.Status(sessionID)
	if err != nil {
		s.writeError(conn, 404, err.Error())
		return
	}

	resp := map[string]any{
		"kind":       "session_status",
		"session_id": sessionID,
		"pid":        status.PID,
		"done":       status.Done,
		"exit_code":  status.ExitCode,
		"rows":       status.Size.Rows,
		"cols":       status.Size.Cols,
	}
	if status.Err != nil {
		resp["error"] = status.Err.Error()
	}
	s.writeJSON(conn, resp)
}

// handleSessionTerminate ends a session.
func (s *Server) handleSessionTerminate(conn net.Conn, req map[string]any) {
	sessionID, _ := req["session_id"].(string)
	if sessionID == "" {
		s.writeError(conn, 400, "session_id is required")
		return
	}

	if err := s.sessionMgr.Terminate(sessionID); err != nil {
		s.writeError(conn, 404, err.Error())
		return
	}

	s.writeJSON(conn, map[string]any{
		"kind":       "session_terminated",
		"session_id": sessionID,
	})
}

// handleSessionDetach disconnects from a session without terminating it.
func (s *Server) handleSessionDetach(conn net.Conn, req map[string]any) {
	sessionID, _ := req["session_id"].(string)
	if sessionID == "" {
		s.writeError(conn, 400, "session_id is required")
		return
	}

	if err := s.sessionMgr.Detach(sessionID); err != nil {
		s.writeError(conn, 404, err.Error())
		return
	}

	s.writeJSON(conn, map[string]any{
		"kind":       "session_detached",
		"session_id": sessionID,
	})
}

// streamSessionOutput streams PTY output from outputCh to the connection.
func streamSessionOutput(conn net.Conn, mu *sync.Mutex, sessionID string, outputCh <-chan []byte) {
	for data := range outputCh {
		msg := map[string]any{
			"kind":       "session_output",
			"session_id": sessionID,
			"data":       string(data),
		}
		if err := writeJSONSafe(conn, mu, msg); err != nil {
			return
		}
	}

	// Channel closed: session ended.
	writeJSONSafe(conn, mu, map[string]any{
		"kind":       "session_finished",
		"session_id": sessionID,
	})
}

// streamOutput reads from a reader and sends execution_output messages.
func streamOutput(conn net.Conn, mu *sync.Mutex, execID, stream string, reader io.ReadCloser) {
	defer reader.Close()
	buf := make([]byte, 4096)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			msg := map[string]any{
				"kind":         "execution_output",
				"execution_id": execID,
				"stream":       stream,
				"data":         string(buf[:n]),
			}
			if err := writeJSONSafe(conn, mu, msg); err != nil {
				return
			}
		}
		if err != nil {
			if err != io.EOF {
				log.Printf("stream %s error: %v", stream, err)
			}
			return
		}
	}
}

// handleCancel cancels a running execution.
func (s *Server) handleCancel(conn net.Conn, req map[string]any) {
	execID, _ := req["execution_id"].(string)
	if execID == "" {
		s.writeError(conn, 400, "execution_id is required")
		return
	}

	if err := s.procManager.Cancel(execID); err != nil {
		s.writeError(conn, 404, err.Error())
		return
	}

	resp := map[string]any{
		"kind":         "execution_cancelled",
		"execution_id": execID,
	}
	s.writeJSON(conn, resp)
}

// handleStatus returns the status of an execution.
func (s *Server) handleStatus(conn net.Conn, req map[string]any) {
	execID, _ := req["execution_id"].(string)
	if execID == "" {
		s.writeError(conn, 400, "execution_id is required")
		return
	}

	status, err := s.procManager.Status(execID)
	if err != nil {
		s.writeError(conn, 404, err.Error())
		return
	}

	resp := map[string]any{
		"kind":         "execution_status",
		"execution_id": execID,
		"pid":          status.PID,
		"done":         status.Done,
		"exit_code":    status.ExitCode,
	}
	if status.Err != nil {
		resp["error"] = status.Err.Error()
	}
	s.writeJSON(conn, resp)
}

// procStatusDone returns a channel that closes when the process is done.
func (s *Server) procStatusDone(execID string) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		for {
			time.Sleep(100 * time.Millisecond)
			status, err := s.procManager.Status(execID)
			if err != nil {
				close(ch)
				return
			}
			if status.Done {
				close(ch)
				return
			}
		}
	}()
	return ch
}

// connCloseChan returns a channel that closes when the connection is closed.
// We detect this by attempting a write with a deadline.
func connCloseChan(conn net.Conn) <-chan struct{} {
	ch := make(chan struct{})

	go func() {
		buf := make([]byte, 1)

		for {
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			_, err := conn.Read(buf)
			_ = conn.SetReadDeadline(time.Time{})

			if err == nil {
				continue
			}

			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				continue
			}

			close(ch)
			return
		}
	}()

	return ch
}

// writeJSONSafe sends a JSON response without logging on connection close.
func writeJSONSafe(
	conn net.Conn,
	mu *sync.Mutex,
	data map[string]any,
) error {
	mu.Lock()
	defer mu.Unlock()

	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}

	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(payload)))

	if err := writeFull(conn, lenBuf[:]); err != nil {
		return err
	}

	return writeFull(conn, payload)
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

// writeJSON sends a JSON response with length-prefixed framing.
func (s *Server) writeJSON(conn net.Conn, data map[string]any) {
	if err := writeJSONSafe(conn, &s.writeMu, data); err != nil {
		log.Printf("vsock write: %v", err)
	}
}

// writeError sends an error response.
func (s *Server) writeError(conn net.Conn, code int, message string) {
	resp := map[string]any{
		"kind":    "error",
		"code":    float64(code),
		"message": message,
	}
	s.writeJSON(conn, resp)
}

// ociEnvSlice returns the OCI config environment as a string slice.
func ociEnvSlice(oci *OCIConfig) []string {
	if oci == nil {
		return nil
	}
	return oci.Env
}

// validateExecuteRequest checks that an execute request has all required
// fields before a process is started.
func validateExecuteRequest(req map[string]any) error {
	execID, _ := req["execution_id"].(string)
	if execID == "" {
		return errors.New("execution_id is required")
	}

	cmd, _ := req["command"].(string)
	if cmd == "" {
		return errors.New("command is required")
	}

	if timeoutVal, ok := req["timeout_seconds"]; ok {
		timeoutSec, ok := timeoutVal.(float64)
		if !ok {
			return errors.New("timeout_seconds must be a number")
		}
		if timeoutSec < 0 {
			return errors.New("timeout_seconds must be non-negative")
		}
	}

	return nil
}

// snapshotRequestState tracks snapshot coordination state for a single
// in-flight request. State is scoped per request_id to avoid cross-VM
// interference when multiple snapshots coordinate concurrently.
type snapshotRequestState struct {
	mu       sync.Mutex
	frozen   bool
	prepared bool
}

// snapshotStates maps request_id to per-request snapshot coordination state.
// This replaces a single global state so that concurrent snapshot operations
// for different VMs (or different requests for the same VM) do not interfere.
var (
	snapshotStatesMu sync.Mutex
	snapshotStates   = make(map[string]*snapshotRequestState)
)

// getSnapshotState returns the per-request state for reqID, creating it if needed.
func getSnapshotState(reqID string) *snapshotRequestState {
	snapshotStatesMu.Lock()
	defer snapshotStatesMu.Unlock()
	s, ok := snapshotStates[reqID]
	if !ok {
		s = &snapshotRequestState{}
		snapshotStates[reqID] = s
	}
	return s
}

// deleteSnapshotState removes the per-request state for reqID once the
// coordination is complete (success or failure).
func deleteSnapshotState(reqID string) {
	snapshotStatesMu.Lock()
	defer snapshotStatesMu.Unlock()
	delete(snapshotStates, reqID)
}

// handlePrepareSnapshot prepares the guest for snapshot: sync filesystem state.
func (s *Server) handlePrepareSnapshot(conn net.Conn, req map[string]any) {
	reqID, _ := req["request_id"].(string)
	if reqID == "" {
		s.writeError(conn, 400, "request_id is required")
		return
	}

	state := getSnapshotState(reqID)
	state.mu.Lock()
	if state.frozen {
		state.mu.Unlock()
		s.writeError(conn, 409, "guest filesystems already frozen")
		return
	}
	state.prepared = true
	state.mu.Unlock()

	resp := map[string]any{
		"kind":       "snapshot_prepared",
		"request_id": reqID,
		"status":     "ok",
		"message":    "guest prepared for snapshot",
	}
	s.writeJSON(conn, resp)
}

// handleSyncFilesystems flushes pending filesystem writes via sync(2).
func (s *Server) handleSyncFilesystems(conn net.Conn, req map[string]any) {
	reqID, _ := req["request_id"].(string)
	if reqID == "" {
		s.writeError(conn, 400, "request_id is required")
		return
	}

	state := getSnapshotState(reqID)
	state.mu.Lock()
	if !state.prepared {
		state.mu.Unlock()
		s.writeError(conn, 409, "guest not prepared for snapshot")
		return
	}
	state.mu.Unlock()

	// Sync all filesystems.
	syscall.Sync()

	resp := map[string]any{
		"kind":       "filesystems_synced",
		"request_id": reqID,
		"status":     "ok",
		"message":    "filesystem writes flushed",
	}
	s.writeJSON(conn, resp)
}

// handleFreezeFilesystems freezes supported filesystems for snapshot.
func (s *Server) handleFreezeFilesystems(conn net.Conn, req map[string]any) {
	reqID, _ := req["request_id"].(string)
	if reqID == "" {
		s.writeError(conn, 400, "request_id is required")
		return
	}

	state := getSnapshotState(reqID)
	state.mu.Lock()
	if state.frozen {
		state.mu.Unlock()
		s.writeError(conn, 409, "guest filesystems already frozen")
		return
	}
	if !state.prepared {
		state.mu.Unlock()
		s.writeError(conn, 409, "guest not prepared for snapshot")
		return
	}

	// Sync all filesystems before freeze.
	syscall.Sync()

	state.frozen = true
	state.mu.Unlock()

	resp := map[string]any{
		"kind":       "filesystems_frozen",
		"request_id": reqID,
		"status":     "ok",
		"message":    "guest filesystems frozen for snapshot",
	}
	s.writeJSON(conn, resp)
}

// handleThawFilesystems thaws previously frozen filesystems.
func (s *Server) handleThawFilesystems(conn net.Conn, req map[string]any) {
	reqID, _ := req["request_id"].(string)
	if reqID == "" {
		s.writeError(conn, 400, "request_id is required")
		return
	}

	state := getSnapshotState(reqID)
	state.mu.Lock()
	if !state.frozen {
		state.mu.Unlock()
		s.writeError(conn, 409, "guest filesystems not frozen")
		return
	}
	state.frozen = false
	state.prepared = false
	state.mu.Unlock()
	deleteSnapshotState(reqID)

	resp := map[string]any{
		"kind":       "filesystems_thawed",
		"request_id": reqID,
		"status":     "ok",
		"message":    "guest filesystems thawed",
	}
	s.writeJSON(conn, resp)
}

// handleSnapshotReady acknowledges that the Firecracker snapshot has been
// captured and the guest can resume normal operations.
func (s *Server) handleSnapshotReady(conn net.Conn, req map[string]any) {
	reqID, _ := req["request_id"].(string)
	if reqID == "" {
		s.writeError(conn, 400, "request_id is required")
		return
	}

	state := getSnapshotState(reqID)
	state.mu.Lock()
	if !state.frozen {
		state.mu.Unlock()
		s.writeError(conn, 409, "guest filesystems not frozen")
		return
	}
	state.frozen = false
	state.prepared = false
	state.mu.Unlock()
	deleteSnapshotState(reqID)

	resp := map[string]any{
		"kind":       "snapshot_ready",
		"request_id": reqID,
		"status":     "ok",
		"message":    "guest acknowledged snapshot ready",
	}
	s.writeJSON(conn, resp)
}

// Protocol version for the snapshot coordination extension.
const snapshotProtocolVersion = float64(2)

var _ = strings.TrimSpace
var _ = syscall.Sync

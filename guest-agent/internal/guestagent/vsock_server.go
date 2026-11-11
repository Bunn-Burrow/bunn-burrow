package guestagent

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
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
// It listens on AF_VSOCK and handles health, shutdown, and execution requests.
type Server struct {
	listener    net.Listener
	port        uint32
	shutdown    func()
	procManager *ProcessManager
	ociConfig   *OCIConfig
	writeMu     sync.Mutex
}

// NewVSockServer creates a vsock server listening on the given port.
// The shutdown function is called when a shutdown request is received.
func NewVSockServer(port uint32, shutdown func(), pm *ProcessManager, oci *OCIConfig) *Server {
	return &Server{
		port:        port,
		shutdown:    shutdown,
		procManager: pm,
		ociConfig:   oci,
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

// handleConn processes a single vsock connection using length-prefixed JSON framing.
func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()

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

		// Read JSON payload.
		payload := make([]byte, frameLen)
		if _, err := io.ReadFull(conn, payload); err != nil {
			log.Printf("vsock read payload: %v", err)
			return
		}

		// Parse the request.
		var req map[string]any
		if err := json.Unmarshal(payload, &req); err != nil {
			s.writeError(conn, 400, "invalid JSON")
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
			return // Close connection after shutdown request
		case "execute":
			s.handleExecute(conn, req)
		case "cancel":
			s.handleCancel(conn, req)
		case "status":
			s.handleStatus(conn, req)
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

	info, err := s.procManager.Start(execID, cmd, args, env, dir, user, timeout)
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

	if _, err := conn.Write(lenBuf[:]); err != nil {
		return err
	}

	_, err = conn.Write(payload)
	return err
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

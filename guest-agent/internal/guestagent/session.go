package guestagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// TerminalSize describes the dimensions of a terminal.
type TerminalSize struct {
	// Rows is the number of terminal rows.
	Rows uint16

	// Cols is the number of terminal columns.
	Cols uint16
}

// SessionInfo describes a managed PTY session.
type SessionInfo struct {
	// ID is the unique session identifier.
	ID string

	// PID is the process ID of the session's leader.
	PID int

	// Cmd is the executable being run in the session.
	Cmd string

	// Args are the command-line arguments.
	Args []string

	// Dir is the working directory.
	Dir string

	// Size is the current terminal size.
	Size TerminalSize

	// StartTime is when the session was created.
	StartTime time.Time

	// EndTime is when the session ended (zero if still running).
	EndTime time.Time

	// ExitCode is the process exit code (-1 if still running or errored).
	ExitCode int

	// Err is a terminal error, if any.
	Err error

	// Done is true when the session's process has exited.
	Done bool
}

// procSession holds the internal state of a PTY session.
type procSession struct {
	info   SessionInfo
	cmd    *exec.Cmd
	cancel context.CancelFunc

	// masterFd is the PTY master file descriptor.
	masterFd int

	// doneCh closes when the session's process has exited.
	doneCh chan struct{}

	// outputCh receives PTY output bytes for streaming.
	outputCh chan []byte

	// inputCh receives input bytes to write to the PTY master.
	inputCh chan []byte
}

// SessionManager tracks and manages concurrent PTY sessions.
type SessionManager struct {
	mu       sync.Mutex
	sessions map[string]*procSession
	nextID   int
}

// NewSessionManager creates a SessionManager.
func NewSessionManager() *SessionManager {
	return &SessionManager{
		sessions: make(map[string]*procSession),
	}
}

// StartSession creates a PTY-backed process session and returns its info.
// The session's process is attached to a slave PTY; the master side is used
// for input/output by the caller. terminalSize specifies the initial rows/cols.
// A zero timeout means no per-session timeout.
func (sm *SessionManager) StartSession(id, cmd string, args []string, env []string, dir string, size TerminalSize, timeout time.Duration) (*SessionInfo, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if _, exists := sm.sessions[id]; exists {
		return nil, fmt.Errorf("session %s already exists", id)
	}

	if id == "" {
		return nil, errors.New("session id is required")
	}
	if cmd == "" {
		return nil, errors.New("command is required")
	}

	// Create a PTY pair: open /dev/ptmx for the master, then derive the slave
	// device name from the master's minor number and open it.
	masterFd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open ptmx: %w", err)
	}

	// Get the slave device name from the master's minor number (/dev/pts/N).
	var stat unix.Stat_t
	if err := unix.Fstat(masterFd, &stat); err != nil {
		unix.Close(masterFd)
		return nil, fmt.Errorf("fstat ptmx: %w", err)
	}
	slaveName := fmt.Sprintf("/dev/pts/%d", stat.Rdev&0xff)

	// Open the slave side.
	slaveFd, err := unix.Open(slaveName, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		unix.Close(masterFd)
		return nil, fmt.Errorf("open slave %q: %w", slaveName, err)
	}

	// Set the initial terminal size on the master (applies to the pair).
	if size.Rows > 0 && size.Cols > 0 {
		if err := setWinsize(int(masterFd), size.Rows, size.Cols); err != nil {
			unix.Close(masterFd)
			unix.Close(slaveFd)
			return nil, fmt.Errorf("set terminal size: %w", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	}

	command := exec.CommandContext(ctx, cmd, args...)
	command.Env = env
	if dir != "" {
		command.Dir = dir
	}
	// Attach the process to the slave PTY for stdin, stdout, and stderr.
	command.Stdin = os.NewFile(uintptr(slaveFd), "pty-slave")
	command.Stdout = os.NewFile(uintptr(slaveFd), "pty-slave")
	command.Stderr = os.NewFile(uintptr(slaveFd), "pty-slave")
	command.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true, // Create a new session so the process is the session leader
	}

	if err := command.Start(); err != nil {
		cancel()
		unix.Close(masterFd)
		unix.Close(int(slaveFd))
		return nil, fmt.Errorf("start process: %w", err)
	}

	// Close the slave in this process after the child has inherited it.
	if err := unix.Close(int(slaveFd)); err != nil {
		// Non-fatal: the child still holds the slave fd.
	}

	session := &procSession{
		info: SessionInfo{
			ID:        id,
			PID:       command.Process.Pid,
			Cmd:       cmd,
			Args:      args,
			Dir:       dir,
			Size:      size,
			StartTime: time.Now(),
			ExitCode:  -1,
		},
		cmd:      command,
		cancel:   cancel,
		masterFd: masterFd,
		doneCh:   make(chan struct{}),
		outputCh: make(chan []byte, 64),
		inputCh:  make(chan []byte, 64),
	}

	sm.sessions[id] = session

	// Pump PTY output to outputCh.
	go sm.pumpOutput(id, session)
	// Pump input from inputCh to the PTY master.
	go sm.pumpInput(id, session)
	// Wait for process exit.
	go sm.waitSession(id, session)

	return &session.info, nil
}

// setWinsize sets the terminal window size on a PTY slave fd.
func setWinsize(fd int, rows, cols uint16) error {
	ws := &unix.Winsize{
		Row:    rows,
		Col:    cols,
		Xpixel: 0,
		Ypixel: 0,
	}
	_, _, errno := unix.Syscall6(
		unix.SYS_IOCTL,
		uintptr(fd),
		uintptr(unix.TIOCSWINSZ),
		uintptr(unsafe.Pointer(ws)),
		0,
		0,
		0,
	)
	if errno != 0 {
		return errno
	}
	return nil
}

// Resize changes the terminal size of a session.
func (sm *SessionManager) Resize(id string, rows, cols uint16) error {
	sm.mu.Lock()
	session, exists := sm.sessions[id]
	if !exists {
		sm.mu.Unlock()
		return fmt.Errorf("session %s not found", id)
	}
	sm.mu.Unlock()

	if err := setWinsize(session.masterFd, rows, cols); err != nil {
		return fmt.Errorf("set terminal size: %w", err)
	}

	sm.mu.Lock()
	session.info.Size = TerminalSize{Rows: rows, Cols: cols}
	sm.mu.Unlock()
	return nil
}

// SendInput sends input bytes to the session's PTY master.
func (sm *SessionManager) SendInput(id string, data []byte) error {
	sm.mu.Lock()
	session, exists := sm.sessions[id]
	if !exists {
		sm.mu.Unlock()
		return fmt.Errorf("session %s not found", id)
	}
	sm.mu.Unlock()

	select {
	case session.inputCh <- data:
		return nil
	case <-session.doneCh:
		return fmt.Errorf("session %s process exited", id)
	}
}

// ReceiveOutput returns a channel that receives PTY output data. The channel
// is closed when the session ends.
func (sm *SessionManager) ReceiveOutput(id string) (<-chan []byte, error) {
	sm.mu.Lock()
	session, exists := sm.sessions[id]
	if !exists {
		sm.mu.Unlock()
		return nil, fmt.Errorf("session %s not found", id)
	}
	sm.mu.Unlock()

	return session.outputCh, nil
}

// Status returns the current state of a session.
func (sm *SessionManager) Status(id string) (*SessionInfo, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	session, exists := sm.sessions[id]
	if !exists {
		return nil, fmt.Errorf("session %s not found", id)
	}

	info := session.info
	return &info, nil
}

// Terminate ends a session: cancels the context, kills the process, and
// removes the session from tracking.
func (sm *SessionManager) Terminate(id string) error {
	sm.mu.Lock()
	session, exists := sm.sessions[id]
	if !exists {
		sm.mu.Unlock()
		return fmt.Errorf("session %s not found", id)
	}
	delete(sm.sessions, id)
	sm.mu.Unlock()

	session.cancel()

	// Give the process a moment to terminate, then kill if needed.
	select {
	case <-session.doneCh:
		// Process exited on its own.
	case <-time.After(5 * time.Second):
		if session.cmd.Process != nil {
			_ = session.cmd.Process.Kill()
		}
	}

	// Close the PTY master.
	if session.masterFd >= 0 {
		_ = unix.Close(session.masterFd)
	}

	return nil
}

// Detach disconnects the caller from a session without terminating the
// underlying process. The session remains tracked and can be reattached.
func (sm *SessionManager) Detach(id string) error {
	sm.mu.Lock()
	session, exists := sm.sessions[id]
	if !exists {
		sm.mu.Unlock()
		return fmt.Errorf("session %s not found", id)
	}
	sm.mu.Unlock()

	// Close the master fd so the caller can no longer read/write, but keep
	// the process running.
	if session.masterFd >= 0 {
		_ = unix.Close(session.masterFd)
		session.masterFd = -1
	}

	return nil
}

// Remove removes a completed session from tracking.
func (sm *SessionManager) Remove(id string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if _, exists := sm.sessions[id]; !exists {
		return fmt.Errorf("session %s not found", id)
	}
	delete(sm.sessions, id)
	return nil
}

// pumpOutput reads from the PTY master and sends data to outputCh.
func (sm *SessionManager) pumpOutput(id string, session *procSession) {
	defer close(session.outputCh)

	buf := make([]byte, 4096)
	for {
		n, err := unix.Read(session.masterFd, buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			select {
			case session.outputCh <- data:
			case <-session.doneCh:
				return
			}
		}
		if err != nil {
			if err != io.EOF {
				sm.mu.Lock()
				session.info.Err = err
				sm.mu.Unlock()
			}
			return
		}
	}
}

// pumpInput writes data from inputCh to the PTY master.
func (sm *SessionManager) pumpInput(id string, session *procSession) {
	for {
		select {
		case data := <-session.inputCh:
			if _, err := unix.Write(session.masterFd, data); err != nil {
				sm.mu.Lock()
				session.info.Err = err
				sm.mu.Unlock()
				return
			}
		case <-session.doneCh:
			return
		}
	}
}

// waitSession waits for the process to exit and updates session state.
func (sm *SessionManager) waitSession(id string, session *procSession) {
	defer close(session.doneCh)

	err := session.cmd.Wait()

	sm.mu.Lock()
	session.info.Done = true
	session.info.EndTime = time.Now()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			session.info.ExitCode = exitErr.ExitCode()
		} else {
			session.info.ExitCode = -1
			session.info.Err = err
		}
	} else {
		session.info.ExitCode = 0
	}
	sm.mu.Unlock()
}

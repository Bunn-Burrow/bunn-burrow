package vmm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
)

// Process manages a Firecracker VMM process and reports its exit. It is safe
// for concurrent use.
type Process struct {
	cmd     *exec.Cmd
	mu      sync.Mutex
	exitCh  chan struct{}
	exitErr error
	reaped  bool
}

// Start launches the Firecracker binary with the given args and environment.
// A goroutine is spawned to reap the process and observe unexpected exits via
// Exited/ExitError. It is safe for concurrent use.
// If a consolePath is provided, the process's stdout (which carries the guest
// serial console output) is appended to that file. The caller owns the returned
// Process and should call Kill or wait for Exited to release resources.
func Start(binary string, args []string, env []string, consolePath ...string) (*Process, error) {
	cmd := exec.Command(binary, args...)
	if len(env) > 0 {
		cmd.Env = env
	}
	var consoleFile *os.File
	if len(consolePath) > 0 && consolePath[0] != "" {
		var err error
		consoleFile, err = os.OpenFile(consolePath[0], os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return nil, fmt.Errorf("open console log %q: %w", consolePath[0], err)
		}
		cmd.Stdout = consoleFile
	}
	if err := cmd.Start(); err != nil {
		if consoleFile != nil {
			_ = consoleFile.Close()
		}
		return nil, err
	}
	// The child inherited a duplicated fd, so the parent's copy can be closed.
	if consoleFile != nil {
		_ = consoleFile.Close()
	}

	p := &Process{cmd: cmd, exitCh: make(chan struct{})}
	go p.wait()
	return p, nil
}

// wait reaps the Firecracker process, recording its exit error and closing
// Exited. It is safe for concurrent use.
func (p *Process) wait() {
	err := p.cmd.Wait()

	p.mu.Lock()
	p.exitErr = err
	p.reaped = true
	p.mu.Unlock()
	close(p.exitCh)
}

// PID returns the process ID, or 0 if the process has not been started.
func (p *Process) PID() int {
	if p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// Exited returns a channel closed once the process has terminated.
func (p *Process) Exited() <-chan struct{} {
	return p.exitCh
}

// ExitError returns the error from cmd.Wait, if any. A nil error means the
// process exited with status 0. It is only valid after Exited is closed.
func (p *Process) ExitError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitErr
}

// Signal sends a signal to the process if it is still running.
func (p *Process) Signal(sig os.Signal) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reaped || p.cmd.Process == nil {
		return errors.New("process is not running")
	}
	return p.cmd.Process.Signal(sig)
}

// Kill terminates the process immediately.
func (p *Process) Kill() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reaped || p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}

// Reaped reports whether the process has been reaped (Wait completed).
func (p *Process) Reaped() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reaped
}

var _ = context.Background // context is used by callers via Wait helpers

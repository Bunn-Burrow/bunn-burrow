package guestagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ProcessInfo describes a managed process.
type ProcessInfo struct {
	ID        string
	PID       int
	Cmd       string
	Args      []string
	Env       []string
	Dir       string
	User      string
	Timeout   time.Duration
	StartTime time.Time
	EndTime   time.Time
	ExitCode  int
	Err       error
	Done      bool
}

// ProcessManager tracks and manages concurrent processes.
type ProcessManager struct {
	mu     sync.Mutex
	procs  map[string]*procEntry
	nextID int
}

// procEntry holds internal process state.
type procEntry struct {
	info   ProcessInfo
	cmd    *exec.Cmd
	cancel context.CancelFunc
	doneCh chan struct{}
	stdout io.ReadCloser
	stderr io.ReadCloser
}

// NewProcessManager creates a ProcessManager.
func NewProcessManager() *ProcessManager {
	return &ProcessManager{
		procs: make(map[string]*procEntry),
	}
}

// Start launches a new process and returns its execution ID.
// The caller receives output via the returned stdout/stderr readers.
// If stdin is non-nil, it is wired to the process's standard input.
func (pm *ProcessManager) Start(id, cmd string, args []string, env []string, dir string, user string, timeout time.Duration, stdin io.Reader) (*ProcessInfo, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if _, exists := pm.procs[id]; exists {
		return nil, fmt.Errorf("execution %s already exists", id)
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
	if user != "" {
		command.SysProcAttr = &syscall.SysProcAttr{}
		_ = user // user switching requires additional privilege handling (no-op for now)
	}

	if stdin != nil {
		command.Stdin = stdin
	}

	stdout, err := command.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := command.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start process: %w", err)
	}

	entry := &procEntry{
		info: ProcessInfo{
			ID:        id,
			PID:       command.Process.Pid,
			Cmd:       cmd,
			Args:      args,
			Env:       env,
			Dir:       dir,
			User:      user,
			Timeout:   timeout,
			StartTime: time.Now(),
		},
		cmd:    command,
		cancel: cancel,
		doneCh: make(chan struct{}),
		stdout: stdout,
		stderr: stderr,
	}

	pm.procs[id] = entry

	go pm.waitProcess(id, entry)

	return &entry.info, nil
}

// waitProcess waits for the process to finish and updates its state.
func (pm *ProcessManager) waitProcess(id string, entry *procEntry) {
	defer close(entry.doneCh)

	err := entry.cmd.Wait()

	pm.mu.Lock()
	entry.info.Done = true
	entry.info.EndTime = time.Now()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			entry.info.ExitCode = exitErr.ExitCode()
		} else {
			entry.info.ExitCode = -1
			entry.info.Err = err
		}
	} else {
		entry.info.ExitCode = 0
	}
	pm.mu.Unlock()
}

// Cancel sends a termination signal to the process.
func (pm *ProcessManager) Cancel(id string) error {
	pm.mu.Lock()
	entry, exists := pm.procs[id]
	if !exists {
		pm.mu.Unlock()
		return fmt.Errorf("execution %s not found", id)
	}
	pm.mu.Unlock()

	entry.cancel()

	// Give the process a moment to terminate, then kill if needed.
	select {
	case <-entry.doneCh:
		return nil
	case <-time.After(5 * time.Second):
	}

	pm.mu.Lock()
	if entry.cmd.Process != nil {
		_ = entry.cmd.Process.Kill()
	}
	pm.mu.Unlock()

	return nil
}

// Status returns the current state of a process.
func (pm *ProcessManager) Status(id string) (*ProcessInfo, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	entry, exists := pm.procs[id]
	if !exists {
		return nil, fmt.Errorf("execution %s not found", id)
	}

	info := entry.info
	return &info, nil
}

// Remove removes a completed process from tracking.
func (pm *ProcessManager) Remove(id string) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if _, exists := pm.procs[id]; !exists {
		return fmt.Errorf("execution %s not found", id)
	}
	delete(pm.procs, id)
	return nil
}

// StdoutReader returns a reader for the process's stdout.
func (pm *ProcessManager) StdoutReader(id string) (io.ReadCloser, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	entry, exists := pm.procs[id]
	if !exists {
		return nil, fmt.Errorf("execution %s not found", id)
	}
	return entry.stdout, nil
}

// StderrReader returns a reader for the process's stderr.
func (pm *ProcessManager) StderrReader(id string) (io.ReadCloser, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	entry, exists := pm.procs[id]
	if !exists {
		return nil, fmt.Errorf("execution %s not found", id)
	}
	return entry.stderr, nil
}

// EnvToSlice converts a map of environment variables to a slice of "KEY=VAL" strings.
func EnvToSlice(env map[string]any) []string {
	var result []string
	for k, v := range env {
		result = append(result, fmt.Sprintf("%s=%v", k, v))
	}
	return result
}

// ArgsToSlice converts a slice of any to a slice of strings.
func ArgsToSlice(args []any) []string {
	result := make([]string, len(args))
	for i, a := range args {
		result[i] = fmt.Sprintf("%v", a)
	}
	return result
}

// MergeEnv merges image environment with execution environment.
// Execution variables override image defaults.
func MergeEnv(imageEnv []string, execEnv map[string]any) []string {
	merged := make(map[string]string)
	for _, e := range imageEnv {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			merged[parts[0]] = parts[1]
		}
	}
	for k, v := range execEnv {
		merged[k] = fmt.Sprintf("%v", v)
	}
	var result []string
	for k, v := range merged {
		result = append(result, k+"="+v)
	}
	return result
}

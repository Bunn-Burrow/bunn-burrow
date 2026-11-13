package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/omkar/sandbox/vmm"
)

// Manager orchestrates agent executions across Firecracker microVMs.
// It manages the complete agent lifecycle, workspace isolation, secrets redaction,
// event streaming, timeout enforcement, and failure cleanup.
type Manager struct {
	mu          sync.RWMutex
	executions  map[string]*Execution
	results     map[string]*ExecutionResult
	sandboxes   map[string]VMSandbox
	cancels     map[string]context.CancelFunc
	secrets     map[string]map[string]string // execID -> raw secrets (in-memory only)
	subscribers map[string][]chan Event
	provider    VMProvider
	wsLockMgr   *WorkspaceLockManager
}

// NewManager creates an agent Execution Manager with the given VMProvider.
func NewManager(provider VMProvider) *Manager {
	return &Manager{
		executions:  make(map[string]*Execution),
		results:     make(map[string]*ExecutionResult),
		sandboxes:   make(map[string]VMSandbox),
		cancels:     make(map[string]context.CancelFunc),
		secrets:     make(map[string]map[string]string),
		subscribers: make(map[string][]chan Event),
		provider:    provider,
		wsLockMgr:   NewWorkspaceLockManager(),
	}
}

// Create validates configuration and registers a new agent execution in the Created state.
// Plaintext secrets are isolated in memory and excluded from stored execution metadata.
func (m *Manager) Create(ctx context.Context, cfg AgentConfig) (*Execution, error) {
	if cfg.ID == "" {
		cfg.ID = "agent-" + uuid.NewString()[:8]
	}

	if err := PrepareConfig(&cfg); err != nil {
		return nil, fmt.Errorf("prepare agent config: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.executions[cfg.ID]; exists {
		return nil, fmt.Errorf("agent execution %q already exists", cfg.ID)
	}

	// Sanitize environment variables so secrets are never stored in metadata
	sanitizedEnv := SanitizeEnv(cfg.Env, cfg.Secrets)

	exec := &Execution{
		ID:          cfg.ID,
		Framework:   cfg.Framework,
		VMID:        cfg.VMID,
		ImageRef:    cfg.ImageRef,
		TemplateRef: cfg.TemplateRef,
		Command:     cfg.Command,
		Args:        cfg.Args,
		Env:         sanitizedEnv,
		Resources:   cfg.Resources,
		Timeout:     cfg.Timeout,
		Workspace:   cfg.Workspace,
		Network:     cfg.Network,
		Interactive: cfg.Interactive,
		State:       StateCreated,
		CreatedAt:   time.Now().UTC(),
	}

	m.executions[cfg.ID] = exec

	// Store raw secrets in memory isolated from the public Execution model
	if len(cfg.Secrets) > 0 {
		secCopy := make(map[string]string, len(cfg.Secrets))
		for k, v := range cfg.Secrets {
			secCopy[k] = v
		}
		m.secrets[cfg.ID] = secCopy
	}

	m.emitEventLocked(cfg.ID, Event{
		ExecutionID: cfg.ID,
		Type:        EventStateChange,
		State:       StateCreated,
		Timestamp:   exec.CreatedAt,
	})

	snap := exec.Snapshot()
	return &snap, nil
}

// Start initiates the agent execution asynchronously.
// It provisions the VM, prepares the workspace, launches the agent command,
// and manages background execution monitoring.
func (m *Manager) Start(ctx context.Context, id string) error {
	m.mu.Lock()
	exec, ok := m.executions[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("agent execution %q not found", id)
	}

	if err := ValidateTransition(exec.State, StateStarting); err != nil {
		m.mu.Unlock()
		return err
	}

	exec.State = StateStarting
	now := time.Now().UTC()
	exec.StartedAt = &now

	// Extract raw secrets and config snapshot
	rawSecrets := m.secrets[id]
	cfgSnapshot := AgentConfig{
		ID:          exec.ID,
		Framework:   exec.Framework,
		VMID:        exec.VMID,
		ImageRef:    exec.ImageRef,
		TemplateRef: exec.TemplateRef,
		Command:     exec.Command,
		Args:        exec.Args,
		Env:         exec.Env,
		Secrets:     rawSecrets,
		Resources:   exec.Resources,
		Timeout:     exec.Timeout,
		Workspace:   exec.Workspace,
		Network:     exec.Network,
		Interactive: exec.Interactive,
	}

	// Execution context with cancellation & timeout
	execCtx, cancel := context.WithCancel(context.Background())
	if cfgSnapshot.Timeout > 0 {
		execCtx, cancel = context.WithTimeout(execCtx, cfgSnapshot.Timeout)
	}
	m.cancels[id] = cancel

	m.emitEventLocked(id, Event{
		ExecutionID: id,
		Type:        EventStateChange,
		State:       StateStarting,
		Timestamp:   now,
	})
	m.mu.Unlock()

	// Launch async execution loop
	go m.runExecution(execCtx, &cfgSnapshot, rawSecrets)

	return nil
}

// runExecution handles VM creation, workspace upload, workload execution,
// output capturing, and state transitions.
func (m *Manager) runExecution(ctx context.Context, cfg *AgentConfig, rawSecrets map[string]string) {
	id := cfg.ID
	startTime := time.Now()

	// 1. Acquire workspace lock if workspace is configured
	var unlockWS func()
	if cfg.Workspace != nil && cfg.Workspace.ID != "" {
		var err error
		unlockWS, err = m.wsLockMgr.Acquire(ctx, cfg.Workspace.ID)
		if err != nil {
			m.failExecution(id, fmt.Errorf("acquire workspace: %w", err), startTime)
			return
		}
		defer unlockWS()
	}

	// 2. Provision sandbox VM
	sb, err := m.provider.CreateSandbox(ctx, cfg)
	if err != nil {
		m.failExecution(id, fmt.Errorf("create VM sandbox: %w", err), startTime)
		return
	}

	m.mu.Lock()
	m.sandboxes[id] = sb
	exec := m.executions[id]
	if exec != nil {
		exec.VMID = sb.ID()
	}
	m.mu.Unlock()

	closeAttempted := false
	closeSandbox := func() error {
		if closeAttempted {
			return nil
		}
		closeAttempted = true
		return sb.Close(context.Background())
	}
	defer func() {
		if !closeAttempted {
			_ = closeSandbox()
		}
	}()

	// 3. Boot sandbox VM
	if err := sb.Start(ctx); err != nil {
		m.failExecution(id, withSandboxCloseError(fmt.Errorf("boot VM sandbox: %w", err), closeSandbox()), startTime)
		return
	}

	// 4. Upload workspace if files are provided
	if cfg.Workspace != nil && len(cfg.Workspace.Files) > 0 {
		proj, err := BuildProject(cfg.Workspace, id)
		if err != nil {
			m.failExecution(id, withSandboxCloseError(fmt.Errorf("build project files: %w", err), closeSandbox()), startTime)
			return
		}
		if err := sb.UploadProject(ctx, proj); err != nil {
			m.failExecution(id, withSandboxCloseError(fmt.Errorf("upload project files to guest: %w", err), closeSandbox()), startTime)
			return
		}
	}

	// 5. Prepare execution environment (merging env + secrets)
	execEnv, err := PrepareExecutionEnv(cfg.Env, rawSecrets)
	if err != nil {
		m.failExecution(id, withSandboxCloseError(fmt.Errorf("prepare environment: %w", err), closeSandbox()), startTime)
		return
	}

	workingDir := "/workspace"
	if cfg.Workspace != nil && cfg.Workspace.WorkingDir != "" {
		workingDir = cfg.Workspace.WorkingDir
	}

	workload := vmm.Workload{
		ID:      id,
		Cmd:     cfg.Command,
		Args:    cfg.Args,
		Env:     execEnv,
		Dir:     workingDir,
		Timeout: cfg.Timeout,
	}

	// Transition to Running state
	m.mu.Lock()
	if exec := m.executions[id]; exec != nil {
		if err := ValidateTransition(exec.State, StateRunning); err == nil {
			exec.State = StateRunning
			m.emitEventLocked(id, Event{
				ExecutionID: id,
				Type:        EventStateChange,
				State:       StateRunning,
				Timestamp:   time.Now().UTC(),
			})
		}
	}
	m.mu.Unlock()

	// 6. Execute workload and capture streaming output
	stdoutPipeReader, stdoutPipeWriter := io.Pipe()
	stderrPipeReader, stderrPipeWriter := io.Pipe()

	var stdoutBuf, stderrBuf bytes.Buffer
	var streamWg sync.WaitGroup
	streamWg.Add(2)

	// Stream stdout
	go func() {
		defer streamWg.Done()
		buf := make([]byte, 1024)
		for {
			n, err := stdoutPipeReader.Read(buf)
			if n > 0 {
				data := buf[:n]
				stdoutBuf.Write(data)
				scrubbed := RedactText(string(data), rawSecrets)
				m.emitEvent(id, Event{
					ExecutionID: id,
					Type:        EventStdout,
					Data:        scrubbed,
					Timestamp:   time.Now().UTC(),
				})
			}
			if err != nil {
				break
			}
		}
	}()

	// Stream stderr
	go func() {
		defer streamWg.Done()
		buf := make([]byte, 1024)
		for {
			n, err := stderrPipeReader.Read(buf)
			if n > 0 {
				data := buf[:n]
				stderrBuf.Write(data)
				scrubbed := RedactText(string(data), rawSecrets)
				m.emitEvent(id, Event{
					ExecutionID: id,
					Type:        EventStderr,
					Data:        scrubbed,
					Timestamp:   time.Now().UTC(),
				})
			}
			if err != nil {
				break
			}
		}
	}()

	exitCode, execErr := sb.Execute(ctx, workload, stdoutPipeWriter, stderrPipeWriter)
	_ = stdoutPipeWriter.Close()
	_ = stderrPipeWriter.Close()
	streamWg.Wait()
	closeErr := closeSandbox()

	// Clean up secrets from memory
	m.mu.Lock()
	delete(m.secrets, id)
	delete(m.cancels, id)
	m.mu.Unlock()

	duration := time.Since(startTime)
	cleanedStdout := RedactText(stdoutBuf.String(), rawSecrets)
	cleanedStderr := RedactText(stderrBuf.String(), rawSecrets)

	// 7. Evaluate completion state
	m.mu.Lock()
	defer m.mu.Unlock()

	exec = m.executions[id]
	if exec == nil {
		return
	}

	completedAt := time.Now().UTC()
	exec.CompletedAt = &completedAt
	exec.ExitCode = &exitCode

	var finalState LifecycleState
	var finalErr string

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		finalState = StateTimedOut
		finalErr = "execution timed out"
	} else if errors.Is(ctx.Err(), context.Canceled) || exec.State == StateCancelled {
		finalState = StateCancelled
		finalErr = "execution cancelled"
	} else if execErr != nil {
		finalState = StateFailed
		finalErr = execErr.Error()
	} else if exitCode != 0 {
		finalState = StateFailed
		finalErr = fmt.Sprintf("process exited with status %d", exitCode)
	} else {
		finalState = StateSucceeded
	}
	if closeErr != nil {
		if finalErr != "" {
			finalErr += "; "
		}
		finalErr += fmt.Sprintf("close VM sandbox: %v", closeErr)
		if finalState == StateSucceeded {
			finalState = StateFailed
		}
	}

	exec.State = finalState
	exec.Error = finalErr

	res := &ExecutionResult{
		ExecutionID: id,
		Framework:   exec.Framework,
		State:       finalState,
		ExitCode:    exitCode,
		Error:       finalErr,
		Stdout:      cleanedStdout,
		Stderr:      cleanedStderr,
		Duration:    duration,
	}
	m.results[id] = res

	m.emitEventLocked(id, Event{
		ExecutionID: id,
		Type:        EventFinished,
		State:       finalState,
		Data:        finalErr,
		Timestamp:   completedAt,
	})
}

func withSandboxCloseError(err, closeErr error) error {
	if closeErr == nil {
		return err
	}
	return errors.Join(err, fmt.Errorf("close VM sandbox: %w", closeErr))
}

// failExecution marks an execution as failed and records the error.
func (m *Manager) failExecution(id string, err error, startTime time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.secrets, id)
	delete(m.cancels, id)

	exec := m.executions[id]
	if exec == nil {
		return
	}

	now := time.Now().UTC()
	exec.State = StateFailed
	exec.CompletedAt = &now
	exec.Error = err.Error()

	m.results[id] = &ExecutionResult{
		ExecutionID: id,
		Framework:   exec.Framework,
		State:       StateFailed,
		ExitCode:    -1,
		Error:       err.Error(),
		Duration:    time.Since(startTime),
	}

	m.emitEventLocked(id, Event{
		ExecutionID: id,
		Type:        EventError,
		State:       StateFailed,
		Data:        err.Error(),
		Timestamp:   now,
	})
}

// Inspect returns a thread-safe snapshot of an agent execution.
func (m *Manager) Inspect(id string) (*Execution, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	exec, ok := m.executions[id]
	if !ok {
		return nil, fmt.Errorf("agent execution %q not found", id)
	}

	snap := exec.Snapshot()
	return &snap, nil
}

// List returns a list of snapshots of all registered agent executions.
func (m *Manager) List() []*Execution {
	m.mu.RLock()
	defer m.mu.RUnlock()

	list := make([]*Execution, 0, len(m.executions))
	for _, exec := range m.executions {
		snap := exec.Snapshot()
		list = append(list, &snap)
	}
	return list
}

// Cancel requests graceful cancellation of an active agent execution.
func (m *Manager) Cancel(ctx context.Context, id string) error {
	m.mu.Lock()
	exec, ok := m.executions[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("agent execution %q not found", id)
	}

	if exec.State.IsTerminal() {
		m.mu.Unlock()
		return fmt.Errorf("cannot cancel execution in terminal state %q", exec.State)
	}

	// Trigger context cancellation
	if cancel, ok := m.cancels[id]; ok {
		cancel()
	}

	sb := m.sandboxes[id]
	m.mu.Unlock()

	// Signal the guest agent vsock server if VM is running
	if sb != nil {
		_ = sb.CancelExecution(ctx, id)
	}

	return nil
}

// Terminate forcefully aborts an agent execution and destroys its VM sandbox.
func (m *Manager) Terminate(ctx context.Context, id string) error {
	if err := m.Cancel(ctx, id); err != nil {
		// If already terminal, nothing more to do
		if errors.Is(err, context.Canceled) {
			return nil
		}
	}

	m.mu.Lock()
	sb := m.sandboxes[id]
	m.mu.Unlock()

	if sb != nil {
		return sb.Close(ctx)
	}
	return nil
}

// GetResult retrieves the completed ExecutionResult.
func (m *Manager) GetResult(id string) (*ExecutionResult, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	res, ok := m.results[id]
	if !ok {
		exec, ok := m.executions[id]
		if !ok {
			return nil, fmt.Errorf("agent execution %q not found", id)
		}
		if !exec.State.IsTerminal() {
			return nil, fmt.Errorf("agent execution %q is still in state %q", id, exec.State)
		}
		return nil, fmt.Errorf("result for agent execution %q unavailable", id)
	}

	return res, nil
}

// Wait blocks until the agent execution reaches a terminal state, or ctx is cancelled.
func (m *Manager) Wait(ctx context.Context, id string) (*ExecutionResult, error) {
	ch, unsubscribe, err := m.Subscribe(id)
	if err != nil {
		return nil, err
	}
	defer unsubscribe()

	// Check if already finished
	if res, err := m.GetResult(id); err == nil {
		return res, nil
	}

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				return m.GetResult(id)
			}
			if ev.Type == EventFinished || ev.State.IsTerminal() {
				return m.GetResult(id)
			}
		}
	}
}

// Subscribe returns an Event channel that streams execution events.
// The caller must call the returned cleanup function when done.
func (m *Manager) Subscribe(id string) (<-chan Event, func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.executions[id]; !ok {
		return nil, nil, fmt.Errorf("agent execution %q not found", id)
	}

	ch := make(chan Event, 64)
	m.subscribers[id] = append(m.subscribers[id], ch)

	cleanup := func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		subs := m.subscribers[id]
		for i, c := range subs {
			if c == ch {
				m.subscribers[id] = append(subs[:i], subs[i+1:]...)
				close(ch)
				break
			}
		}
	}

	return ch, cleanup, nil
}

func (m *Manager) emitEvent(id string, ev Event) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	m.emitEventLocked(id, ev)
}

func (m *Manager) emitEventLocked(id string, ev Event) {
	for _, ch := range m.subscribers[id] {
		select {
		case ch <- ev:
		default:
			// Non-blocking drop if subscriber is too slow
		}
	}
}

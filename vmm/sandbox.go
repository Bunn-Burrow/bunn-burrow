package vmm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// Workload describes a generic command to execute inside an isolated guest VM.
// It is intentionally language-agnostic: any guest that runs a compatible
// vsock exec server can host it.
type Workload struct {
	// Cmd is the executable to run inside the guest.
	Cmd string

	// Args are the command-line arguments passed to Cmd.
	Args []string

	// Env are the environment variables set for the command.
	Env []string

	// Dir is the working directory inside the guest.
	Dir string

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
// server that speaks the newline-delimited JSON protocol documented below for
// Execute to succeed.
//
// Protocol (newline-delimited JSON, host -> guest):
//
//	{"kind":"exec","cmd":"/bin/sh","args":["-c","echo hi"],"env":[],"cwd":"/","timeout_ms":30000}
//
// Guest -> host:
//
//	{"kind":"stdout","data":"hello\n"}
//	{"kind":"stderr","data":"..."}
//	{"kind":"exit","code":0}
//	{"kind":"error","message":"..."}
type Sandbox struct {
	mu   sync.Mutex
	vm   *VM
	cfg  Config
	done bool // Close has been called
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

	exitCode, err := runExec(execCtx, conn, w, stdout, stderr)
	if err != nil {
		return exitCode, err
	}
	return exitCode, nil
}

// runExec sends the exec request over conn and streams guest responses until
// an exit or error message is received, or ctx is cancelled.
func runExec(ctx context.Context, conn io.ReadWriteCloser, w Workload, stdout, stderr io.Writer) (int, error) {
	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(conn)

	if err := enc.Encode(map[string]any{
		"kind":       "exec",
		"cmd":        w.Cmd,
		"args":       w.Args,
		"env":        w.Env,
		"cwd":        w.Dir,
		"timeout_ms": w.Timeout.Milliseconds(),
	}); err != nil {
		return -1, fmt.Errorf("sandbox send workload: %w", err)
	}

	// Decode guest messages on a single goroutine so context cancellation can
	// interrupt a blocking read without concurrent decoder access.
	type decodeResult struct {
		msg map[string]any
		err error
	}
	msgCh := make(chan decodeResult, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			var msg map[string]any
			if err := dec.Decode(&msg); err != nil {
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
				return exitCode, nil
			}
			if res.err != nil {
				if errors.Is(res.err, io.EOF) {
					return exitCode, nil
				}
				return exitCode, fmt.Errorf("sandbox read guest message: %w", res.err)
			}

			switch kind, _ := res.msg["kind"].(string); kind {
			case "stdout":
				if data, _ := res.msg["data"].(string); data != "" {
					if _, werr := stdout.Write([]byte(data)); werr != nil {
						return exitCode, werr
					}
				}
			case "stderr":
				if data, _ := res.msg["data"].(string); data != "" {
					if _, werr := stderr.Write([]byte(data)); werr != nil {
						return exitCode, werr
					}
				}
			case "exit":
				exitCode = -1
				if code, ok := res.msg["code"].(float64); ok {
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

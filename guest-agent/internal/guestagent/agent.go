package guestagent

import (
	"context"
	"fmt"
	"os"
	"sync"
	"syscall"
)

type State string

const (
	StateStarting State = "starting"
	StateReady    State = "ready"
	StateStopping State = "stopping"
	StateStopped  State = "stopped"
)

type Agent struct {
	mu         sync.RWMutex
	state      State
	procMgr    *ProcessManager
	shutdownFn func()
}

func New() *Agent {
	return &Agent{state: StateStarting}
}

func (a *Agent) State() State {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.state
}

func (a *Agent) transition(from, to State) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.state != from {
		return fmt.Errorf(
			"invalid lifecycle transition: %s -> %s",
			a.state,
			to,
		)
	}

	a.state = to
	return nil
}

func (a *Agent) ValidatePID1() error {
	if os.Getpid() != 1 {
		return fmt.Errorf("expected PID 1, got PID %d", os.Getpid())
	}
	return nil
}

// SetProcessManager attaches a ProcessManager to the agent.
func (a *Agent) SetProcessManager(pm *ProcessManager) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.procMgr = pm
}

// SetShutdownFn sets the shutdown callback.
func (a *Agent) SetShutdownFn(fn func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.shutdownFn = fn
}

// ProcessManager returns the attached ProcessManager.
func (a *Agent) ProcessManager() *ProcessManager {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.procMgr
}

// Run runs the agent's main event loop.
// ctx is cancelled when a vsock shutdown request is received.
func (a *Agent) Run(
	ctx context.Context,
	signals <-chan os.Signal,
	reap func() error,
) error {
	if err := a.transition(StateStarting, StateReady); err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return a.shutdown()

		case sig, ok := <-signals:
			if !ok {
				return fmt.Errorf("signal channel unexpectedly closed")
			}

			switch sig {
			case syscall.SIGCHLD:
				if err := reap(); err != nil {
					return fmt.Errorf("reap children: %w", err)
				}

			case syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT:
				return a.shutdown()
			}
		}
	}
}

func (a *Agent) shutdown() error {
	if err := a.transition(StateReady, StateStopping); err != nil {
		return err
	}

	a.mu.Lock()
	if a.shutdownFn != nil {
		a.shutdownFn()
	}
	a.mu.Unlock()

	return a.transition(StateStopping, StateStopped)
}

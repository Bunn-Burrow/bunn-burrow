package vmm

import (
	"fmt"
	"sync"
)

// State is the host-side lifecycle state of a VM. It is distinct from
// Firecracker's own instance state ("Not started", "Running", "Paused"),
// which we query separately via the API. The host state tracks the lifecycle
// operations this implementation performs and owns.
//
// Allowed transitions:
//   Created   -> Preparing   (firecracker process launched, API socket awaited)
//   Preparing -> Running     (devices configured, instance started)
//   Running   -> Paused      (pause requested)
//   Paused    -> Running     (resume requested)
//   Paused    -> Snapshotting(snapshot requested)
//   Snapshotting -> Paused   (snapshot complete)
//   Running   -> Stopping    (stop requested)
//   Stopping  -> Stopped     (process termination confirmed)
//   Stopped   -> Destroying  (destroy requested)
//   Destroying -> Destroyed  (resources cleaned)
//   any       -> Failed      (terminal error)
//
// Stop and Destroy are idempotent: Stopping/Stopped/Destroyed are stable
// under repeated stop/destroy calls.
type State string

const (
	StateCreated      State = "Created"
	StatePreparing    State = "Preparing"
	StateRunning      State = "Running"
	StatePaused       State = "Paused"
	StateSnapshotting State = "Snapshotting"
	StateStopping     State = "Stopping"
	StateStopped      State = "Stopped"
	StateDestroying   State = "Destroying"
	StateDestroyed    State = "Destroyed"
	StateFailed       State = "Failed"
)

// transitions maps each state to the set of states it may transition to.
var transitions = map[State]map[State]struct{}{
	StateCreated:      {StatePreparing: {}, StateFailed: {}},
	StatePreparing:    {StateRunning: {}, StateFailed: {}},
	StateRunning:      {StatePaused: {}, StateStopping: {}, StateFailed: {}},
	StatePaused:       {StateRunning: {}, StateSnapshotting: {}, StateStopping: {}, StateFailed: {}},
	StateSnapshotting: {StatePaused: {}, StateFailed: {}},
	StateStopping:     {StateStopped: {}, StateFailed: {}},
	StateStopped:      {StateDestroying: {}, StateFailed: {}},
	StateDestroying:   {StateDestroyed: {}, StateFailed: {}},
	StateDestroyed:    {},
	StateFailed:       {},
}

// idempotentStop are states in which a stop/destroy is a no-op (already
// terminal with respect to stopping).
var idempotentStop = map[State]struct{}{
	StateStopped:   {},
	StateDestroyed: {},
	StateFailed:    {},
}

// StateMachine validates and performs state transitions. It is safe for
// concurrent use.
type StateMachine struct {
	mu   sync.Mutex
	cond *sync.Cond
	state State
}

// NewStateMachine creates a StateMachine in the Created state.
func NewStateMachine() *StateMachine {
	s := &StateMachine{state: StateCreated}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// State returns the current state.
func (s *StateMachine) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Waiting returns a channel that is closed when the state changes. Callers
// should re-read State() after the channel closes. Passing a nil channel
// disables waiting.
func (s *StateMachine) Waiting() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan struct{})
	go func() {
		s.cond.L.Lock()
		s.cond.Wait()
		s.cond.L.Unlock()
		close(ch)
	}()
	return ch
}

// Transition attempts to move from one of the given current states to the
// target state. It returns ErrInvalidTransition if the current state does not
// permit the transition. On success the state is updated and waiters are
// woken.
func (s *StateMachine) Transition(from State, to State) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state != from {
		return &TransitionError{
			Current: s.state,
			From:    from,
			To:      to,
			Err:     fmt.Errorf("state is %q, expected %q", s.state, from),
		}
	}

	allowed, ok := transitions[s.state]
	if !ok {
		return &TransitionError{
			Current: s.state,
			From:    from,
			To:      to,
			Err:     fmt.Errorf("no transitions defined from %q", s.state),
		}
	}
	if _, ok := allowed[to]; !ok {
		return &TransitionError{
			Current: s.state,
			From:    from,
			To:      to,
			Err:     fmt.Errorf("transition %q -> %q not allowed", s.state, to),
		}
	}

	s.state = to
	s.cond.Broadcast()
	return nil
}

// TransitionTo moves to target regardless of current state, used only for
// terminal failure and destroy completion where the caller has already
// validated the operation externally.
func (s *StateMachine) TransitionTo(to State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = to
	s.cond.Broadcast()
}

// IsTerminal reports whether the state is terminal (no further lifecycle
// operations are meaningful).
func (s *StateMachine) IsTerminal(state State) bool {
	switch state {
	case StateDestroyed, StateFailed:
		return true
	default:
		return false
	}
}

// TransitionError describes a rejected state transition.
type TransitionError struct {
	Current State
	From    State
	To      State
	Err     error
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("vmm: invalid transition: %v", e.Err)
}

func (e *TransitionError) Unwrap() error { return e.Err }

var _ error = (*TransitionError)(nil)

package agent

import (
	"errors"
	"fmt"
	"time"
)

// Framework represents the agent framework or runtime type.
type Framework string

const (
	FrameworkCustom     Framework = "custom"
	FrameworkPython     Framework = "python"
	FrameworkGo         Framework = "go"
	FrameworkNode       Framework = "node"
	FrameworkLangGraph  Framework = "langgraph"
	FrameworkCrewAI     Framework = "crewai"
	FrameworkAutoGen    Framework = "autogen"
	FrameworkAgno       Framework = "agno"
	FrameworkClaudeCode Framework = "claude-code"
	FrameworkCodex      Framework = "codex"
	FrameworkOpenCode   Framework = "opencode"
)

// Valid returns whether the framework is recognized.
func (f Framework) Valid() bool {
	switch f {
	case FrameworkCustom, FrameworkPython, FrameworkGo, FrameworkNode,
		FrameworkLangGraph, FrameworkCrewAI, FrameworkAutoGen, FrameworkAgno,
		FrameworkClaudeCode, FrameworkCodex, FrameworkOpenCode:
		return true
	default:
		return false
	}
}

// LifecycleState represents the current state of an agent execution.
type LifecycleState string

const (
	StateCreated   LifecycleState = "created"
	StateStarting  LifecycleState = "starting"
	StateRunning   LifecycleState = "running"
	StateSucceeded LifecycleState = "succeeded"
	StateFailed    LifecycleState = "failed"
	StateCancelled LifecycleState = "cancelled"
	StateTimedOut  LifecycleState = "timed_out"
)

// IsTerminal returns true if the state cannot transition to any subsequent state.
func (s LifecycleState) IsTerminal() bool {
	switch s {
	case StateSucceeded, StateFailed, StateCancelled, StateTimedOut:
		return true
	default:
		return false
	}
}

// ValidateTransition verifies whether transitioning from current to next is valid.
func ValidateTransition(current, next LifecycleState) error {
	if current == next {
		return nil
	}
	if current.IsTerminal() {
		return fmt.Errorf("cannot transition from terminal state %q to %q", current, next)
	}

	switch current {
	case StateCreated:
		if next == StateStarting || next == StateCancelled {
			return nil
		}
	case StateStarting:
		if next == StateRunning || next == StateFailed || next == StateCancelled {
			return nil
		}
	case StateRunning:
		if next == StateSucceeded || next == StateFailed || next == StateCancelled || next == StateTimedOut {
			return nil
		}
	}

	return fmt.Errorf("invalid state transition from %q to %q", current, next)
}

// ResourceConfig defines resource allocations for the agent VM.
type ResourceConfig struct {
	VCPU       int64 `json:"vcpu,omitempty"`
	MemSizeMiB int64 `json:"mem_size_mib,omitempty"`
}

// WorkspaceConfig configures the workspace filesystem for an agent execution.
type WorkspaceConfig struct {
	// ID uniquely identifies the workspace/project.
	ID string `json:"id,omitempty"`

	// GuestPath is the absolute directory path inside the guest VM where the
	// workspace is mounted or extracted. If empty, defaults to /tmp/bunn-workspace/<id>.
	GuestPath string `json:"guest_path,omitempty"`

	// WorkingDir is the directory in which the agent process runs inside the guest.
	WorkingDir string `json:"working_dir,omitempty"`

	// Files maps relative file paths to their contents.
	// Omitted from public JSON serialization to avoid bloat and leaking sensitive data.
	Files map[string][]byte `json:"-"`

	// VolumeID optionally references a persistent storage block volume (W4).
	// Note: Guest-level formatting and mounting requires guest-agent filesystem integration.
	VolumeID string `json:"volume_id,omitempty"`

	// ReadOnly marks the workspace as read-only.
	ReadOnly bool `json:"read_only,omitempty"`
}

// NetworkConfig configures VM networking for an agent execution.
type NetworkConfig struct {
	// Enabled toggles outbound network access for model APIs and external tools.
	Enabled bool `json:"enabled"`

	// Bridge is the Linux bridge name to connect the VM TAP interface to.
	Bridge string `json:"bridge,omitempty"`

	// CIDR is the IP subnet for VM allocations.
	CIDR string `json:"cidr,omitempty"`

	// NATInterface is the host interface used for outbound masquerade.
	NATInterface string `json:"nat_interface,omitempty"`
}

// AgentConfig contains the complete specification for an agent execution.
type AgentConfig struct {
	// ID is the unique identifier for this agent execution.
	ID string `json:"id"`

	// Framework identifies the agent framework (e.g. python, langgraph, crewai, claude-code).
	Framework Framework `json:"framework"`

	// VMID is an optional ID of an existing VM or assigned to the new VM.
	VMID string `json:"vm_id,omitempty"`

	// ImageRef optionally references a VM image (W5).
	ImageRef string `json:"image_ref,omitempty"`

	// TemplateRef optionally references a VM template (W5).
	TemplateRef string `json:"template_ref,omitempty"`

	// Command is the executable or binary to run inside the guest VM.
	Command string `json:"command"`

	// Args are the command-line arguments.
	Args []string `json:"args,omitempty"`

	// Env holds standard environment variables (key -> value).
	Env map[string]string `json:"env,omitempty"`

	// Secrets holds sensitive environment variables (API keys, auth tokens).
	// Secrets are securely injected at execution time and NEVER persisted in metadata or logs.
	Secrets map[string]string `json:"-"`

	// Resources configures CPU and memory limits.
	Resources ResourceConfig `json:"resources,omitempty"`

	// Timeout bounds how long the agent process may execute before being cancelled.
	Timeout time.Duration `json:"timeout,omitempty"`

	// Workspace configures the project filesystem and files available to the agent.
	Workspace *WorkspaceConfig `json:"workspace,omitempty"`

	// Network configures outbound networking for tool and LLM API access.
	Network *NetworkConfig `json:"network,omitempty"`

	// Interactive specifies if the process should run inside an interactive PTY session (W3).
	Interactive bool `json:"interactive,omitempty"`
}

// Validate checks the configuration for required fields and semantic correctness.
func (c *AgentConfig) Validate() error {
	if c.ID == "" {
		return errors.New("agent execution id is required")
	}
	if c.Framework == "" {
		c.Framework = FrameworkCustom
	}
	if !c.Framework.Valid() {
		return fmt.Errorf("unsupported agent framework %q", c.Framework)
	}
	if c.Command == "" {
		return errors.New("command is required")
	}
	if c.Timeout < 0 {
		return errors.New("timeout cannot be negative")
	}
	if c.Resources.VCPU < 0 {
		return errors.New("vcpu count cannot be negative")
	}
	if c.Resources.MemSizeMiB < 0 {
		return errors.New("memory size cannot be negative")
	}
	return nil
}

// Execution represents an instantiated agent execution and its observable state.
type Execution struct {
	ID          string            `json:"id"`
	Framework   Framework         `json:"framework"`
	VMID        string            `json:"vm_id"`
	ImageRef    string            `json:"image_ref,omitempty"`
	TemplateRef string            `json:"template_ref,omitempty"`
	Command     string            `json:"command"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"` // Secrets are sanitized/redacted!
	Resources   ResourceConfig    `json:"resources"`
	Timeout     time.Duration     `json:"timeout"`
	Workspace   *WorkspaceConfig  `json:"workspace,omitempty"`
	Network     *NetworkConfig    `json:"network,omitempty"`
	Interactive bool              `json:"interactive,omitempty"`
	State       LifecycleState    `json:"state"`
	CreatedAt   time.Time         `json:"created_at"`
	StartedAt   *time.Time        `json:"started_at,omitempty"`
	CompletedAt *time.Time        `json:"completed_at,omitempty"`
	ExitCode    *int              `json:"exit_code,omitempty"`
	Error       string            `json:"error,omitempty"`
}

// Snapshot returns a copy of the execution state for thread-safe inspection.
func (e *Execution) Snapshot() Execution {
	if e == nil {
		return Execution{}
	}
	cp := Execution{
		ID:          e.ID,
		Framework:   e.Framework,
		VMID:        e.VMID,
		ImageRef:    e.ImageRef,
		TemplateRef: e.TemplateRef,
		Command:     e.Command,
		Resources:   e.Resources,
		Timeout:     e.Timeout,
		Workspace:   e.Workspace,
		Network:     e.Network,
		Interactive: e.Interactive,
		State:       e.State,
		CreatedAt:   e.CreatedAt,
		StartedAt:   e.StartedAt,
		CompletedAt: e.CompletedAt,
		ExitCode:    e.ExitCode,
		Error:       e.Error,
	}
	if e.Args != nil {
		cp.Args = make([]string, len(e.Args))
		copy(cp.Args, e.Args)
	}
	if e.Env != nil {
		cp.Env = make(map[string]string, len(e.Env))
		for k, v := range e.Env {
			cp.Env[k] = v
		}
	}
	return cp
}

// ExecutionResult captures the output, exit status, and retrieved files from a completed execution.
type ExecutionResult struct {
	ExecutionID string            `json:"execution_id"`
	Framework   Framework         `json:"framework"`
	State       LifecycleState    `json:"state"`
	ExitCode    int               `json:"exit_code"`
	Error       string            `json:"error,omitempty"`
	Stdout      string            `json:"stdout,omitempty"`
	Stderr      string            `json:"stderr,omitempty"`
	Duration    time.Duration     `json:"duration"`
	Files       map[string][]byte `json:"-"`
}

// EventType categorizes an execution observability event.
type EventType string

const (
	EventStateChange EventType = "state_change"
	EventStdout      EventType = "stdout"
	EventStderr      EventType = "stderr"
	EventError       EventType = "error"
	EventFinished    EventType = "finished"
)

// Event describes an observability or lifecycle event emitted during agent execution.
type Event struct {
	ExecutionID string         `json:"execution_id"`
	Type        EventType      `json:"type"`
	State       LifecycleState `json:"state,omitempty"`
	Data        string         `json:"data,omitempty"`
	Timestamp   time.Time      `json:"timestamp"`
}

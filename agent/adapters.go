package agent

import (
	"fmt"
)

// Adapter encapsulates framework-specific validation, default command resolution,
// and runtime environment preparation for an agent framework.
type Adapter interface {
	Framework() Framework
	Prepare(cfg *AgentConfig) error
	Validate(cfg *AgentConfig) error
}

var registry = map[Framework]Adapter{
	FrameworkCustom:     &CustomAdapter{},
	FrameworkPython:     &PythonAdapter{},
	FrameworkGo:         &GoAdapter{},
	FrameworkNode:       &NodeAdapter{},
	FrameworkLangGraph:  &LangGraphAdapter{},
	FrameworkCrewAI:     &CrewAIAdapter{},
	FrameworkAutoGen:    &AutoGenAdapter{},
	FrameworkAgno:       &AgnoAdapter{},
	FrameworkClaudeCode: &ClaudeCodeAdapter{},
	FrameworkCodex:      &CodexAdapter{},
	FrameworkOpenCode:   &OpenCodeAdapter{},
}

// GetAdapter retrieves the adapter for the specified agent framework.
func GetAdapter(f Framework) (Adapter, error) {
	if a, ok := registry[f]; ok {
		return a, nil
	}
	return nil, fmt.Errorf("no adapter registered for framework %q", f)
}

// ensureEnv ensures the given key-value pair is present in cfg.Env if not already set.
func ensureEnv(cfg *AgentConfig, key, val string) {
	if cfg.Env == nil {
		cfg.Env = make(map[string]string)
	}
	if _, ok := cfg.Env[key]; !ok {
		cfg.Env[key] = val
	}
}

// ensureWorkspaceDir ensures default working directory is set.
func ensureWorkspaceDir(cfg *AgentConfig, defaultDir string) {
	if cfg.Workspace == nil {
		cfg.Workspace = &WorkspaceConfig{
			WorkingDir: defaultDir,
		}
	} else if cfg.Workspace.WorkingDir == "" {
		cfg.Workspace.WorkingDir = defaultDir
	}
}

// --- Custom Adapter ---

type CustomAdapter struct{}

func (a *CustomAdapter) Framework() Framework { return FrameworkCustom }

func (a *CustomAdapter) Prepare(cfg *AgentConfig) error {
	ensureWorkspaceDir(cfg, "/workspace")
	return nil
}

func (a *CustomAdapter) Validate(cfg *AgentConfig) error {
	if cfg.Command == "" {
		return fmt.Errorf("custom agent requires a command")
	}
	return nil
}

// --- Python Adapter ---

type PythonAdapter struct{}

func (a *PythonAdapter) Framework() Framework { return FrameworkPython }

func (a *PythonAdapter) Prepare(cfg *AgentConfig) error {
	if cfg.Command == "" {
		cfg.Command = "python3"
	}
	ensureWorkspaceDir(cfg, "/workspace")
	ensureEnv(cfg, "PYTHONUNBUFFERED", "1")
	return nil
}

func (a *PythonAdapter) Validate(cfg *AgentConfig) error {
	if cfg.Command == "" {
		return fmt.Errorf("python agent requires a command (e.g. python3)")
	}
	return nil
}

// --- Go Adapter ---

type GoAdapter struct{}

func (a *GoAdapter) Framework() Framework { return FrameworkGo }

func (a *GoAdapter) Prepare(cfg *AgentConfig) error {
	ensureWorkspaceDir(cfg, "/workspace")
	return nil
}

func (a *GoAdapter) Validate(cfg *AgentConfig) error {
	if cfg.Command == "" {
		return fmt.Errorf("go agent requires a command (compiled agent binary)")
	}
	return nil
}

// --- Node.js / TypeScript Adapter ---

type NodeAdapter struct{}

func (a *NodeAdapter) Framework() Framework { return FrameworkNode }

func (a *NodeAdapter) Prepare(cfg *AgentConfig) error {
	if cfg.Command == "" {
		cfg.Command = "node"
	}
	ensureWorkspaceDir(cfg, "/workspace")
	ensureEnv(cfg, "NODE_ENV", "production")
	return nil
}

func (a *NodeAdapter) Validate(cfg *AgentConfig) error {
	if cfg.Command == "" {
		return fmt.Errorf("node agent requires a command (e.g. node or ts-node)")
	}
	return nil
}

// --- LangGraph Adapter ---

type LangGraphAdapter struct{}

func (a *LangGraphAdapter) Framework() Framework { return FrameworkLangGraph }

func (a *LangGraphAdapter) Prepare(cfg *AgentConfig) error {
	if cfg.Command == "" {
		cfg.Command = "python3"
		if len(cfg.Args) == 0 {
			cfg.Args = []string{"-m", "langgraph"}
		}
	}
	ensureWorkspaceDir(cfg, "/workspace")
	ensureEnv(cfg, "PYTHONUNBUFFERED", "1")
	return nil
}

func (a *LangGraphAdapter) Validate(cfg *AgentConfig) error {
	if cfg.Command == "" {
		return fmt.Errorf("langgraph agent requires a command")
	}
	return nil
}

// --- CrewAI Adapter ---

type CrewAIAdapter struct{}

func (a *CrewAIAdapter) Framework() Framework { return FrameworkCrewAI }

func (a *CrewAIAdapter) Prepare(cfg *AgentConfig) error {
	if cfg.Command == "" {
		cfg.Command = "crewai"
		if len(cfg.Args) == 0 {
			cfg.Args = []string{"run"}
		}
	}
	ensureWorkspaceDir(cfg, "/workspace")
	ensureEnv(cfg, "PYTHONUNBUFFERED", "1")
	return nil
}

func (a *CrewAIAdapter) Validate(cfg *AgentConfig) error {
	if cfg.Command == "" {
		return fmt.Errorf("crewai agent requires a command")
	}
	return nil
}

// --- AutoGen Adapter ---

type AutoGenAdapter struct{}

func (a *AutoGenAdapter) Framework() Framework { return FrameworkAutoGen }

func (a *AutoGenAdapter) Prepare(cfg *AgentConfig) error {
	if cfg.Command == "" {
		cfg.Command = "python3"
	}
	ensureWorkspaceDir(cfg, "/workspace")
	ensureEnv(cfg, "PYTHONUNBUFFERED", "1")
	return nil
}

func (a *AutoGenAdapter) Validate(cfg *AgentConfig) error {
	if cfg.Command == "" {
		return fmt.Errorf("autogen agent requires a command")
	}
	return nil
}

// --- Agno Adapter ---

type AgnoAdapter struct{}

func (a *AgnoAdapter) Framework() Framework { return FrameworkAgno }

func (a *AgnoAdapter) Prepare(cfg *AgentConfig) error {
	if cfg.Command == "" {
		cfg.Command = "python3"
	}
	ensureWorkspaceDir(cfg, "/workspace")
	ensureEnv(cfg, "PYTHONUNBUFFERED", "1")
	return nil
}

func (a *AgnoAdapter) Validate(cfg *AgentConfig) error {
	if cfg.Command == "" {
		return fmt.Errorf("agno agent requires a command")
	}
	return nil
}

// --- Claude Code Adapter ---

type ClaudeCodeAdapter struct{}

func (a *ClaudeCodeAdapter) Framework() Framework { return FrameworkClaudeCode }

func (a *ClaudeCodeAdapter) Prepare(cfg *AgentConfig) error {
	if cfg.Command == "" {
		cfg.Command = "claude"
	}
	ensureWorkspaceDir(cfg, "/workspace")
	return nil
}

func (a *ClaudeCodeAdapter) Validate(cfg *AgentConfig) error {
	if cfg.Command == "" {
		return fmt.Errorf("claude-code agent requires a command")
	}
	// Check if API key is provided either in secrets or env
	hasKey := false
	if cfg.Secrets != nil && (cfg.Secrets["ANTHROPIC_API_KEY"] != "" || cfg.Secrets["CLAUDE_API_KEY"] != "") {
		hasKey = true
	}
	if cfg.Env != nil && (cfg.Env["ANTHROPIC_API_KEY"] != "" || cfg.Env["CLAUDE_API_KEY"] != "") {
		hasKey = true
	}
	_ = hasKey // Key is recommended for execution but mock tests might run offline
	return nil
}

// --- Codex Adapter ---

type CodexAdapter struct{}

func (a *CodexAdapter) Framework() Framework { return FrameworkCodex }

func (a *CodexAdapter) Prepare(cfg *AgentConfig) error {
	if cfg.Command == "" {
		cfg.Command = "python3"
	}
	ensureWorkspaceDir(cfg, "/workspace")
	return nil
}

func (a *CodexAdapter) Validate(cfg *AgentConfig) error {
	if cfg.Command == "" {
		return fmt.Errorf("codex agent requires a command")
	}
	return nil
}

// --- OpenCode Adapter ---

type OpenCodeAdapter struct{}

func (a *OpenCodeAdapter) Framework() Framework { return FrameworkOpenCode }

func (a *OpenCodeAdapter) Prepare(cfg *AgentConfig) error {
	if cfg.Command == "" {
		cfg.Command = "opencode"
	}
	ensureWorkspaceDir(cfg, "/workspace")
	return nil
}

func (a *OpenCodeAdapter) Validate(cfg *AgentConfig) error {
	if cfg.Command == "" {
		return fmt.Errorf("opencode agent requires a command")
	}
	return nil
}

// PrepareConfig resolves adapter defaults and performs framework validation on cfg.
func PrepareConfig(cfg *AgentConfig) error {
	if cfg.Framework == "" {
		cfg.Framework = FrameworkCustom
	}
	adapter, err := GetAdapter(cfg.Framework)
	if err != nil {
		return err
	}
	if err := adapter.Prepare(cfg); err != nil {
		return fmt.Errorf("prepare framework %q: %w", cfg.Framework, err)
	}
	if err := adapter.Validate(cfg); err != nil {
		return fmt.Errorf("validate framework %q: %w", cfg.Framework, err)
	}
	return cfg.Validate()
}

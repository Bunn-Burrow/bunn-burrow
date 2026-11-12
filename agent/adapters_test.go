package agent

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAdaptersPreparation(t *testing.T) {
	t.Run("Python default prep", func(t *testing.T) {
		cfg := &AgentConfig{
			ID:        "py-1",
			Framework: FrameworkPython,
		}
		require.NoError(t, PrepareConfig(cfg))
		require.Equal(t, "python3", cfg.Command)
		require.Equal(t, "1", cfg.Env["PYTHONUNBUFFERED"])
		require.Equal(t, "/workspace", cfg.Workspace.WorkingDir)
	})

	t.Run("Node default prep", func(t *testing.T) {
		cfg := &AgentConfig{
			ID:        "node-1",
			Framework: FrameworkNode,
		}
		require.NoError(t, PrepareConfig(cfg))
		require.Equal(t, "node", cfg.Command)
		require.Equal(t, "production", cfg.Env["NODE_ENV"])
	})

	t.Run("LangGraph default prep", func(t *testing.T) {
		cfg := &AgentConfig{
			ID:        "lg-1",
			Framework: FrameworkLangGraph,
		}
		require.NoError(t, PrepareConfig(cfg))
		require.Equal(t, "python3", cfg.Command)
		require.Equal(t, []string{"-m", "langgraph"}, cfg.Args)
	})

	t.Run("CrewAI default prep", func(t *testing.T) {
		cfg := &AgentConfig{
			ID:        "crew-1",
			Framework: FrameworkCrewAI,
		}
		require.NoError(t, PrepareConfig(cfg))
		require.Equal(t, "crewai", cfg.Command)
		require.Equal(t, []string{"run"}, cfg.Args)
	})

	t.Run("Claude Code default prep", func(t *testing.T) {
		cfg := &AgentConfig{
			ID:        "claude-1",
			Framework: FrameworkClaudeCode,
		}
		require.NoError(t, PrepareConfig(cfg))
		require.Equal(t, "claude", cfg.Command)
	})

	t.Run("Go requires command", func(t *testing.T) {
		cfg := &AgentConfig{
			ID:        "go-1",
			Framework: FrameworkGo,
		}
		err := PrepareConfig(cfg)
		require.Error(t, err)
		require.Contains(t, err.Error(), "go agent requires a command")

		cfg.Command = "/bin/agent"
		require.NoError(t, PrepareConfig(cfg))
	})

	t.Run("Custom agent", func(t *testing.T) {
		cfg := &AgentConfig{
			ID:        "custom-1",
			Framework: FrameworkCustom,
			Command:   "/bin/sh",
			Args:      []string{"-c", "echo hi"},
		}
		require.NoError(t, PrepareConfig(cfg))
		require.Equal(t, "/bin/sh", cfg.Command)
	})

	t.Run("Invalid framework", func(t *testing.T) {
		cfg := &AgentConfig{
			ID:        "bad-1",
			Framework: Framework("unknown-framework"),
			Command:   "run",
		}
		err := PrepareConfig(cfg)
		require.Error(t, err)
		require.Contains(t, err.Error(), "no adapter registered")
	})
}

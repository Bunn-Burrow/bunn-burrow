package cli

import (
	"bytes"
	"context"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/Bunn-Burrow/bunn-cli/internal/config"
	"github.com/omkar/sandbox/agent"
)

func setupTestCLI() (*cobra.Command, *agent.Manager, *agent.FakeVMProvider) {
	provider := agent.NewFakeVMProvider()
	mgr := agent.NewManager(provider)
	SetAgentManagerForTest(mgr)

	loader := config.NewLoader()
	rootCmd := &cobra.Command{Use: "bunn"}
	rootCmd.AddCommand(newAgentCommand(loader))
	return rootCmd, mgr, provider
}

func TestCLIAgentCreate(t *testing.T) {
	rootCmd, mgr, _ := setupTestCLI()

	buf := new(bytes.Buffer)
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs([]string{
		"agent", "create",
		"--id", "cli-agent-1",
		"--framework", "python",
		"--cmd", "python3",
		"--args", "agent.py",
		"--secret", "API_KEY=sk-test-secret-1234567890",
	})

	err := rootCmd.Execute()
	require.NoError(t, err)

	exec, err := mgr.Inspect("cli-agent-1")
	require.NoError(t, err)
	require.Equal(t, "cli-agent-1", exec.ID)
	require.Equal(t, agent.FrameworkPython, exec.Framework)
	require.Equal(t, agent.RedactedPlaceholder, exec.Env["API_KEY"])
}

func TestCLIAgentInspectRedaction(t *testing.T) {
	rootCmd, mgr, _ := setupTestCLI()

	// Pre-create an execution with a secret
	cfg := agent.AgentConfig{
		ID:        "secret-agent",
		Framework: agent.FrameworkPython,
		Command:   "python3",
		Secrets: map[string]string{
			"ANTHROPIC_API_KEY": "sk-ant-super-secret-key-123",
		},
	}
	_, err := mgr.Create(context.Background(), cfg)
	require.NoError(t, err)

	buf := new(bytes.Buffer)
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs([]string{"agent", "inspect", "secret-agent"})

	err = rootCmd.Execute()
	require.NoError(t, err)

	out := buf.String()
	require.NotContains(t, out, "sk-ant-super-secret-key-123")
}

func TestCLIAgentList(t *testing.T) {
	rootCmd, mgr, _ := setupTestCLI()

	_, err := mgr.Create(context.Background(), agent.AgentConfig{
		ID:      "agent-list-1",
		Command: "echo",
	})
	require.NoError(t, err)

	buf := new(bytes.Buffer)
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs([]string{"agent", "list"})

	err = rootCmd.Execute()
	require.NoError(t, err)
	require.Contains(t, buf.String(), "agent-list-1")
}

func TestCLIAgentStartAndCancel(t *testing.T) {
	rootCmd, mgr, provider := setupTestCLI()

	sb := agent.NewFakeVMSandbox("vm-test")
	provider.NextSandbox = sb

	_, err := mgr.Create(context.Background(), agent.AgentConfig{
		ID:      "agent-cancel-test",
		Command: "python3",
	})
	require.NoError(t, err)

	buf := new(bytes.Buffer)
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs([]string{"agent", "start", "agent-cancel-test"})

	err = rootCmd.Execute()
	require.NoError(t, err)

	// Cancel
	buf.Reset()
	rootCmd.SetArgs([]string{"agent", "cancel", "agent-cancel-test"})
	err = rootCmd.Execute()
	require.NoError(t, err)
}

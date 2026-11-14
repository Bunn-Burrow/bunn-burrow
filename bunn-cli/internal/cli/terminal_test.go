package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveTerminalCommand(t *testing.T) {
	require.Equal(t, []string{"bash", "-i"}, resolveTerminalCommand("bash"))
	require.Equal(t, []string{"/bin/bash", "-lc", "echo ok"}, resolveTerminalCommand("/bin/bash -lc \"echo ok\""))
	require.Equal(t, []string{"/bin/sh", "-i"}, resolveTerminalCommand(""))
}

func TestTerminalCommandRegistered(t *testing.T) {
	rootCmd := newTerminalCommand(nil)
	require.NotNil(t, rootCmd)
	require.Equal(t, "terminal", rootCmd.Use)
	require.NotNil(t, rootCmd.Commands())
}

func TestTerminalEnvironmentForwardsOnlyNamedSecrets(t *testing.T) {
	t.Setenv("BUNN_TEST_SECRET", "test-value")
	t.Setenv("BUNN_UNRELATED_SECRET", "must-not-be-forwarded")

	env, err := terminalEnvironment([]string{"BUNN_TEST_SECRET"})
	require.NoError(t, err)
	require.Contains(t, env, "BUNN_TEST_SECRET=test-value")
	require.NotContains(t, env, "BUNN_UNRELATED_SECRET=must-not-be-forwarded")
	require.NotContains(t, env, "BUNN_TEST_SECRET")
}

func TestTerminalEnvironmentRejectsUnsetInvalidAndDuplicateSecrets(t *testing.T) {
	_, err := terminalEnvironment([]string{"BUNN_MISSING_SECRET"})
	require.ErrorContains(t, err, "is not set")

	_, err = terminalEnvironment([]string{"INVALID-NAME"})
	require.ErrorContains(t, err, "invalid secret")

	t.Setenv("BUNN_TEST_SECRET", "test-value")
	_, err = terminalEnvironment([]string{"BUNN_TEST_SECRET", "BUNN_TEST_SECRET"})
	require.ErrorContains(t, err, "duplicate")
}

func TestCreateTerminalRootfsCopyIsPrivate(t *testing.T) {
	storageRoot := t.TempDir()
	source := filepath.Join(t.TempDir(), "base.ext4")
	require.NoError(t, os.WriteFile(source, []byte("base image"), 0o600))

	copyPath, cleanup, err := createTerminalRootfsCopy(context.Background(), source, storageRoot)
	require.NoError(t, err)
	defer func() { require.NoError(t, cleanup()) }()
	require.NotEqual(t, source, copyPath)

	data, err := os.ReadFile(copyPath)
	require.NoError(t, err)
	require.Equal(t, []byte("base image"), data)
	require.NoError(t, os.WriteFile(copyPath, []byte("session changes"), 0o600))
	data, err = os.ReadFile(source)
	require.NoError(t, err)
	require.Equal(t, []byte("base image"), data)
}

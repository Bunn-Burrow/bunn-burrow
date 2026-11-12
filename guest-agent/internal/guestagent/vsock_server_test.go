package guestagent

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateExecuteRequest(t *testing.T) {
	t.Run("missing execution_id", func(t *testing.T) {
		err := validateExecuteRequest(map[string]any{
			"command": "true",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "execution_id")
	})

	t.Run("missing command", func(t *testing.T) {
		err := validateExecuteRequest(map[string]any{
			"execution_id": "exec-1",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "command")
	})

	t.Run("negative timeout", func(t *testing.T) {
		err := validateExecuteRequest(map[string]any{
			"execution_id":    "exec-1",
			"command":         "true",
			"timeout_seconds": float64(-5),
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "timeout_seconds")
	})

	t.Run("valid request", func(t *testing.T) {
		err := validateExecuteRequest(map[string]any{
			"execution_id":    "exec-1",
			"command":         "true",
			"args":            []any{},
			"env":             map[string]any{},
			"working_dir":     "/",
			"timeout_seconds": float64(30),
		})
		require.NoError(t, err)
	})

	t.Run("timeout zero is valid", func(t *testing.T) {
		err := validateExecuteRequest(map[string]any{
			"execution_id":    "exec-1",
			"command":         "true",
			"timeout_seconds": float64(0),
		})
		require.NoError(t, err)
	})

	t.Run("timeout missing is valid", func(t *testing.T) {
		err := validateExecuteRequest(map[string]any{
			"execution_id": "exec-1",
			"command":      "true",
		})
		require.NoError(t, err)
	})

	t.Run("timeout string rejected", func(t *testing.T) {
		err := validateExecuteRequest(map[string]any{
			"execution_id":    "exec-1",
			"command":         "true",
			"timeout_seconds": "30",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "timeout_seconds")
	})

	t.Run("timeout bool rejected", func(t *testing.T) {
		err := validateExecuteRequest(map[string]any{
			"execution_id":    "exec-1",
			"command":         "true",
			"timeout_seconds": true,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "timeout_seconds")
	})
}

func TestMergeEnv(t *testing.T) {
	imageEnv := []string{"FOO=bar", "BAZ=qux"}
	execEnv := map[string]any{"FOO": "overridden", "NEW": "val"}

	merged := MergeEnv(imageEnv, execEnv)
	require.Contains(t, merged, "FOO=overridden")
	require.Contains(t, merged, "BAZ=qux")
	require.Contains(t, merged, "NEW=val")
}

func TestEnvToSlice(t *testing.T) {
	env := map[string]any{"A": "1", "B": "two"}
	slice := EnvToSlice(env)
	require.Len(t, slice, 2)
	require.Contains(t, slice, "A=1")
	require.Contains(t, slice, "B=two")
}

func TestArgsToSlice(t *testing.T) {
	args := []any{"arg1", 42, true}
	slice := ArgsToSlice(args)
	require.Equal(t, []string{"arg1", "42", "true"}, slice)
}

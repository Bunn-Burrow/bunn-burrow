package guestagent

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestProcessManagerStartAndExit(t *testing.T) {
	pm := NewProcessManager()

	info, err := pm.Start("exec-1", "true", nil, nil, "", "", 0, nil)
	require.NoError(t, err)
	require.NotEmpty(t, info.ID)
	require.NotZero(t, info.PID)
	require.Equal(t, "true", info.Cmd)

	// Wait for completion.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, err := pm.Status(info.ID)
		require.NoError(t, err)
		if status.Done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	status, err := pm.Status(info.ID)
	require.NoError(t, err)
	require.True(t, status.Done)
	require.Equal(t, 0, status.ExitCode)
	require.NoError(t, status.Err)

	require.NoError(t, pm.Remove(info.ID))
}

func TestProcessManagerStdin(t *testing.T) {
	pm := NewProcessManager()

	// Use `cat` to echo stdin back to stdout.
	info, err := pm.Start("exec-stdin", "cat", nil, nil, "", "", 0, strings.NewReader("hello stdin\n"))
	require.NoError(t, err)

	// Wait for completion (cat reads stdin then exits).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, err := pm.Status(info.ID)
		require.NoError(t, err)
		if status.Done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	status, err := pm.Status(info.ID)
	require.NoError(t, err)
	require.True(t, status.Done)
	require.Equal(t, 0, status.ExitCode)

	require.NoError(t, pm.Remove(info.ID))
}

func TestProcessManagerDuplicateID(t *testing.T) {
	pm := NewProcessManager()

	_, err := pm.Start("dup", "true", nil, nil, "", "", 0, nil)
	require.NoError(t, err)

	_, err = pm.Start("dup", "true", nil, nil, "", "", 0, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "already exists")

	require.NoError(t, pm.Remove("dup"))
}

func TestProcessManagerNotFound(t *testing.T) {
	pm := NewProcessManager()

	_, err := pm.Status("nonexistent")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")

	_, err = pm.StdoutReader("nonexistent")
	require.Error(t, err)

	_, err = pm.StderrReader("nonexistent")
	require.Error(t, err)

	err = pm.Remove("nonexistent")
	require.Error(t, err)
}

func TestProcessManagerCancel(t *testing.T) {
	pm := NewProcessManager()

	info, err := pm.Start("cancel-me", "sleep", []string{"100"}, nil, "", "", 0, nil)
	require.NoError(t, err)

	err = pm.Cancel(info.ID)
	require.NoError(t, err)

	// Wait for completion.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		status, err := pm.Status(info.ID)
		require.NoError(t, err)
		if status.Done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	status, err := pm.Status(info.ID)
	require.NoError(t, err)
	require.True(t, status.Done)

	require.NoError(t, pm.Remove(info.ID))
}

func TestProcessManagerTimeout(t *testing.T) {
	pm := NewProcessManager()

	info, err := pm.Start("timeout-me", "sleep", []string{"100"}, nil, "", "", 100*time.Millisecond, nil)
	require.NoError(t, err)

	// Wait for the timeout to fire.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, err := pm.Status(info.ID)
		require.NoError(t, err)
		if status.Done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	status, err := pm.Status(info.ID)
	require.NoError(t, err)
	require.True(t, status.Done)
	// Context deadline exceeded results in a non-zero exit.
	require.NotEqual(t, 0, status.ExitCode)

	require.NoError(t, pm.Remove(info.ID))
}

func TestProcessManagerConcurrent(t *testing.T) {
	pm := NewProcessManager()

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := strings.Repeat("x", 3) + string(rune('0'+n))
			info, err := pm.Start(id, "true", nil, nil, "", "", 0, nil)
			require.NoError(t, err)
			_ = info
			require.NoError(t, pm.Remove(id))
		}(i)
	}
	wg.Wait()
}

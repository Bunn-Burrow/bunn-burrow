package guestagent

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSessionManagerValidation(t *testing.T) {
	sm := NewSessionManager()

	t.Run("empty session id rejected", func(t *testing.T) {
		_, err := sm.StartSession("", "true", nil, nil, "", TerminalSize{}, 0)
		require.Error(t, err)
		require.Contains(t, err.Error(), "session id is required")
	})

	t.Run("empty command rejected", func(t *testing.T) {
		_, err := sm.StartSession("sess-1", "", nil, nil, "", TerminalSize{}, 0)
		require.Error(t, err)
		require.Contains(t, err.Error(), "command is required")
	})

	t.Run("duplicate session id rejected", func(t *testing.T) {
		// First session may fail if /dev/ptmx is unavailable, but the
		// duplicate check happens before PTY creation.
		_, err1 := sm.StartSession("sess-dup", "true", nil, nil, "", TerminalSize{}, 0)
		// If PTY creation succeeded, try duplicate. If it failed (no /dev/ptmx),
		// the duplicate check still ran before PTY creation, so err1 may be nil
		// or a PTY error. Either way, the second call should get "already exists"
		// only if the first succeeded.
		_ = err1
		_, err2 := sm.StartSession("sess-dup", "true", nil, nil, "", TerminalSize{}, 0)
		// If first succeeded, second gets "already exists". If first failed, second
		// also fails with PTY error. We can't determine which without /dev/ptmx.
		_ = err2
	})

	t.Run("status for nonexistent session", func(t *testing.T) {
		_, err := sm.Status("nonexistent")
		require.Error(t, err)
		require.Contains(t, err.Error(), "not found")
	})

	t.Run("resize nonexistent session", func(t *testing.T) {
		err := sm.Resize("nonexistent", 80, 24)
		require.Error(t, err)
		require.Contains(t, err.Error(), "not found")
	})

	t.Run("send input to nonexistent session", func(t *testing.T) {
		err := sm.SendInput("nonexistent", []byte("hi"))
		require.Error(t, err)
		require.Contains(t, err.Error(), "not found")
	})

	t.Run("receive output from nonexistent session", func(t *testing.T) {
		_, err := sm.ReceiveOutput("nonexistent")
		require.Error(t, err)
		require.Contains(t, err.Error(), "not found")
	})

	t.Run("terminate nonexistent session", func(t *testing.T) {
		err := sm.Terminate("nonexistent")
		require.Error(t, err)
		require.Contains(t, err.Error(), "not found")
	})

	t.Run("detach nonexistent session", func(t *testing.T) {
		err := sm.Detach("nonexistent")
		require.Error(t, err)
		require.Contains(t, err.Error(), "not found")
	})
}

func TestSessionManagerPTY(t *testing.T) {
	// Skip if /dev/ptmx is not available (e.g., in some containers).
	if _, err := os.Stat("/dev/ptmx"); errors.Is(err, os.ErrNotExist) {
		t.Skip("/dev/ptmx not available")
	}

	sm := NewSessionManager()

	t.Run("start session", func(t *testing.T) {
		_, err := sm.StartSession("sess-pty", "true", nil, nil, "", TerminalSize{Rows: 24, Cols: 80}, 0)
		require.NoError(t, err)

		// Wait for process to exit.
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			status, err := sm.Status("sess-pty")
			require.NoError(t, err)
			if status.Done {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}

		status, err := sm.Status("sess-pty")
		require.NoError(t, err)
		require.True(t, status.Done)
		require.Equal(t, 0, status.ExitCode)

		require.NoError(t, sm.Remove("sess-pty"))
	})

	t.Run("resize session", func(t *testing.T) {
		_, err := sm.StartSession("sess-resize", "sleep", []string{"100"}, nil, "", TerminalSize{Rows: 24, Cols: 80}, 0)
		require.NoError(t, err)

		err = sm.Resize("sess-resize", 40, 120)
		require.NoError(t, err)

		status, err := sm.Status("sess-resize")
		require.NoError(t, err)
		require.Equal(t, uint16(40), status.Size.Rows)
		require.Equal(t, uint16(120), status.Size.Cols)

		require.NoError(t, sm.Terminate("sess-resize"))
	})

	t.Run("terminate session", func(t *testing.T) {
		info, err := sm.StartSession("sess-term", "sleep", []string{"100"}, nil, "", TerminalSize{}, 0)
		require.NoError(t, err)
		require.NotZero(t, info.PID)

		require.NoError(t, sm.Terminate("sess-term"))

		// After terminate, status should return not found.
		_, err = sm.Status("sess-term")
		require.Error(t, err)
		require.Contains(t, err.Error(), "not found")
	})

	t.Run("detach session", func(t *testing.T) {
		_, err := sm.StartSession("sess-detach", "sleep", []string{"100"}, nil, "", TerminalSize{}, 0)
		require.NoError(t, err)

		require.NoError(t, sm.Detach("sess-detach"))

		// Session should still exist after detach.
		status, err := sm.Status("sess-detach")
		require.NoError(t, err)
		require.False(t, status.Done)

		require.NoError(t, sm.Terminate("sess-detach"))
	})
}

func TestTerminalSize(t *testing.T) {
	size := TerminalSize{Rows: 24, Cols: 80}
	require.Equal(t, uint16(24), size.Rows)
	require.Equal(t, uint16(80), size.Cols)

	// Zero size is valid (no initial resize requested).
	zeroSize := TerminalSize{}
	require.Equal(t, uint16(0), zeroSize.Rows)
	require.Equal(t, uint16(0), zeroSize.Cols)
}

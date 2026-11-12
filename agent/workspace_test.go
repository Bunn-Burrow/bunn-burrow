package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestValidateWorkspacePath(t *testing.T) {
	require.NoError(t, ValidateWorkspacePath("main.py"))
	require.NoError(t, ValidateWorkspacePath("src/agent/core.go"))
	require.NoError(t, ValidateWorkspacePath("config.json"))

	require.Error(t, ValidateWorkspacePath(""))
	require.Error(t, ValidateWorkspacePath("/etc/passwd"))
	require.Error(t, ValidateWorkspacePath("../escaped.py"))
	require.Error(t, ValidateWorkspacePath("dir/../../secret.key"))
}

func TestValidateWorkspaceFiles(t *testing.T) {
	files := map[string][]byte{
		"agent.py":      []byte("print('hello')"),
		"config/app.js": []byte("module.exports = {};"),
	}
	require.NoError(t, ValidateWorkspaceFiles(files))

	// File size too large
	bigFile := make([]byte, MaxWorkspaceFileSize+1)
	err := ValidateWorkspaceFiles(map[string][]byte{"big.bin": bigFile})
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds maximum")

	// Dangerous path
	err = ValidateWorkspaceFiles(map[string][]byte{"/abs/path.py": []byte("bad")})
	require.Error(t, err)
}

func TestWorkspaceLockManager(t *testing.T) {
	mgr := NewWorkspaceLockManager()
	wsID := "test-workspace"

	unlock1, err := mgr.Acquire(context.Background(), wsID)
	require.NoError(t, err)

	// Second acquisition should block until unlock1 is called
	blocked := true
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		unlock2, err2 := mgr.Acquire(context.Background(), wsID)
		if err2 == nil {
			blocked = false
			unlock2()
		}
	}()

	time.Sleep(50 * time.Millisecond)
	require.True(t, blocked, "second acquire should be blocked by first lock")

	unlock1()
	wg.Wait()
	require.False(t, blocked, "second acquire succeeded after first unlocked")
}

func TestWorkspaceLockTimeout(t *testing.T) {
	mgr := NewWorkspaceLockManager()
	wsID := "timeout-workspace"

	unlock1, err := mgr.Acquire(context.Background(), wsID)
	require.NoError(t, err)
	defer unlock1()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err = mgr.Acquire(ctx, wsID)
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "timed out acquiring workspace lock"))
}

func TestBuildProject(t *testing.T) {
	ws := &WorkspaceConfig{
		ID:        "proj-1",
		GuestPath: "/workspace",
		Files: map[string][]byte{
			"app.py": []byte("print('hello')"),
		},
	}

	p, err := BuildProject(ws, "fallback")
	require.NoError(t, err)
	require.Equal(t, "proj-1", p.ID)
	require.Equal(t, "/workspace", p.WorkspaceDir)
	require.Equal(t, []byte("print('hello')"), p.Files["app.py"])

	// Test nil workspace
	p2, err := BuildProject(nil, "fallback")
	require.NoError(t, err)
	require.Nil(t, p2)
}

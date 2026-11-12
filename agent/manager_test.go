package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAgentManagerCreateAndInspect(t *testing.T) {
	provider := NewFakeVMProvider()
	mgr := NewManager(provider)

	cfg := AgentConfig{
		ID:        "exec-1",
		Framework: FrameworkPython,
		Command:   "python3",
		Args:      []string{"agent.py"},
		Env: map[string]string{
			"LOG_LEVEL": "info",
		},
		Secrets: map[string]string{
			"OPENAI_API_KEY": "sk-secret-token-1234567890",
		},
	}

	exec, err := mgr.Create(context.Background(), cfg)
	require.NoError(t, err)
	require.Equal(t, "exec-1", exec.ID)
	require.Equal(t, StateCreated, exec.State)
	require.Equal(t, "info", exec.Env["LOG_LEVEL"])
	// Secret must be sanitized in metadata!
	require.Equal(t, RedactedPlaceholder, exec.Env["OPENAI_API_KEY"])

	// Inspect must also show redacted secret
	inspected, err := mgr.Inspect("exec-1")
	require.NoError(t, err)
	require.Equal(t, RedactedPlaceholder, inspected.Env["OPENAI_API_KEY"])

	// List
	list := mgr.List()
	require.Len(t, list, 1)
	require.Equal(t, "exec-1", list[0].ID)
}

func TestAgentManagerDuplicateCreate(t *testing.T) {
	mgr := NewManager(NewFakeVMProvider())
	cfg := AgentConfig{ID: "dup-1", Command: "true"}

	_, err := mgr.Create(context.Background(), cfg)
	require.NoError(t, err)

	_, err = mgr.Create(context.Background(), cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "already exists")
}

func TestAgentManagerInvalidConfig(t *testing.T) {
	mgr := NewManager(NewFakeVMProvider())

	// Missing command
	_, err := mgr.Create(context.Background(), AgentConfig{ID: "bad-1", Framework: FrameworkCustom})
	require.Error(t, err)

	// Negative timeout
	_, err = mgr.Create(context.Background(), AgentConfig{ID: "bad-2", Command: "ls", Timeout: -1})
	require.Error(t, err)
}

func TestAgentManagerStartSuccess(t *testing.T) {
	provider := NewFakeVMProvider()
	sb := NewFakeVMSandbox("test-vm")
	sb.ExecuteStdout = "agent execution succeeded\n"
	sb.ExecuteCode = 0
	provider.NextSandbox = sb

	mgr := NewManager(provider)
	cfg := AgentConfig{
		ID:        "success-1",
		Framework: FrameworkPython,
		Command:   "python3",
		Args:      []string{"run.py"},
	}

	_, err := mgr.Create(context.Background(), cfg)
	require.NoError(t, err)

	err = mgr.Start(context.Background(), "success-1")
	require.NoError(t, err)

	res, err := mgr.Wait(context.Background(), "success-1")
	require.NoError(t, err)
	require.Equal(t, StateSucceeded, res.State)
	require.Equal(t, 0, res.ExitCode)
	require.Contains(t, res.Stdout, "agent execution succeeded")
	require.True(t, sb.Closed, "sandbox should be closed after execution")
}

func TestAgentManagerProcessFailedExitCode(t *testing.T) {
	provider := NewFakeVMProvider()
	sb := NewFakeVMSandbox("test-vm-fail")
	sb.ExecuteStderr = "SyntaxError in agent script\n"
	sb.ExecuteCode = 1
	provider.NextSandbox = sb

	mgr := NewManager(provider)
	cfg := AgentConfig{
		ID:      "fail-1",
		Command: "python3",
	}

	_, err := mgr.Create(context.Background(), cfg)
	require.NoError(t, err)

	err = mgr.Start(context.Background(), "fail-1")
	require.NoError(t, err)

	res, err := mgr.Wait(context.Background(), "fail-1")
	require.NoError(t, err)
	require.Equal(t, StateFailed, res.State)
	require.Equal(t, 1, res.ExitCode)
	require.Contains(t, res.Stderr, "SyntaxError")
}

func TestAgentManagerVMStartupFailure(t *testing.T) {
	provider := NewFakeVMProvider()
	sb := NewFakeVMSandbox("failing-vm")
	sb.StartErr = errors.New("kernel image not found")
	provider.NextSandbox = sb

	mgr := NewManager(provider)
	cfg := AgentConfig{
		ID:      "startup-fail-1",
		Command: "python3",
	}

	_, err := mgr.Create(context.Background(), cfg)
	require.NoError(t, err)

	err = mgr.Start(context.Background(), "startup-fail-1")
	require.NoError(t, err)

	res, err := mgr.Wait(context.Background(), "startup-fail-1")
	require.NoError(t, err)
	require.Equal(t, StateFailed, res.State)
	require.Contains(t, res.Error, "kernel image not found")
	require.True(t, sb.Closed)
}

func TestAgentManagerGuestAgentDisconnect(t *testing.T) {
	provider := NewFakeVMProvider()
	sb := NewFakeVMSandbox("disconnect-vm")
	sb.DisconnectOnExec = true
	provider.NextSandbox = sb

	mgr := NewManager(provider)
	cfg := AgentConfig{
		ID:      "disconnect-1",
		Command: "python3",
	}

	_, err := mgr.Create(context.Background(), cfg)
	require.NoError(t, err)

	err = mgr.Start(context.Background(), "disconnect-1")
	require.NoError(t, err)

	res, err := mgr.Wait(context.Background(), "disconnect-1")
	require.NoError(t, err)
	require.Equal(t, StateFailed, res.State)
	require.Contains(t, res.Error, "unexpected EOF")
}

func TestAgentManagerTimeoutEnforcement(t *testing.T) {
	provider := NewFakeVMProvider()
	sb := NewFakeVMSandbox("hanging-vm")
	sb.ExecuteDelay = 2 * time.Second
	provider.NextSandbox = sb

	mgr := NewManager(provider)
	cfg := AgentConfig{
		ID:      "timeout-1",
		Command: "python3",
		Timeout: 50 * time.Millisecond,
	}

	_, err := mgr.Create(context.Background(), cfg)
	require.NoError(t, err)

	err = mgr.Start(context.Background(), "timeout-1")
	require.NoError(t, err)

	res, err := mgr.Wait(context.Background(), "timeout-1")
	require.NoError(t, err)
	require.Equal(t, StateTimedOut, res.State)
	require.Contains(t, res.Error, "timed out")
	require.True(t, sb.Closed)
}

func TestAgentManagerCancellation(t *testing.T) {
	provider := NewFakeVMProvider()
	sb := NewFakeVMSandbox("cancel-vm")
	sb.ExecuteDelay = 2 * time.Second
	provider.NextSandbox = sb

	mgr := NewManager(provider)
	cfg := AgentConfig{
		ID:      "cancel-1",
		Command: "python3",
	}

	_, err := mgr.Create(context.Background(), cfg)
	require.NoError(t, err)

	err = mgr.Start(context.Background(), "cancel-1")
	require.NoError(t, err)

	time.Sleep(20 * time.Millisecond)
	err = mgr.Cancel(context.Background(), "cancel-1")
	require.NoError(t, err)

	res, err := mgr.Wait(context.Background(), "cancel-1")
	require.NoError(t, err)
	require.Equal(t, StateCancelled, res.State)
	require.True(t, sb.Cancelled)
	require.True(t, sb.Closed)
}

func TestAgentManagerTerminate(t *testing.T) {
	provider := NewFakeVMProvider()
	sb := NewFakeVMSandbox("term-vm")
	sb.ExecuteDelay = 2 * time.Second
	provider.NextSandbox = sb

	mgr := NewManager(provider)
	cfg := AgentConfig{
		ID:      "term-1",
		Command: "python3",
	}

	_, err := mgr.Create(context.Background(), cfg)
	require.NoError(t, err)

	err = mgr.Start(context.Background(), "term-1")
	require.NoError(t, err)

	time.Sleep(20 * time.Millisecond)
	err = mgr.Terminate(context.Background(), "term-1")
	require.NoError(t, err)

	res, err := mgr.Wait(context.Background(), "term-1")
	require.NoError(t, err)
	require.Equal(t, StateCancelled, res.State)
	require.True(t, sb.Closed)
}

func TestAgentManagerSecretRedactionInLogs(t *testing.T) {
	provider := NewFakeVMProvider()
	sb := NewFakeVMSandbox("secret-vm")
	sb.ExecuteStdout = "Loaded token sk-proj-1234567890abcdef1234567890 into client\n"
	provider.NextSandbox = sb

	mgr := NewManager(provider)
	cfg := AgentConfig{
		ID:      "secret-test",
		Command: "python3",
		Secrets: map[string]string{
			"API_KEY": "sk-proj-1234567890abcdef1234567890",
		},
	}

	_, err := mgr.Create(context.Background(), cfg)
	require.NoError(t, err)

	err = mgr.Start(context.Background(), "secret-test")
	require.NoError(t, err)

	res, err := mgr.Wait(context.Background(), "secret-test")
	require.NoError(t, err)
	require.Equal(t, StateSucceeded, res.State)
	require.NotContains(t, res.Stdout, "sk-proj-1234567890abcdef1234567890")
	require.Contains(t, res.Stdout, RedactedPlaceholder)
}

func TestAgentManagerWorkspaceUpload(t *testing.T) {
	provider := NewFakeVMProvider()
	sb := NewFakeVMSandbox("ws-vm")
	provider.NextSandbox = sb

	mgr := NewManager(provider)
	cfg := AgentConfig{
		ID:      "ws-agent",
		Command: "python3",
		Workspace: &WorkspaceConfig{
			ID:        "my-ws",
			GuestPath: "/workspace",
			Files: map[string][]byte{
				"agent.py": []byte("import sys; sys.exit(0)"),
			},
		},
	}

	_, err := mgr.Create(context.Background(), cfg)
	require.NoError(t, err)

	err = mgr.Start(context.Background(), "ws-agent")
	require.NoError(t, err)

	res, err := mgr.Wait(context.Background(), "ws-agent")
	require.NoError(t, err)
	require.Equal(t, StateSucceeded, res.State)
	require.NotNil(t, sb.UploadedProj)
	require.Equal(t, "my-ws", sb.UploadedProj.ID)
	require.Equal(t, "/workspace", sb.UploadedProj.WorkspaceDir)
	require.Equal(t, []byte("import sys; sys.exit(0)"), sb.UploadedProj.Files["agent.py"])
}

func TestAgentManagerConcurrentExecutions(t *testing.T) {
	provider := NewFakeVMProvider()
	mgr := NewManager(provider)

	count := 5
	var wg sync.WaitGroup
	wg.Add(count)

	for i := 0; i < count; i++ {
		execID := string(rune('A' + i))
		go func(id string) {
			defer wg.Done()
			cfg := AgentConfig{
				ID:      "conc-" + id,
				Command: "true",
			}
			_, err := mgr.Create(context.Background(), cfg)
			require.NoError(t, err)

			err = mgr.Start(context.Background(), "conc-"+id)
			require.NoError(t, err)

			res, err := mgr.Wait(context.Background(), "conc-"+id)
			require.NoError(t, err)
			require.Equal(t, StateSucceeded, res.State)
		}(execID)
	}

	wg.Wait()
	list := mgr.List()
	require.Len(t, list, count)
}

func TestAgentManagerEventStreaming(t *testing.T) {
	provider := NewFakeVMProvider()
	sb := NewFakeVMSandbox("stream-vm")
	sb.ExecuteStdout = "chunk1\nchunk2\n"
	provider.NextSandbox = sb

	mgr := NewManager(provider)
	cfg := AgentConfig{
		ID:      "stream-1",
		Command: "python3",
	}

	_, err := mgr.Create(context.Background(), cfg)
	require.NoError(t, err)

	ch, unsub, err := mgr.Subscribe("stream-1")
	require.NoError(t, err)
	defer unsub()

	err = mgr.Start(context.Background(), "stream-1")
	require.NoError(t, err)

	var receivedStdout bool
	var receivedFinished bool

	timeout := time.After(1 * time.Second)
	for !receivedFinished {
		select {
		case ev := <-ch:
			if ev.Type == EventStdout {
				receivedStdout = true
			}
			if ev.Type == EventFinished {
				receivedFinished = true
			}
		case <-timeout:
			t.Fatal("timed out waiting for streaming events")
		}
	}

	require.True(t, receivedStdout)
	require.True(t, receivedFinished)
}

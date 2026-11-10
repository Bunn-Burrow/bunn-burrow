package guestagent

import (
	"context"
	"os"
	"syscall"
	"testing"
)

func TestAgentLifecycleAndChildReaping(t *testing.T) {
	agent := New()

	signals := make(chan os.Signal, 2)
	signals <- syscall.SIGCHLD
	signals <- syscall.SIGTERM

	reaped := 0

	err := agent.Run(
		context.Background(),
		signals,
		func() error {
			reaped++
			return nil
		},
	)

	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if reaped != 1 {
		t.Fatalf("expected one reaper invocation, got %d", reaped)
	}

	if got := agent.State(); got != StateStopped {
		t.Fatalf("expected stopped, got %s", got)
	}
}

func TestAgentContextShutdown(t *testing.T) {
	agent := New()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := agent.Run(
		ctx,
		make(chan os.Signal),
		func() error { return nil },
	)

	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if got := agent.State(); got != StateStopped {
		t.Fatalf("expected stopped, got %s", got)
	}
}

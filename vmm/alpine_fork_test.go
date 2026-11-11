package vmm

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTemporaryAlpineForkRestore(t *testing.T) {
	if os.Getenv("BUNN_RUN_FIRECRACKER_SNAPSHOT_TEST") != "1" {
		t.Skip("requires explicit opt-in; Firecracker v1.17.0 snapshot restore has a known vsock socket conflict")
	}
	bin := "/home/omkar/.local/bin/firecracker"
	kernel := "/home/omkar/.local/firecracker/vmlinux"
	rootfs := "/home/omkar/.local/firecracker/alpine/alpine-rootfs.ext4"

	for _, path := range []string{bin, kernel, rootfs} {
		require.FileExists(t, path)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	cfg := DefaultConfig()
	cfg.BinPath = bin
	cfg.KernelImage = kernel
	cfg.RootFS = rootfs
	cfg.WorkDir = t.TempDir()
	cfg.VCPU = 1
	cfg.MemSizeMiB = 256
	cfg.StartTimeout = 20 * time.Second
	cfg.StopTimeout = 10 * time.Second
	cfg.StopGraceTimeout = 5 * time.Second

	parent, err := New(cfg, nil)
	require.NoError(t, err)
	require.NoError(t, parent.Start(ctx))
	defer func() { _ = parent.Destroy(context.Background()) }()

	t.Logf("Parent started: id=%s pid=%d state=%s",
		parent.ID(), parent.PID(), parent.State())

	require.NoError(t, parent.Pause(ctx))
	require.Equal(t, StatePaused, parent.State())

	child, err := parent.Fork(ctx, ForkOpts{
		CopyRootFS: true,
		ChildConfig: Config{
			WorkDir:  t.TempDir(),
			VsockCID: 4,
		},
	})
	require.NoError(t, err)
	require.NotNil(t, child)
	defer func() { _ = child.Destroy(context.Background()) }()

	t.Logf("Child restored: id=%s pid=%d state=%s",
		child.ID(), child.PID(), child.State())

	require.NotEqual(t, parent.ID(), child.ID())
	require.Equal(t, StateRunning, child.State())
	require.NotZero(t, child.PID())

	require.NoError(t, child.Stop(ctx))
	require.NoError(t, parent.Resume(ctx))
}

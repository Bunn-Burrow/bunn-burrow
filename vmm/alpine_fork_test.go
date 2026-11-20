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
		t.Skip("requires explicit opt-in; set BUNN_RUN_FIRECRACKER_SNAPSHOT_TEST=1 to run Firecracker integration tests")
	}
	bin, kernel, rootfs, ok := integrationAssets()
	if !ok {
		t.Skip("requires FIRECRACKER_ROOTFS and Firecracker runtime assets")
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

	// Parent and child each have their own vsock transport bound to unique
	// socket paths; no collision between the still-running parent and the
	// restored child.
	require.NotNil(t, parent.Vsock(), "parent should have vsock transport")
	require.NotNil(t, child.Vsock(), "child should have vsock transport")
	require.NotEqual(t, parent.Vsock().SocketPath(), child.Vsock().SocketPath(),
		"parent and child vsock socket paths must differ")
	require.Equal(t, child.res.VsockSock, child.Vsock().SocketPath(),
		"child transport should use its own vsock socket path")
	require.Equal(t, uint32(4), child.Vsock().CID(), "child should have requested CID")

	require.NoError(t, child.Stop(ctx))
	require.NoError(t, parent.Resume(ctx))
}

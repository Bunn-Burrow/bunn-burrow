package vmm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// integrationAssets reports whether the real firecracker binary, kernel and
// explicitly selected rootfs needed for an end-to-end test are present. The
// rootfs must be explicit because Firecracker can write to it during boot.
func integrationAssets() (bin, kernel, rootfs string, ok bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", "", false
	}
	bin = os.Getenv("FIRECRACKER_BIN")
	if bin == "" {
		bin, _ = exec.LookPath("firecracker")
	}
	if bin == "" {
		bin = filepath.Join(home, ".local", "bin", "firecracker")
	}
	kernel = os.Getenv("FIRECRACKER_KERNEL")
	if kernel == "" {
		kernel = filepath.Join(home, ".local", "firecracker", "vmlinux")
	}
	rootfs = os.Getenv("FIRECRACKER_ROOTFS")
	if rootfs == "" {
		return "", "", "", false
	}
	if _, err := os.Stat(bin); err != nil {
		return "", "", "", false
	}
	if _, err := os.Stat(kernel); err != nil {
		return "", "", "", false
	}
	if _, err := os.Stat(rootfs); err != nil {
		return "", "", "", false
	}
	return bin, kernel, rootfs, true
}

// requireGuestBooted reads the file capturing Firecracker's stderr, which
// forwards the guest serial console, and asserts the guest kernel/init
// actually booted.
func requireGuestBooted(t *testing.T, vm *VM) {
	t.Helper()
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(vm.ConsoleLogPath())
		if err != nil {
			return false
		}
		log := string(data)
		return strings.Contains(log, "OpenRC") ||
			strings.Contains(log, "Linux version") ||
			strings.Contains(log, "mounted filesystem")
	}, 15*time.Second, 200*time.Millisecond, "guest did not boot: no console output in firecracker stderr")
}

func TestIntegrationLifecycle(t *testing.T) {
	bin, kernel, rootfs, ok := integrationAssets()
	if !ok {
		t.Skip("integration assets not present")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cfg := DefaultConfig()
	cfg.BinPath = bin
	cfg.KernelImage = kernel
	cfg.RootFS = rootfs
	cfg.WorkDir = t.TempDir()
	cfg.VCPU = 1
	cfg.MemSizeMiB = 256
	cfg.LogFile = vmResLog(cfg.WorkDir)
	cfg.StartTimeout = 20 * time.Second
	cfg.StopTimeout = 10 * time.Second
	cfg.StopGraceTimeout = 5 * time.Second

	vm, err := New(cfg, nil)
	require.NoError(t, err)
	require.Equal(t, StateCreated, vm.State())

	// Start: Created -> Preparing -> Running.
	require.NoError(t, vm.Start(ctx))
	require.Equal(t, StateRunning, vm.State())
	require.NotZero(t, vm.PID())

	// The Firecracker log forwards the guest serial console; assert Alpine
	// actually booted.
	requireGuestBooted(t, vm)

	// Pause + resume: Running -> Paused -> Running.
	require.NoError(t, vm.Pause(ctx))
	require.Equal(t, StatePaused, vm.State())
	require.NoError(t, vm.Resume(ctx))
	require.Equal(t, StateRunning, vm.State())

	// Stop: Running -> Stopping -> Stopped.
	require.NoError(t, vm.Stop(ctx))
	require.Equal(t, StateStopped, vm.State())

	// Destroy: Stopped -> Destroying -> Destroyed.
	require.NoError(t, vm.Destroy(ctx))
	require.Equal(t, StateDestroyed, vm.State())

	// VM-owned artifacts removed; shared kernel/rootfs preserved.
	for _, p := range []string{kernel, rootfs} {
		if _, err := os.Stat(p); err != nil {
			require.NoError(t, err, "shared artifact %q should still exist", p)
		}
	}
	if _, err := os.Stat(vm.res.APISock); err != nil {
		require.True(t, os.IsNotExist(err))
	} else {
		require.Fail(t, "api socket should have been removed")
	}
	if _, err := os.Stat(vm.res.VsockSock); err != nil {
		require.True(t, os.IsNotExist(err))
	} else {
		require.Fail(t, "vsock socket should have been removed")
	}
}

func TestIntegrationSnapshot(t *testing.T) {
	bin, kernel, rootfs, ok := integrationAssets()
	if !ok {
		t.Skip("integration assets not present")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	cfg := DefaultConfig()
	cfg.BinPath = bin
	cfg.KernelImage = kernel
	cfg.RootFS = rootfs
	cfg.WorkDir = t.TempDir()
	cfg.VCPU = 1
	cfg.MemSizeMiB = 256
	cfg.LogFile = vmResLog(cfg.WorkDir)
	cfg.StartTimeout = 20 * time.Second
	cfg.StopTimeout = 10 * time.Second
	cfg.StopGraceTimeout = 5 * time.Second

	vm, err := New(cfg, nil)
	require.NoError(t, err)
	require.NoError(t, vm.Start(ctx))
	require.Equal(t, StateRunning, vm.State())
	requireGuestBooted(t, vm)

	// Snapshot requires a paused VM.
	require.NoError(t, vm.Pause(ctx))
	require.Equal(t, StatePaused, vm.State())

	snap, err := vm.CreateSnapshot(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, snap.ID)
	require.Equal(t, vm.ID(), snap.SourceVM)
	require.Equal(t, "Paused", snap.SourceState)
	for _, p := range []string{snap.SnapshotPath, snap.MemFilePath} {
		if _, err := os.Stat(p); err != nil {
			require.NoError(t, err, "snapshot artifact %q should exist", p)
		}
	}
	require.Greater(t, snap.CreatedAt.Unix(), int64(0))

	require.NoError(t, vm.Resume(ctx))
	require.Equal(t, StateRunning, vm.State())
	require.NoError(t, vm.Stop(ctx))
	require.Equal(t, StateStopped, vm.State())
	require.NoError(t, vm.Destroy(ctx))
	require.Equal(t, StateDestroyed, vm.State())
}

// vmResLog returns the Firecracker log path for a VM whose work dir is base.
// The per-VM dir is base/<id>, so the log lives at base/firecracker.log (a
// sibling of the VM dir, not inside it).
func vmResLog(base string) string {
	return base + "/firecracker.log"
}

// TestRestoreSnapshotTransport verifies that RestoreSnapshot binds a
// host-side vsock transport on the restored VM, using the restored VM's
// unique vsock socket path rather than the source VM's path.
func TestRestoreSnapshotTransport(t *testing.T) {
	bin, kernel, rootfs, ok := integrationAssets()
	if !ok {
		t.Skip("integration assets not present")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	cfg := DefaultConfig()
	cfg.BinPath = bin
	cfg.KernelImage = kernel
	cfg.RootFS = rootfs
	cfg.WorkDir = t.TempDir()
	cfg.VCPU = 1
	cfg.MemSizeMiB = 256
	cfg.LogFile = vmResLog(cfg.WorkDir)
	cfg.StartTimeout = 20 * time.Second
	cfg.StopTimeout = 10 * time.Second
	cfg.StopGraceTimeout = 5 * time.Second

	source, err := New(cfg, nil)
	require.NoError(t, err)
	require.NoError(t, source.Start(ctx))
	requireGuestBooted(t, source)

	require.NoError(t, source.Pause(ctx))
	require.Equal(t, StatePaused, source.State())

	snap, err := source.CreateSnapshot(ctx)
	require.NoError(t, err)

	require.NoError(t, source.Stop(ctx))
	require.NoError(t, source.Destroy(ctx))

	// Build M6-layout snapshot from the legacy snapshot artifacts.
	store := NewSnapshotStore(filepath.Join(t.TempDir(), "snapshots"))
	m6Snap := &SnapshotInfo{
		ID:           snap.ID,
		SourceVM:     snap.SourceVM,
		SnapshotPath: store.statePath(snap.ID),
		MemFilePath:  store.memPath(snap.ID),
		RootFSPath:   snap.RootFSPath,
		VCPU:         snap.VCPU,
		MemSizeMiB:   snap.MemSizeMiB,
		VsockCID:     snap.VsockCID,
		VsockPort:    snap.VsockPort,
		KernelImage:  snap.KernelImage,
		BootArgs:     snap.BootArgs,
		Status:       snapshotStatusReady,
	}
	snapDir := filepath.Join(store.baseDir, m6Snap.ID)
	require.NoError(t, os.MkdirAll(snapDir, 0o750))
	require.NoError(t, copyFile(snap.SnapshotPath, m6Snap.SnapshotPath))
	require.NoError(t, copyFile(snap.MemFilePath, m6Snap.MemFilePath))
	require.NoError(t, store.Create(m6Snap))

	restored, err := RestoreSnapshot(ctx, store, m6Snap.ID, nil, nil, nil, RestoreOpts{
		WorkDir:  t.TempDir(),
		VsockCID: 6,
	}, nil)
	require.NoError(t, err)
	defer func() { _ = restored.Destroy(context.Background()) }()

	require.Equal(t, StateRunning, restored.State())
	require.NotNil(t, restored.Vsock(), "restored VM should have vsock transport")
	require.NotEqual(t, source.Vsock().SocketPath(), restored.Vsock().SocketPath(),
		"source and restored vsock socket paths must differ")
	require.Equal(t, restored.res.VsockSock, restored.Vsock().SocketPath(),
		"restored transport should use its own vsock socket path")
	require.Equal(t, uint32(6), restored.Vsock().CID(), "restored VM should have requested CID")

	require.NoError(t, restored.Stop(ctx))
}

package runtime

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Bunn-Burrow/bunn-burrow/vmm"
	"github.com/stretchr/testify/require"
)

// newTestRuntime creates a Runtime in a temp directory for testing.
func newTestRuntime(t *testing.T) *Runtime {
	t.Helper()
	dir := t.TempDir()
	r, err := NewRuntime(dir, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		// Best-effort cleanup; Shutdown should be called explicitly in tests.
		_ = r.Shutdown(context.Background())
	})
	return r
}

func TestRuntimeConstruction(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRuntime(dir, nil)
	require.NoError(t, err)
	require.NotNil(t, r)

	// All shared components should be non-nil.
	require.NotNil(t, r.MetadataStore())
	require.NotNil(t, r.LayerManager())
	require.NotNil(t, r.NBDManager())
	require.NotNil(t, r.TemplateStore())
	require.NotNil(t, r.TemplateManager())
	require.NotNil(t, r.ImageManager())
	require.NotNil(t, r.SnapshotStore())
	require.NotNil(t, r.StorageManager())
	require.NotNil(t, r.VolumeManager())
	require.NotNil(t, r.NetworkManager())
	require.NotNil(t, r.SnapshotCoordinator())

	// VM count should be zero initially.
	require.Equal(t, 0, r.VMCount())
}

func TestRuntimeRootDir(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRuntime(dir, nil)
	require.NoError(t, err)
	require.Equal(t, dir, r.RootDir())
}

// TestRuntimeNewVM verifies that NewVM creates VMs with shared dependencies
// injected and that they are tracked.
func TestRuntimeNewVM(t *testing.T) {
	r := newTestRuntime(t)

	vm1, err := r.NewVM(vmm.Config{
		KernelImage: "/dev/null",
		RootFS:      "/dev/null",
		VCPU:        1,
		MemSizeMiB:  256,
	})
	require.NoError(t, err)
	require.NotNil(t, vm1)

	vm2, err := r.NewVM(vmm.Config{
		KernelImage: "/dev/null",
		RootFS:      "/dev/null",
		VCPU:        1,
		MemSizeMiB:  256,
	})
	require.NoError(t, err)
	require.NotNil(t, vm2)

	// VMs should have distinct IDs.
	require.NotEqual(t, vm1.ID(), vm2.ID())

	// Both VMs should share the same dependency instances.
	require.Same(t, r.NetworkManager(), vm1.NetworkManager)
	require.Same(t, r.NetworkManager(), vm2.NetworkManager)
	require.Same(t, r.StorageManager(), vm1.StorageManager)
	require.Same(t, r.StorageManager(), vm2.StorageManager)
	require.Same(t, r.VolumeManager(), vm1.VolumeManager)
	require.Same(t, r.VolumeManager(), vm2.VolumeManager)
	require.Same(t, r.TemplateManager(), vm1.TemplateManager)
	require.Same(t, r.TemplateManager(), vm2.TemplateManager)
	require.Same(t, r.TemplateStore(), vm1.TemplateStore)
	require.Same(t, r.TemplateStore(), vm2.TemplateStore)
	require.Same(t, r.LayerManager(), vm1.LayerMgr)
	require.Same(t, r.LayerManager(), vm2.LayerMgr)
	require.Same(t, r.NBDManager(), vm1.NBDMgr)
	require.Same(t, r.NBDManager(), vm2.NBDMgr)

	// VM count should be 2.
	require.Equal(t, 2, r.VMCount())

	// VMs should be retrievable by ID.
	require.Same(t, vm1, r.VM(vm1.ID()))
	require.Same(t, vm2, r.VM(vm2.ID()))

	// Clean up VMs individually.
	require.NoError(t, vm1.Destroy(context.Background()))
	require.NoError(t, vm2.Destroy(context.Background()))
	require.Equal(t, 0, r.VMCount())
	require.Nil(t, r.VM(vm1.ID()))
	require.Nil(t, r.VM(vm2.ID()))
}

// TestRuntimeShutdown verifies that Shutdown destroys all VMs and releases
// shared resources.
func TestRuntimeShutdown(t *testing.T) {
	r := newTestRuntime(t)

	// Create a few VMs.
	for i := 0; i < 3; i++ {
		_, err := r.NewVM(vmm.Config{
			KernelImage: "/dev/null",
			RootFS:      "/dev/null",
			VCPU:        1,
			MemSizeMiB:  256,
		})
		require.NoError(t, err)
	}
	require.Equal(t, 3, r.VMCount())

	// Shutdown should destroy all VMs and release shared resources.
	require.NoError(t, r.Shutdown(context.Background()))
	require.Equal(t, 0, r.VMCount())

	// Shutdown again should be idempotent.
	require.NoError(t, r.Shutdown(context.Background()))
	require.Equal(t, 0, r.VMCount())
}

// TestRuntimeNewVMAfterShutdown verifies that creating a VM after shutdown
// returns an error.
func TestRuntimeNewVMAfterShutdown(t *testing.T) {
	r := newTestRuntime(t)

	require.NoError(t, r.Shutdown(context.Background()))

	_, err := r.NewVM(vmm.Config{
		KernelImage: "/dev/null",
		RootFS:      "/dev/null",
		VCPU:        1,
		MemSizeMiB:  256,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "runtime is closed")
}

// TestRuntimeVMsIndependent verifies that VMs created by the same Runtime
// have independent identities and directories, even though they share
// dependencies.
func TestRuntimeVMsIndependent(t *testing.T) {
	r := newTestRuntime(t)

	dir := r.RootDir()

	vm1, err := r.NewVM(vmm.Config{
		KernelImage: "/dev/null",
		RootFS:      "/dev/null",
		VCPU:        1,
		MemSizeMiB:  256,
		ID:          "vm-alpha",
		WorkDir:     filepath.Join(dir, "vm-work"),
	})
	require.NoError(t, err)

	vm2, err := r.NewVM(vmm.Config{
		KernelImage: "/dev/null",
		RootFS:      "/dev/null",
		VCPU:        1,
		MemSizeMiB:  256,
		ID:          "vm-beta",
		WorkDir:     filepath.Join(dir, "vm-work"),
	})
	require.NoError(t, err)

	// Different IDs.
	require.NotEqual(t, vm1.ID(), vm2.ID())
	require.NotEqual(t, "", vm1.ID())
	require.NotEqual(t, "", vm2.ID())

	// Different working directories.
	require.NotEqual(t, vm1.Dir(), vm2.Dir())
	require.True(t, filepath.HasPrefix(vm1.Dir(), r.RootDir()))
	require.True(t, filepath.HasPrefix(vm2.Dir(), r.RootDir()))

	// Clean up.
	require.NoError(t, vm1.Destroy(context.Background()))
	require.NoError(t, vm2.Destroy(context.Background()))
}

// TestRuntimeNilLogger verifies that NewRuntime works with a nil logger.
func TestRuntimeNilLogger(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRuntime(dir, nil)
	require.NoError(t, err)
	require.NotNil(t, r)

	vm, err := r.NewVM(vmm.Config{
		KernelImage: "/dev/null",
		RootFS:      "/dev/null",
		VCPU:        1,
		MemSizeMiB:  256,
	})
	require.NoError(t, err)
	require.NotNil(t, vm)
	require.NoError(t, vm.Destroy(context.Background()))
}

// TestRuntimeMetadataStoreShared verifies that all metadata consumers share
// the same MetadataStore instance.
func TestRuntimeMetadataStoreShared(t *testing.T) {
	r := newTestRuntime(t)

	// VolumeManager and StorageManager should share the same MetadataStore.
	require.Same(t, r.MetadataStore(), r.StorageManager().MetadataStore())
	require.Same(t, r.MetadataStore(), r.VolumeManager().MetadataStore())

	// TemplateManager's store should be the same template store.
	require.Same(t, r.TemplateStore(), r.TemplateManager().Store())
}

// TestRuntimeSnapshotCoordinator verifies that the SnapshotCoordinator is
// accessible and shares the same underlying stores.
func TestRuntimeSnapshotCoordinator(t *testing.T) {
	r := newTestRuntime(t)

	sc := r.SnapshotCoordinator()
	require.NotNil(t, sc)

	// The coordinator should share the same snapshot store.
	require.Same(t, r.SnapshotStore(), sc.Store())

	// The same template store and NBD manager should be shared.
	require.Same(t, r.TemplateStore(), sc.TemplateStore())
	require.Same(t, r.NBDManager(), sc.NBDManager())
}

// TestRuntimeEmptyRootDir verifies that NewRuntime rejects an empty root dir.
func TestRuntimeEmptyRootDir(t *testing.T) {
	_, err := NewRuntime("", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "rootDir is required")
}

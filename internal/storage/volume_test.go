package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func nbdaAvailable() bool {
	_, err := os.Stat("/dev/nbd0")
	return err == nil
}

func skipIfNoNBD(t *testing.T) {
	if !nbdaAvailable() {
		t.Skip("NBD devices not available in this environment")
	}
}

func TestVolumeConfigDefaults(t *testing.T) {
	cfg := DefaultVolumeConfig(t.TempDir())
	require.Greater(t, cfg.DefaultSizeBytes, int64(0))
	require.Greater(t, cfg.MaxSizeBytes, cfg.DefaultSizeBytes)
	require.Greater(t, cfg.DefaultSizeBytes, cfg.MinSizeBytes)
}

func newTestVolumeManager(t *testing.T, dir string) *VolumeManager {
	layerMgr := NewLayerManager(dir, nil)
	nbdMgr := NewNBDManager(0, 16, filepath.Join(dir, "nbd"), filepath.Join(dir, "locks"))
	metaStore := NewMetadataStore(dir)
	vm := NewVolumeManager(DefaultVolumeConfig(dir), layerMgr, nbdMgr, metaStore)
	require.NoError(t, vm.EnsureDirs())
	return vm
}

func TestVolumeManagerCreateAndInspect(t *testing.T) {
	dir := t.TempDir()
	vm := newTestVolumeManager(t, dir)
	require.NoError(t, vm.EnsureDirs())

	vol, err := vm.CreateVolume("test-vol-1", 0, nil, nil)
	require.NoError(t, err)
	require.Equal(t, "test-vol-1", vol.ID)
	require.Greater(t, vol.SizeBytes, int64(0))
	require.Equal(t, "qcow2", vol.Format)
	require.Equal(t, VolumeStateActive, vol.State)
	require.NotEmpty(t, vol.Path)
	require.True(t, filepath.IsAbs(vol.Path))
	require.NotZero(t, vol.CreatedAt)

	// Volume image file should exist.
	require.FileExists(t, vol.Path)

	// Inspect should return the same volume.
	inspected, err := vm.InspectVolume("test-vol-1")
	require.NoError(t, err)
	require.NotNil(t, inspected)
	require.Equal(t, vol.ID, inspected.ID)
	require.Equal(t, vol.SizeBytes, inspected.SizeBytes)
	require.Equal(t, vol.State, inspected.State)
}

func TestVolumeManagerCreateDuplicate(t *testing.T) {
	dir := t.TempDir()
	vm := newTestVolumeManager(t, dir)

	_, err := vm.CreateVolume("dup-vol", 0, nil, nil)
	require.NoError(t, err)

	_, err = vm.CreateVolume("dup-vol", 0, nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "already exists")
}

func TestVolumeManagerCreateInvalidID(t *testing.T) {
	dir := t.TempDir()
	vm := newTestVolumeManager(t, dir)

	_, err := vm.CreateVolume("", 0, nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "volume id is required")

	_, err = vm.CreateVolume("../escape", 0, nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "path separators")

	_, err = vm.CreateVolume("..", 0, nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "reserved")
}

func TestVolumeManagerCreateSizeValidation(t *testing.T) {
	dir := t.TempDir()
	vm := newTestVolumeManager(t, dir)

	// Too small.
	_, err := vm.CreateVolume("small-vol", 100, nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "below minimum")

	// Too large.
	_, err = vm.CreateVolume("large-vol", vm.cfg.MaxSizeBytes+1, nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds maximum")

	// Zero uses default.
	vol, err := vm.CreateVolume("default-vol", 0, nil, nil)
	require.NoError(t, err)
	require.Equal(t, vm.cfg.DefaultSizeBytes, vol.SizeBytes)
}

func TestVolumeManagerListVolumes(t *testing.T) {
	dir := t.TempDir()
	vm := newTestVolumeManager(t, dir)

	// Empty list initially.
	vols, err := vm.ListVolumes()
	require.NoError(t, err)
	require.Len(t, vols, 0)

	// Create some volumes.
	_, err = vm.CreateVolume("vol-a", 0, nil, nil)
	require.NoError(t, err)
	_, err = vm.CreateVolume("vol-b", 0, nil, nil)
	require.NoError(t, err)

	vols, err = vm.ListVolumes()
	require.NoError(t, err)
	require.Len(t, vols, 2)

	ids := map[string]bool{}
	for _, v := range vols {
		ids[v.ID] = true
	}
	require.True(t, ids["vol-a"])
	require.True(t, ids["vol-b"])
}

func TestVolumeManagerDeleteVolume(t *testing.T) {
	dir := t.TempDir()
	vm := newTestVolumeManager(t, dir)

	_, err := vm.CreateVolume("del-vol", 0, nil, nil)
	require.NoError(t, err)

	volPath := filepath.Join(dir, "volumes", "del-vol.qcow2")
	require.FileExists(t, volPath)

	// Delete should succeed.
	require.NoError(t, vm.DeleteVolume("del-vol"))

	// Volume should no longer be found.
	inspected, err := vm.InspectVolume("del-vol")
	require.NoError(t, err)
	require.Nil(t, inspected)

	// Volume image should be removed.
	require.NoFileExists(t, volPath)

	// Deleting again should fail (not found).
	err = vm.DeleteVolume("del-vol")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

func TestVolumeManagerDeleteAttachedVolumeRejected(t *testing.T) {
	skipIfNoNBD(t)
	dir := t.TempDir()
	vm := newTestVolumeManager(t, dir)

	_, err := vm.CreateVolume("attach-del-vol", 0, nil, nil)
	require.NoError(t, err)

	// Attach the volume.
	devicePath, err := vm.AttachVolume(context.Background(), "attach-del-vol", "test-vm")
	require.NoError(t, err)
	require.NotEmpty(t, devicePath)

	// Delete should be rejected because it's attached.
	err = vm.DeleteVolume("attach-del-vol")
	require.Error(t, err)
	require.Contains(t, err.Error(), "attached")

	// Detach and delete should work.
	require.NoError(t, vm.DetachVolume("attach-del-vol"))
	require.NoError(t, vm.DeleteVolume("attach-del-vol"))

	inspected, err := vm.InspectVolume("attach-del-vol")
	require.NoError(t, err)
	require.Nil(t, inspected)
}

func TestVolumeManagerAttachDetach(t *testing.T) {
	skipIfNoNBD(t)
	dir := t.TempDir()
	vm := newTestVolumeManager(t, dir)

	_, err := vm.CreateVolume("attach-test", 0, nil, nil)
	require.NoError(t, err)

	// Attach to VM.
	devicePath, err := vm.AttachVolume(context.Background(), "attach-test", "vm-1")
	require.NoError(t, err)
	require.NotEmpty(t, devicePath)

	// Volume state should be attached.
	inspected, err := vm.InspectVolume("attach-test")
	require.NoError(t, err)
	require.Equal(t, VolumeStateAttached, inspected.State)
	require.Equal(t, "vm-1", inspected.AttachedVMID)
	require.Equal(t, devicePath, inspected.AttachedDevice)
	require.NotZero(t, inspected.AttachedAt)

	// GetVolumeForVM should return the volume.
	volForVM, err := vm.GetVolumeForVM("vm-1")
	require.NoError(t, err)
	require.NotNil(t, volForVM)
	require.Equal(t, "attach-test", volForVM.ID)

	// Detach.
	require.NoError(t, vm.DetachVolume("attach-test"))

	// Volume state should be active again.
	inspected, err = vm.InspectVolume("attach-test")
	require.NoError(t, err)
	require.Equal(t, VolumeStateActive, inspected.State)
	require.Equal(t, "", inspected.AttachedVMID)
	require.Equal(t, "", inspected.AttachedDevice)

	// GetVolumeForVM should return nil.
	volForVM, err = vm.GetVolumeForVM("vm-1")
	require.NoError(t, err)
	require.Nil(t, volForVM)
}

func TestVolumeManagerDuplicateAttachRejected(t *testing.T) {
	skipIfNoNBD(t)
	dir := t.TempDir()
	vm := newTestVolumeManager(t, dir)

	_, err := vm.CreateVolume("vol-1", 0, nil, nil)
	require.NoError(t, err)
	_, err = vm.CreateVolume("vol-2", 0, nil, nil)
	require.NoError(t, err)

	// Attach vol-1 to vm-1.
	_, err = vm.AttachVolume(context.Background(), "vol-1", "vm-1")
	require.NoError(t, err)

	// Attaching vol-1 again should fail (already attached).
	_, err = vm.AttachVolume(context.Background(), "vol-1", "vm-1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "already attached")

	// Attaching vol-2 to vm-1 should fail (VM already has a volume).
	_, err = vm.AttachVolume(context.Background(), "vol-2", "vm-1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "already has a volume attached")

	// Detach vol-1.
	require.NoError(t, vm.DetachVolume("vol-1"))

	// Now vol-2 can attach to vm-1.
	_, err = vm.AttachVolume(context.Background(), "vol-2", "vm-1")
	require.NoError(t, err)
}

func TestVolumeManagerDetachNotAttached(t *testing.T) {
	dir := t.TempDir()
	vm := newTestVolumeManager(t, dir)

	_, err := vm.CreateVolume("not-attached", 0, nil, nil)
	require.NoError(t, err)

	err = vm.DetachVolume("not-attached")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not attached")

	err = vm.DetachVolume("nonexistent")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

func TestVolumeManagerDetachVMVolumes(t *testing.T) {
	skipIfNoNBD(t)
	dir := t.TempDir()
	vm := newTestVolumeManager(t, dir)

	_, err := vm.CreateVolume("vm-detach-vol", 0, nil, nil)
	require.NoError(t, err)

	_, err = vm.AttachVolume(context.Background(), "vm-detach-vol", "vm-detach-test")
	require.NoError(t, err)

	// DetachVMVolumes should detach all volumes for the VM.
	require.NoError(t, vm.DetachVMVolumes("vm-detach-test"))

	vol, err := vm.InspectVolume("vm-detach-vol")
	require.NoError(t, err)
	require.Equal(t, VolumeStateActive, vol.State)
	require.Equal(t, "", vol.AttachedVMID)
}

func TestVolumeManagerVolumeWithMetadata(t *testing.T) {
	dir := t.TempDir()
	vm := newTestVolumeManager(t, dir)

	metadata := map[string]string{"description": "test volume", "owner": "user-1"}
	labels := map[string]string{"env": "test", "team": "platform"}

	vol, err := vm.CreateVolume("meta-vol", 0, metadata, labels)
	require.NoError(t, err)
	require.Equal(t, metadata, vol.Metadata)
	require.Equal(t, labels, vol.Labels)

	inspected, err := vm.InspectVolume("meta-vol")
	require.NoError(t, err)
	require.Equal(t, metadata, inspected.Metadata)
	require.Equal(t, labels, inspected.Labels)
}

func TestVolumeManagerRecover(t *testing.T) {
	dir := t.TempDir()
	vm1 := newTestVolumeManager(t, dir)

	_, err := vm1.CreateVolume("recover-vol", 0, nil, nil)
	require.NoError(t, err)

	// Attach before recovery (skip if no NBD).
	if nbdaAvailable() {
		_, err = vm1.AttachVolume(context.Background(), "recover-vol", "recover-vm")
		require.NoError(t, err)
	}

	// Create a new VolumeManager (simulating process restart) and recover.
	vm2 := newTestVolumeManager(t, dir)
	require.NoError(t, vm2.Recover())

	inspected, err := vm2.InspectVolume("recover-vol")
	require.NoError(t, err)
	require.NotNil(t, inspected)
	require.Equal(t, VolumeStateActive, inspected.State)

	// If NBD was available, check attachment was recovered.
	if nbdaAvailable() {
		require.Equal(t, VolumeStateAttached, inspected.State)
		require.Equal(t, "recover-vm", inspected.AttachedVMID)

		// Attachment index should be rebuilt.
		volForVM, err := vm2.GetVolumeForVM("recover-vm")
		require.NoError(t, err)
		require.NotNil(t, volForVM)
		require.Equal(t, "recover-vol", volForVM.ID)
	}
}

func TestVolumeManagerSizeFromFile(t *testing.T) {
	dir := t.TempDir()
	vm := newTestVolumeManager(t, dir)

	// Create a volume with a specific size.
	customSize := int64(5 * 1024 * 1024 * 1024) // 5 GiB
	vol, err := vm.CreateVolume("sized-vol", customSize, nil, nil)
	require.NoError(t, err)
	require.Equal(t, customSize, vol.SizeBytes)

	// Verify the qcow2 file exists.
	info, err := os.Stat(vol.Path)
	require.NoError(t, err)
	_ = info.Size()
}

func TestVolumeManagerAttachNilNBDManager(t *testing.T) {
	dir := t.TempDir()
	layerMgr := NewLayerManager(dir, nil)
	metaStore := NewMetadataStore(dir)
	// nbdMgr is nil.
	vm := NewVolumeManager(DefaultVolumeConfig(dir), layerMgr, nil, metaStore)
	require.NoError(t, vm.EnsureDirs())

	_, err := vm.CreateVolume("no-nbd-vol", 0, nil, nil)
	require.NoError(t, err)

	_, err = vm.AttachVolume(context.Background(), "no-nbd-vol", "test-vm")
	require.Error(t, err)
	require.Contains(t, err.Error(), "nbd manager not configured")
}

func TestVolumeManagerDeleteAttachedFromMetadata(t *testing.T) {
	dir := t.TempDir()
	layerMgr := NewLayerManager(dir, nil)
	nbdMgr := NewNBDManager(0, 16, filepath.Join(dir, "nbd"), filepath.Join(dir, "locks"))
	metaStore := NewMetadataStore(dir)
	vm := NewVolumeManager(DefaultVolumeConfig(dir), layerMgr, nbdMgr, metaStore)
	require.NoError(t, vm.EnsureDirs())

	// Create a volume.
	_, err := vm.CreateVolume("attached-meta-vol", 0, nil, nil)
	require.NoError(t, err)

	// Manually mark it as attached in the metadata store (simulating a crash
	// where NBD mapping was lost but metadata still says attached).
	res := metaStore.Get(volumeResourceID("attached-meta-vol"))
	require.NotNil(t, res)
	res.State = StateActive // VolumeStateAttached maps to StateActive in VolumeLifecycleState
	// We need to set AttachedVMID via the Volume struct, but StorageResource
	// doesn't have that field. Instead, directly manipulate the in-memory
	// volume to be "attached" and test that DeleteVolume rejects it.
	vm.mu.Lock()
	vol := vm.volumes["attached-meta-vol"]
	vol.State = VolumeStateAttached
	vol.AttachedVMID = "some-vm"
	vm.mu.Unlock()

	// Delete should fail because the volume is attached.
	err = vm.DeleteVolume("attached-meta-vol")
	require.Error(t, err)
	require.Contains(t, err.Error(), "attached")

	// Volume should still exist.
	inspected, err := vm.InspectVolume("attached-meta-vol")
	require.NoError(t, err)
	require.NotNil(t, inspected)
	require.Equal(t, VolumeStateAttached, inspected.State)
}

func TestVolumeManagerSizePreservedThroughRecover(t *testing.T) {
	dir := t.TempDir()
	layerMgr := NewLayerManager(dir, nil)
	nbdMgr := NewNBDManager(0, 16, filepath.Join(dir, "nbd"), filepath.Join(dir, "locks"))
	metaStore := NewMetadataStore(dir)

	vm1 := NewVolumeManager(DefaultVolumeConfig(dir), layerMgr, nbdMgr, metaStore)
	require.NoError(t, vm1.EnsureDirs())

	customSize := int64(7 * 1024 * 1024 * 1024) // 7 GiB
	_, err := vm1.CreateVolume("size-recover-vol", customSize, nil, nil)
	require.NoError(t, err)

	// Create a new VolumeManager (simulating process restart) and recover.
	vm2 := NewVolumeManager(DefaultVolumeConfig(dir), layerMgr, nbdMgr, metaStore)
	require.NoError(t, vm2.Recover())

	inspected, err := vm2.InspectVolume("size-recover-vol")
	require.NoError(t, err)
	require.NotNil(t, inspected)
	require.Equal(t, customSize, inspected.SizeBytes)
}

func TestVolumeManagerRecoverDoesNotRebuildAttachments(t *testing.T) {
	dir := t.TempDir()
	layerMgr := NewLayerManager(dir, nil)
	nbdMgr := NewNBDManager(0, 16, filepath.Join(dir, "nbd"), filepath.Join(dir, "locks"))
	metaStore := NewMetadataStore(dir)

	vm1 := NewVolumeManager(DefaultVolumeConfig(dir), layerMgr, nbdMgr, metaStore)
	require.NoError(t, vm1.EnsureDirs())

	_, err := vm1.CreateVolume("no-attach-recover", 0, nil, nil)
	require.NoError(t, err)

	// Mark as attached in memory only (not persisted via NBD).
	vm1.mu.Lock()
	vol := vm1.volumes["no-attach-recover"]
	vol.State = VolumeStateAttached
	vol.AttachedVMID = "crash-vm"
	vm1.mu.Unlock()

	// Create a new VolumeManager and recover.
	vm2 := NewVolumeManager(DefaultVolumeConfig(dir), layerMgr, nbdMgr, metaStore)
	require.NoError(t, vm2.Recover())

	// Attachment index should NOT be rebuilt from metadata.
	volForVM, err := vm2.GetVolumeForVM("crash-vm")
	require.NoError(t, err)
	require.Nil(t, volForVM)

	// Volume should be active (not attached) after recovery.
	inspected, err := vm2.InspectVolume("no-attach-recover")
	require.NoError(t, err)
	require.NotNil(t, inspected)
	require.Equal(t, VolumeStateActive, inspected.State)
	require.Equal(t, "", inspected.AttachedVMID)
}

package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Bunn-Burrow/bunn-burrow/internal/template"
	"github.com/stretchr/testify/require"
)

// mockSnapshotStore implements SnapshotStore for tests.
type mockSnapshotStore struct {
	snaps map[string]*SnapshotRef
}

func newMockSnapshotStore() *mockSnapshotStore {
	return &mockSnapshotStore{snaps: make(map[string]*SnapshotRef)}
}

func (m *mockSnapshotStore) Get(id string) (*SnapshotRef, bool) {
	s, ok := m.snaps[id]
	return s, ok
}

func (m *mockSnapshotStore) List() ([]*SnapshotRef, error) {
	var out []*SnapshotRef
	for _, s := range m.snaps {
		out = append(out, s)
	}
	return out, nil
}

func (m *mockSnapshotStore) Delete(id string) error {
	delete(m.snaps, id)
	return nil
}

func TestStorageManagerRegisterReleaseVM(t *testing.T) {
	rootDir := t.TempDir()

	tmplStore := template.New(rootDir)
	require.NoError(t, tmplStore.EnsureDirs())

	layerMgr := NewLayerManager(rootDir, tmplStore)
	require.NoError(t, layerMgr.EnsureDirs())

	nbdMgr := NewNBDManager(0, 16, "/dev", "/var/lock")

	sm := NewStorageManager(rootDir, layerMgr, nbdMgr, nil, tmplStore)
	require.NoError(t, sm.EnsureDirs())

	vmID := "test-vm"
	overlayPath := filepath.Join(rootDir, "layers", vmID+".qcow2")
	nbdDevice := "/dev/nbd0"

	// Register VM without a template (empty templateID avoids template ref counting).
	err := sm.RegisterVM(vmID, "", overlayPath, nbdDevice)
	require.NoError(t, err)

	// Verify resources exist.
	vmRes := sm.metaStore.Get("vm:" + vmID)
	require.NotNil(t, vmRes)
	require.Equal(t, StateActive, vmRes.State)
	require.Equal(t, "", vmRes.TemplateID)

	overlayRes := sm.metaStore.Get("overlay:" + vmID)
	require.NotNil(t, overlayRes)
	require.Equal(t, overlayPath, overlayRes.Path)
	require.Equal(t, "", overlayRes.TemplateID)
	require.Equal(t, 1, overlayRes.ReferenceCount)

	// No backing image should exist (no template).
	backingRes := sm.metaStore.Get("backing:" + overlayPath)
	require.Nil(t, backingRes)

	// Release VM.
	err = sm.ReleaseVM(vmID)
	require.NoError(t, err)

	// VM resource should be deleted.
	vmRes = sm.metaStore.Get("vm:" + vmID)
	require.NotNil(t, vmRes)
	require.Equal(t, StateDeleted, vmRes.State)

	// Overlay resource should be deleted.
	overlayRes = sm.metaStore.Get("overlay:" + vmID)
	require.Nil(t, overlayRes)
}

func TestStorageManagerRegisterReleaseVM_Idempotent(t *testing.T) {
	rootDir := t.TempDir()
	tmplStore := template.New(rootDir)
	require.NoError(t, tmplStore.EnsureDirs())

	layerMgr := NewLayerManager(rootDir, tmplStore)
	require.NoError(t, layerMgr.EnsureDirs())
	nbdMgr := NewNBDManager(0, 16, "/dev", "/var/lock")
	sm := NewStorageManager(rootDir, layerMgr, nbdMgr, nil, tmplStore)
	require.NoError(t, sm.EnsureDirs())

	vmID := "test-vm-idempotent"

	// Releasing a non-existent VM should not error.
	require.NoError(t, sm.ReleaseVM(vmID))

	// Register then release twice.
	require.NoError(t, sm.RegisterVM(vmID, "", "/tmp/overlay.qcow2", "/dev/nbd1"))
	require.NoError(t, sm.ReleaseVM(vmID))
	require.NoError(t, sm.ReleaseVM(vmID)) // idempotent
}

func TestStorageManagerSnapshotOverlay(t *testing.T) {
	rootDir := t.TempDir()
	tmplStore := template.New(rootDir)
	require.NoError(t, tmplStore.EnsureDirs())

	layerMgr := NewLayerManager(rootDir, tmplStore)
	require.NoError(t, layerMgr.EnsureDirs())
	nbdMgr := NewNBDManager(0, 16, "/dev", "/var/lock")
	sm := NewStorageManager(rootDir, layerMgr, nbdMgr, nil, tmplStore)
	require.NoError(t, sm.EnsureDirs())

	snapID := "test-snap"
	overlayPath := filepath.Join(rootDir, "snapshots", snapID+".qcow2")
	backingPath := filepath.Join(rootDir, "backing", "tmpl.qcow2")

	// Register snapshot overlay.
	require.NoError(t, sm.RegisterSnapshotOverlay(snapID, "tmpl", overlayPath, backingPath))

	// Verify backing image refcount incremented.
	backingRes := sm.metaStore.Get("backing:" + backingPath)
	require.NotNil(t, backingRes)
	require.Equal(t, 1, backingRes.ReferenceCount)

	// Release snapshot overlay.
	require.NoError(t, sm.ReleaseSnapshotOverlay(snapID))

	// Backing refcount should be 0 and deleted.
	backingRes = sm.metaStore.Get("backing:" + backingPath)
	require.NotNil(t, backingRes)
	require.Equal(t, 0, backingRes.ReferenceCount)
	require.Equal(t, StateDeleted, backingRes.State)
}

func TestStorageManagerDeleteSnapshot_NotFound(t *testing.T) {
	rootDir := t.TempDir()
	tmplStore := template.New(rootDir)
	require.NoError(t, tmplStore.EnsureDirs())

	mockSnapStore := newMockSnapshotStore()
	sm := NewStorageManager(rootDir, nil, nil, mockSnapStore, tmplStore)
	require.NoError(t, sm.EnsureDirs())

	// Deleting a non-existent snapshot should return an error.
	err := sm.DeleteSnapshot(context.Background(), "nonexistent-snap")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

func TestStorageManagerRecoverStuckStates(t *testing.T) {
	rootDir := t.TempDir()
	tmplStore := template.New(rootDir)
	require.NoError(t, tmplStore.EnsureDirs())

	layerMgr := NewLayerManager(rootDir, tmplStore)
	require.NoError(t, layerMgr.EnsureDirs())
	nbdMgr := NewNBDManager(0, 16, "/dev", "/var/lock")
	sm := NewStorageManager(rootDir, layerMgr, nbdMgr, nil, tmplStore)
	require.NoError(t, sm.EnsureDirs())

	// Register a resource stuck in creating state.
	stuckRes := &StorageResource{
		ID:        "stuck-resource",
		Type:      ResourceTypeOverlay,
		State:     StateCreating,
		Path:      "/tmp/stuck.qcow2",
		CreatedAt: time.Now().Add(-time.Hour),
		UpdatedAt: time.Now().Add(-time.Hour),
	}
	require.NoError(t, sm.metaStore.Register(stuckRes))

	// Recover should mark it as failed.
	require.NoError(t, sm.Recover(context.Background()))

	stuckRes = sm.metaStore.Get("stuck-resource")
	require.NotNil(t, stuckRes)
	require.Equal(t, StateFailed, stuckRes.State)
}

func TestStorageManagerCollectGarbageDryRun(t *testing.T) {
	rootDir := t.TempDir()
	tmplStore := template.New(rootDir)
	require.NoError(t, tmplStore.EnsureDirs())

	layerMgr := NewLayerManager(rootDir, tmplStore)
	require.NoError(t, layerMgr.EnsureDirs())
	nbdMgr := NewNBDManager(0, 16, "/dev", "/var/lock")
	sm := NewStorageManager(rootDir, layerMgr, nbdMgr, nil, tmplStore)
	require.NoError(t, sm.EnsureDirs())

	// Register a failed overlay (should be GC candidate).
	failedOverlay := &StorageResource{
		ID:        "failed-overlay",
		Type:      ResourceTypeOverlay,
		Path:      "/tmp/failed.qcow2",
		State:     StateFailed,
		VMID:      "dead-vm",
		CreatedAt: time.Now().Add(-time.Hour),
		UpdatedAt: time.Now().Add(-time.Hour),
	}
	require.NoError(t, sm.metaStore.Register(failedOverlay))

	// Create a temp file in layers dir.
	layersDir := filepath.Join(rootDir, "layers")
	require.NoError(t, os.MkdirAll(layersDir, 0o750))
	tmpFile := filepath.Join(layersDir, "orphan.tmp")
	require.NoError(t, os.WriteFile(tmpFile, []byte("junk"), 0o644))

	report, err := sm.CollectGarbage(context.Background(), GCOptions{
		Mode:            GCModeDryRun,
		DeleteTempFiles: true,
	})
	require.NoError(t, err)
	require.Equal(t, GCModeDryRun, report.Mode)
	require.Contains(t, report.WouldDelete, tmpFile)
	require.Contains(t, report.OrphanedOverlays, "/tmp/failed.qcow2")

	// Verify nothing was actually deleted.
	require.NotContains(t, report.Deleted, tmpFile)
	require.FileExists(t, tmpFile)
}

func TestStorageManagerCollectGarbageExec(t *testing.T) {
	rootDir := t.TempDir()
	tmplStore := template.New(rootDir)
	require.NoError(t, tmplStore.EnsureDirs())

	layerMgr := NewLayerManager(rootDir, tmplStore)
	require.NoError(t, layerMgr.EnsureDirs())
	nbdMgr := NewNBDManager(0, 16, "/dev", "/var/lock")
	sm := NewStorageManager(rootDir, layerMgr, nbdMgr, nil, tmplStore)
	require.NoError(t, sm.EnsureDirs())

	// Create a temp file in layers dir.
	layersDir := filepath.Join(rootDir, "layers")
	require.NoError(t, os.MkdirAll(layersDir, 0o750))
	tmpFile := filepath.Join(layersDir, "orphan.tmp")
	require.NoError(t, os.WriteFile(tmpFile, []byte("junk"), 0o644))

	report, err := sm.CollectGarbage(context.Background(), GCOptions{
		Mode:            GCModeExec,
		DeleteTempFiles: true,
	})
	require.NoError(t, err)
	require.Equal(t, GCModeExec, report.Mode)
	require.Contains(t, report.Deleted, tmpFile)

	// Verify file was deleted.
	require.NoFileExists(t, tmpFile)
}

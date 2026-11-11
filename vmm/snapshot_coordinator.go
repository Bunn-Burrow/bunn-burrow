package vmm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"

	"github.com/omkar/sandbox/internal/storage"
	"github.com/omkar/sandbox/internal/template"
)

// SnapshotConsistencyMode defines the consistency guarantee level for a snapshot.
type SnapshotConsistencyMode string

const (
	// SnapshotConsistencyCrash pauses the VM and captures the Firecracker
	// snapshot without guest coordination. Disk writes are captured as they
	// exist in the host page cache at snapshot time.
	SnapshotConsistencyCrash SnapshotConsistencyMode = "crash_consistent"

	// SnapshotConsistencyFilesystem is not yet implemented. Real filesystem
	// freezing requires guest-side FIFREEZE ioctl support and mount-specific
	// handling that cannot be safely guaranteed across all guest images.
	// sync() alone does not provide filesystem freezing. This mode is deferred
	// until a safe guest-agent freeze/thaw implementation is available.
	SnapshotConsistencyFilesystem SnapshotConsistencyMode = "filesystem_consistent"
)

// SnapshotCoordinator orchestrates VM snapshots with optional guest
// coordination. Currently only crash_consistent mode is implemented.
// filesystem_consistent mode is deferred (see SnapshotConsistencyFilesystem).
type SnapshotCoordinator struct {
	mu        sync.Mutex
	snapStore *SnapshotStore
	layerMgr  *storage.LayerManager
	tmplStore *template.Store
	nbdMgr    *storage.NBDManager
	logger    *logrus.Entry
}

// NewSnapshotCoordinator creates a coordinator for VM snapshots.
func NewSnapshotCoordinator(
	snapStore *SnapshotStore,
	layerMgr *storage.LayerManager,
	tmplStore *template.Store,
	nbdMgr *storage.NBDManager,
	logger *logrus.Entry,
) *SnapshotCoordinator {
	if logger == nil {
		l := logrus.New()
		l.SetOutput(os.Stderr)
		logger = l.WithField("component", "snapshot-coordinator")
	}
	return &SnapshotCoordinator{
		snapStore: snapStore,
		layerMgr:  layerMgr,
		tmplStore: tmplStore,
		nbdMgr:    nbdMgr,
		logger:    logger,
	}
}

// CreateSnapshotWithCoordination creates a snapshot with the specified
// consistency mode. Currently only crash_consistent is implemented.
// filesystem_consistent returns an error (deferred).
//
// Workflow (crash_consistent):
//  1. Verify VM is running or paused
//  2. Pause Firecracker VM if running
//  3. Firecracker CreateSnapshot (state.snap + mem.snap)
//  4. Create self-contained overlay copy (template-based VMs)
//  5. Persist snapshot metadata atomically
//  6. Resume VM if it was running
//
// On any failure, incomplete artifacts are cleaned up and the snapshot is
// NOT marked as ready.
func (sc *SnapshotCoordinator) CreateSnapshotWithCoordination(
	ctx context.Context,
	vm *VM,
	mode SnapshotConsistencyMode,
	snapRoot string,
) (*SnapshotInfo, error) {

	if vm == nil {
		return nil, errors.New("vm is required")
	}
	if sc.snapStore == nil {
		return nil, errors.New("snapshot store not configured")
	}
	if err := sc.snapStore.EnsureDirs(); err != nil {
		return nil, fmt.Errorf("ensure snapshot dirs: %w", err)
	}

	if mode == SnapshotConsistencyFilesystem {
		return nil, fmt.Errorf("filesystem_consistent snapshot mode is not implemented: guest freeze/thaw is deferred")
	}

	// Verify VM is in a state that can be snapshotted.
	state := vm.State()
	if state != StateRunning && state != StatePaused {
		return nil, fmt.Errorf("vm %q must be running or paused for snapshot, got %q", vm.id, state)
	}
	wasRunning := state == StateRunning

	reqID := uuid.NewString()
	sc.logger.Info("snapshot coordination started",
		map[string]any{"vm_id": vm.id, "snapshot_id": reqID, "mode": mode, "was_running": wasRunning})

	// Track whether we need to resume on failure.
	vmPaused := false
	defer func() {
		// On failure, attempt safe recovery: resume VM if we paused it.
		if vmPaused && !wasRunning {
			_ = vm.Resume(context.Background())
		}
	}()

	// Step 1: Pause the VM if it's running.
	if wasRunning {
		if err := vm.Pause(ctx); err != nil {
			return nil, fmt.Errorf("pause vm: %w", err)
		}
		vmPaused = true
	}

	// Step 2: Firecracker CreateSnapshot.
	snapID := uuid.NewString()
	snapDir := sc.snapStore.snapshotDir(snapID)
	if err := os.MkdirAll(snapDir, 0o750); err != nil {
		return nil, fmt.Errorf("create snapshot dir: %w", err)
	}

	snapshotPath := sc.snapStore.statePath(snapID)
	memFilePath := sc.snapStore.memPath(snapID)

	params := &models.SnapshotCreateParams{
		SnapshotPath: &snapshotPath,
		MemFilePath:  &memFilePath,
	}
	if err := vm.api.CreateSnapshot(ctx, params); err != nil {
		return nil, fmt.Errorf("create snapshot: %w", err)
	}

	// Validate Firecracker artifacts.
	for _, p := range []string{snapshotPath, memFilePath} {
		if err := validateArtifact(p); err != nil {
			_ = os.Remove(snapshotPath)
			_ = os.Remove(memFilePath)
			return nil, fmt.Errorf("snapshot artifact incomplete %q: %w", p, err)
		}
	}

	version := ""
	if info, err := vm.api.GetInstanceInfo(ctx); err == nil && info.VmmVersion != nil {
		version = *info.VmmVersion
	}

	// Step 3: Create self-contained overlay copy (template-based VMs).
	info := &SnapshotInfo{
		ID:                 snapID,
		SourceVM:           vm.id,
		SnapshotPath:       snapshotPath,
		MemFilePath:        memFilePath,
		RootFSPath:         vm.cfg.RootFS,
		FirecrackerVersion: version,
		CreatedAt:          time.Now(),
		SourceState:        "Paused",
		Status:             "creating",
		MetadataVersion:    snapshotMetadataVersion,
		VCPU:               vm.cfg.VCPU,
		MemSizeMiB:         vm.cfg.MemSizeMiB,
		VsockCID:           vm.cfg.VsockCID,
		VsockPort:          vm.cfg.VsockPort,
		KernelImage:        vm.cfg.KernelImage,
		BootArgs:           vm.cfg.BootArgs,
		ConsistencyMode:    string(mode),
		RequestID:          reqID,
	}

	if vm.TemplateStore != nil && vm.LayerMgr != nil && vm.cfg.TemplateID != "" {
		layerInfo := vm.LayerMgr.LayerMetadata(vm.id)
		if layerInfo != nil {
			info.TemplateID = layerInfo.TemplateID
			info.BackingImagePath = layerInfo.BackingImagePath
			info.StorageFormat = layerInfo.Format

			snapOverlayPath := filepath.Join(snapDir, snapID+".qcow2")
			tmpOverlay := snapOverlayPath + ".tmp"

			cmd := exec.Command("qemu-img", "convert", "-O", "qcow2", layerInfo.LayerPath, tmpOverlay)
			if out, err := cmd.CombinedOutput(); err != nil {
				_ = os.Remove(snapshotPath)
				_ = os.Remove(memFilePath)
				_ = os.Remove(tmpOverlay)
				return nil, fmt.Errorf("copy overlay for snapshot: %w\noutput: %s", err, string(out))
			}
			if err := os.Rename(tmpOverlay, snapOverlayPath); err != nil {
				_ = os.Remove(snapshotPath)
				_ = os.Remove(memFilePath)
				_ = os.Remove(tmpOverlay)
				return nil, fmt.Errorf("rename snapshot overlay: %w", err)
			}
			info.OverlayPath = snapOverlayPath
			info.NBDDevice = vm.nbdDevice
		}
	}

	// Step 4: Persist snapshot metadata atomically.
	if err := sc.snapStore.Create(info); err != nil {
		_ = os.Remove(snapshotPath)
		_ = os.Remove(memFilePath)
		if info.OverlayPath != "" {
			_ = os.Remove(info.OverlayPath)
		}
		return nil, err
	}

	// Step 5: Resume VM if it was running.
	if wasRunning {
		if err := vm.Resume(ctx); err != nil {
			// Snapshot was created successfully but resume failed.
			vmPaused = false // don't resume again in deferred cleanup
			return nil, fmt.Errorf("resume vm after snapshot: %w", err)
		}
		vmPaused = false // already resumed
	}

	info.Status = snapshotStatusReady
	// Re-write metadata with final status.
	if err := sc.snapStore.Create(info); err != nil {
		sc.logger.Warn("update snapshot status failed",
			map[string]any{"vm_id": vm.id, "snapshot_id": snapID, "error": err.Error()})
	}

	sc.logger.Info("snapshot coordination complete",
		map[string]any{"vm_id": vm.id, "snapshot_id": snapID, "mode": mode, "status": info.Status})

	return info, nil
}

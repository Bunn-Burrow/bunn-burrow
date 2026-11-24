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

	"github.com/Bunn-Burrow/bunn-burrow/internal/storage"
	"github.com/Bunn-Burrow/bunn-burrow/internal/template"
)

// SnapshotConsistencyMode defines the consistency guarantee level for a snapshot.
type SnapshotConsistencyMode string

const (
	// SnapshotConsistencyCrash pauses the VM and captures the Firecracker
	// snapshot without guest coordination. Disk writes are captured as they
	// exist in the host page cache at snapshot time.
	SnapshotConsistencyCrash SnapshotConsistencyMode = "crash_consistent"

	// SnapshotConsistencyFilesystem coordinates with the guest agent to flush
	// and freeze filesystems before Firecracker captures the snapshot, then
	// thaws afterwards. This provides filesystem-consistent snapshots.
	SnapshotConsistencyFilesystem SnapshotConsistencyMode = "filesystem_consistent"
)

// SnapshotCoordinator orchestrates VM snapshots with optional guest
// coordination.
type SnapshotCoordinator struct {
	mu        sync.Mutex
	snapStore *SnapshotStore
	layerMgr  *storage.LayerManager
	tmplStore *template.Store
	nbdMgr    storage.NBDManager
	logger    *logrus.Entry
}

// Store returns the underlying SnapshotStore.
func (sc *SnapshotCoordinator) Store() *SnapshotStore {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.snapStore
}

// TemplateStore returns the underlying template store.
func (sc *SnapshotCoordinator) TemplateStore() *template.Store {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.tmplStore
}

// NBDManager returns the underlying NBD manager.
func (sc *SnapshotCoordinator) NBDManager() storage.NBDManager {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.nbdMgr
}

// NewSnapshotCoordinator creates a coordinator for VM snapshots.
func NewSnapshotCoordinator(
	snapStore *SnapshotStore,
	layerMgr *storage.LayerManager,
	tmplStore *template.Store,
	nbdMgr storage.NBDManager,
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
// consistency mode.
//
// Workflow (crash_consistent):
//  1. Verify VM is running or paused
//  2. Pause Firecracker VM if running
//  3. Firecracker CreateSnapshot (state.snap + mem.snap)
//  4. Create self-contained overlay copy (template-based VMs)
//  5. Persist snapshot metadata atomically
//  6. Resume VM if it was running
//
// Workflow (filesystem_consistent):
//  1. Verify VM is running or paused
//  2. Pause Firecracker VM if running
//  3. Guest agent: prepare_snapshot → sync_filesystems → freeze_filesystems
//  4. Firecracker CreateSnapshot (state.snap + mem.snap)
//  5. Guest agent: thaw_filesystems (guaranteed even if step 4 fails)
//  6. Guest agent: snapshot_ready
//  7. Create self-contained overlay copy (template-based VMs)
//  8. Persist snapshot metadata atomically
//  9. Resume VM if it was running
//
// On any failure, incomplete artifacts are cleaned up, the guest filesystem
// is thawed if it was frozen, and the VM is resumed if it was paused by this
// call. The snapshot is NOT marked as ready.
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

	// Verify VM is in a state that can be snapshotted.
	state := vm.State()
	if state != StateRunning && state != StatePaused {
		return nil, fmt.Errorf("vm %q must be running or paused for snapshot, got %q", vm.id, state)
	}
	wasRunning := state == StateRunning

	if err := vm.sm.Transition(StatePaused, StateSnapshotting); err != nil {
		return nil, fmt.Errorf("snapshot requires VM %q to be paused: %w", vm.id, err)
	}
	defer func() {
		_ = vm.sm.Transition(StateSnapshotting, StatePaused)
	}()

	reqID := uuid.NewString()
	sc.logger.Info("snapshot coordination started",
		map[string]any{"vm_id": vm.id, "snapshot_id": reqID, "mode": mode, "was_running": wasRunning})

	// Track whether we paused the VM (so we can resume on failure).
	vmPaused := false
	// Track whether guest filesystems are frozen (so we can thaw on failure).
	frozen := false
	defer func() {
		// Guarantee thaw if we froze the guest filesystems, even on failure.
		// Use context.Background() so thaw is not blocked by caller cancellation.
		if frozen {
			if err := sc.thawGuestFilesystems(context.Background(), vm, reqID); err != nil {
				sc.logger.Warn("failed to thaw guest filesystems",
					map[string]any{"vm_id": vm.id, "snapshot_id": reqID, "error": err.Error()})
			}
		}
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

	// Step 2: Guest filesystem coordination (filesystem_consistent mode only).
	if mode == SnapshotConsistencyFilesystem {
		if vm.Vsock() == nil {
			return nil, fmt.Errorf("vm %q has no vsock transport for guest coordination", vm.id)
		}
		if _, err := sc.sendGuestRequest(ctx, vm, map[string]any{
			"kind":       "prepare_snapshot",
			"request_id": reqID,
		}); err != nil {
			return nil, fmt.Errorf("guest prepare snapshot: %w", err)
		}
		if _, err := sc.sendGuestRequest(ctx, vm, map[string]any{
			"kind":       "sync_filesystems",
			"request_id": reqID,
		}); err != nil {
			return nil, fmt.Errorf("guest sync filesystems: %w", err)
		}
		if _, err := sc.sendGuestRequest(ctx, vm, map[string]any{
			"kind":       "freeze_filesystems",
			"request_id": reqID,
		}); err != nil {
			return nil, fmt.Errorf("guest freeze filesystems: %w", err)
		}
		frozen = true
	}

	// Step 3: Firecracker CreateSnapshot.
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

	// Step 4: Guest snapshot_ready acknowledgement (filesystem_consistent mode only).
	if mode == SnapshotConsistencyFilesystem {
		if _, err := sc.sendGuestRequest(ctx, vm, map[string]any{
			"kind":       "snapshot_ready",
			"request_id": reqID,
		}); err != nil {
			return nil, fmt.Errorf("guest snapshot ready: %w", err)
		}
	}

	// Step 5: Build SnapshotInfo.
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
	}
	// RequestID is only set for filesystem_consistent mode (guest coordination).
	// Crash-consistent snapshots don't coordinate with the guest, so RequestID
	// remains empty.
	if mode == SnapshotConsistencyFilesystem {
		info.RequestID = reqID
	}

	// Step 6: Create self-contained overlay copy (template-based VMs).
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

	// Step 7: Persist snapshot metadata atomically.
	if err := sc.snapStore.Create(info); err != nil {
		_ = os.Remove(snapshotPath)
		_ = os.Remove(memFilePath)
		if info.OverlayPath != "" {
			_ = os.Remove(info.OverlayPath)
		}
		return nil, err
	}

	// Step 8: Resume VM if it was running.
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

// sendGuestRequest sends a control request to the guest agent over vsock and
// returns the parsed response. The connection is closed after the request.
// If the guest agent returns an error response, sendGuestRequest returns a
// formatted error including the guest error code and message.
func (sc *SnapshotCoordinator) sendGuestRequest(
	ctx context.Context, vm *VM, req map[string]any,
) (map[string]any, error) {
	resp, err := vm.Vsock().SendRequest(ctx, GuestAgentVsockPort, req)
	if err != nil {
		return nil, err
	}
	if kind, _ := resp["kind"].(string); kind == "error" {
		msg, _ := resp["message"].(string)
		code, _ := resp["code"].(float64)
		return nil, fmt.Errorf("guest request %q failed (code %v): %s",
			req["kind"], code, msg)
	}
	return resp, nil
}

// thawGuestFilesystems sends thaw_filesystems to the guest agent to release a
// previously frozen filesystem. It is safe to call even if the guest is not
// frozen (the guest agent will return an error which is logged but not
// propagated).
func (sc *SnapshotCoordinator) thawGuestFilesystems(
	ctx context.Context, vm *VM, reqID string,
) error {
	_, err := sc.sendGuestRequest(ctx, vm, map[string]any{
		"kind":       "thaw_filesystems",
		"request_id": reqID,
	})
	return err
}

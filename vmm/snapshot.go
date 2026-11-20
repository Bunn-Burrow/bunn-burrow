package vmm

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// SnapshotInfo records the metadata and artifact locations for a snapshot.
// Firecracker snapshots capture guest memory and microVM device state; they do
// NOT capture root filesystem disk writes. The disk overlay must be preserved
// separately and is associated with the snapshot via OverlayPath.
type SnapshotInfo struct {
	// ID is the unique snapshot identifier.
	ID string `json:"id"`

	// SourceVM is the ID of the VM the snapshot was taken from.
	SourceVM string `json:"source_vm"`

	// SnapshotPath is the path to the microVM state file.
	SnapshotPath string `json:"snapshot_path"`

	// MemFilePath is the path to the guest memory file.
	MemFilePath string `json:"mem_file_path"`

	// OverlayPath is the path to the VM-private writable overlay associated
	// with this snapshot. For template-based VMs this is a self-contained copy
	// of the VM's overlay created at snapshot time.
	OverlayPath string `json:"overlay_path"`

	// TemplateID is the template the overlay is derived from, if any.
	TemplateID string `json:"template_id"`

	// BackingImagePath is the template's immutable qcow2 backing image, if any.
	BackingImagePath string `json:"backing_image_path"`

	// NBDDevice is the NBD device path (e.g. /dev/nbd0) the overlay was mapped
	// to when the snapshot was taken. Restore must map the overlay to the same
	// device path because Firecracker bakes the drive path into the snapshot.
	NBDDevice string `json:"nbd_device"`

	// RootFSPath is the root filesystem path used when the snapshot was taken
	// (NBD device path for template-based VMs, raw path for direct-rootfs VMs).
	RootFSPath string `json:"rootfs_path"`

	// StorageFormat is the overlay image format ("qcow2" or "raw").
	StorageFormat string `json:"storage_format"`

	// FirecrackerVersion is the Firecracker version active at snapshot time.
	FirecrackerVersion string `json:"firecracker_version"`

	// CreatedAt is the time the snapshot was created.
	CreatedAt time.Time `json:"created_at"`

	// SourceState is the Firecracker instance state when snapshotted ("Paused").
	SourceState string `json:"source_state"`

	// Status is the snapshot lifecycle status ("creating", "ready", "failed").
	Status string `json:"status"`

	// MetadataVersion is the snapshot metadata schema version.
	MetadataVersion string `json:"metadata_version"`

	// VM configuration captured at snapshot time.
	VCPU        int64  `json:"vcpu"`
	MemSizeMiB  int64  `json:"mem_size_mib"`
	VsockCID    uint32 `json:"vsock_cid"`
	VsockPort   uint32 `json:"vsock_port"`
	KernelImage string `json:"kernel_image"`
	BootArgs    string `json:"boot_args"`

	// ConsistencyMode is the snapshot consistency guarantee level
	// ("crash_consistent" or "filesystem_consistent").
	ConsistencyMode string `json:"consistency_mode"`

	// RequestID is the coordination request ID used for guest snapshot
	// coordination (empty for crash_consistent snapshots).
	RequestID string `json:"request_id"`

	// ArtifactChecksums maps artifact file paths to their hex-encoded SHA-256
	// checksums, computed at snapshot creation time. Used to verify artifact
	// integrity before restore. Empty for legacy snapshots created before
	// checksums were introduced; such snapshots skip checksum verification.
	ArtifactChecksums map[string]string `json:"artifact_checksums,omitempty"`
}

const (
	// snapshotMetadataVersion is the current snapshot metadata schema version.
	snapshotMetadataVersion = "1"

	// snapshotStatusReady marks a snapshot whose artifacts are complete and validated.
	snapshotStatusReady = "ready"

	// M6 snapshot directory layout: <snapRoot>/<snapID>/
	snapshotStateFile    = "state.snap"
	snapshotMemFile      = "mem.snap"
	snapshotMetadataFile = "metadata.json"
)

// snapshotStoreDir returns the directory holding snapshot artifacts for a VM
// under the given store root: <root>/snapshots/<vmID>. This is the legacy M5
// layout used by Fork/restoreVM.
func snapshotStoreDir(base, vmID string) string {
	return filepath.Join(base, "snapshots", vmID)
}

// m6SnapshotDir returns the directory for a snapshot under the M6 layout:
// <snapRoot>/<snapID>/.
func m6SnapshotDir(snapRoot, snapID string) string {
	return filepath.Join(snapRoot, snapID)
}

// snapshotCleanup tracks resources acquired during snapshot create, restore,
// and fork operations and releases them in reverse acquisition order on failure.
// It is safe to call Cleanup multiple times; subsequent calls are no-ops.
type snapshotCleanup struct {
	vm *VM

	// Snapshot artifact files (creation paths)
	snapPath    string
	memPath     string
	overlayPath string
	tmpOverlay  string
	snapDir     string

	// VM-level resources (restore/fork paths)
	procStarted  bool
	transportSet bool
	nbdMapped    bool
	destroyVM    bool

	mu   sync.Mutex
	done bool
}

func newSnapshotCleanup(vm *VM) *snapshotCleanup {
	return &snapshotCleanup{vm: vm}
}

func (c *snapshotCleanup) trackSnapshot(snapPath, memPath, snapDir string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snapPath = snapPath
	c.memPath = memPath
	c.snapDir = snapDir
}

func (c *snapshotCleanup) trackOverlay(overlayPath, tmpOverlay string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.overlayPath = overlayPath
	c.tmpOverlay = tmpOverlay
}

func (c *snapshotCleanup) trackRestore(procStarted, transportSet, nbdMapped, destroyVM bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.procStarted = procStarted
	c.transportSet = transportSet
	c.nbdMapped = nbdMapped
	c.destroyVM = destroyVM
}

// Cleanup releases all tracked resources in reverse acquisition order.
// It is idempotent: subsequent calls after the first are no-ops.
// Cleanup errors are collected and returned via errors.Join; they are never
// silently ignored.
func (c *snapshotCleanup) Cleanup() error {
	c.mu.Lock()
	if c.done {
		c.mu.Unlock()
		return nil
	}
	c.done = true
	c.mu.Unlock()

	var errs []error

	// Reverse acquisition order:

	// 1. Undo transport binding (unless full VM destroy will handle it).
	if c.transportSet && c.vm != nil && !c.destroyVM {
		c.vm.mu.Lock()
		c.vm.transport = nil
		c.vm.mu.Unlock()
	}

	// 2. Kill Firecracker process (unless full VM destroy will handle it).
	if c.procStarted && c.vm != nil && !c.destroyVM {
		if proc := c.vm.process(); proc != nil {
			_ = proc.Kill()
		}
	}

	// 3. Full VM destroy (handles process, transport, NBD, sockets, work dir,
	// writable layer, image, network manager).
	if c.destroyVM && c.vm != nil {
		if err := c.vm.destroy(context.Background(), true); err != nil {
			errs = append(errs, err)
		}
	} else if c.nbdMapped && c.vm != nil {
		// Just unmap NBD without full destroy.
		if err := c.vm.unmapImage(); err != nil {
			errs = append(errs, err)
		}
	}

	// 4. Remove overlay files (not covered by vm.destroy).
	if c.overlayPath != "" {
		if err := os.Remove(c.overlayPath); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove overlay %q: %w", c.overlayPath, err))
		}
	}
	if c.tmpOverlay != "" {
		if err := os.Remove(c.tmpOverlay); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove temp overlay %q: %w", c.tmpOverlay, err))
		}
	}

	// 5. Remove snapshot state and memory artifacts.
	if c.snapPath != "" {
		if err := os.Remove(c.snapPath); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove snapshot state %q: %w", c.snapPath, err))
		}
	}
	if c.memPath != "" {
		if err := os.Remove(c.memPath); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove snapshot memory %q: %w", c.memPath, err))
		}
	}

	// 6. Remove snapshot directory (if empty after artifact removal).
	if c.snapDir != "" {
		if err := os.Remove(c.snapDir); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove snapshot dir %q: %w", c.snapDir, err))
		}
	}

	return errors.Join(errs...)
}

// CreateSnapshot snapshots the VM. Firecracker only supports snapshotting a
// paused VM, so the VM must be in the Paused host state. Two artifacts are
// produced by Firecracker: the microVM state file and the guest memory file.
// Incomplete artifacts are cleaned up on failure.
//
// This is the legacy M5 entry point; snapshots are stored under the VM's
// work-dir base using the legacy layout. Use CreateSnapshotWithStore for the
// M6 layout with durable metadata and overlay copy.
func (vm *VM) CreateSnapshot(ctx context.Context) (*SnapshotInfo, error) {
	return vm.createSnapshotLegacy(ctx)
}

func (vm *VM) createSnapshotLegacy(ctx context.Context) (*SnapshotInfo, error) {
	if err := vm.sm.Transition(StatePaused, StateSnapshotting); err != nil {
		return nil, fmt.Errorf("snapshot requires VM %q to be paused: %w", vm.id, err)
	}
	defer func() {
		_ = vm.sm.Transition(StateSnapshotting, StatePaused)
	}()

	cleanup := newSnapshotCleanup(vm)

	snapDir := snapshotStoreDir(filepath.Dir(vm.res.Dir), vm.id)
	if err := os.MkdirAll(snapDir, 0750); err != nil {
		return nil, fmt.Errorf("create snapshot dir: %w", err)
	}

	snapID := uuid.NewString()
	snapshotPath := filepath.Join(snapDir, snapID+".snapshot")
	memFilePath := filepath.Join(snapDir, snapID+".mem")
	cleanup.trackSnapshot(snapshotPath, memFilePath, snapDir)

	params := &models.SnapshotCreateParams{
		SnapshotPath: &snapshotPath,
		MemFilePath:  &memFilePath,
	}
	if err := vm.api.CreateSnapshot(ctx, params); err != nil {
		if cerr := cleanup.Cleanup(); cerr != nil {
			vm.logger.WithError(cerr).Warn("snapshot cleanup failed")
		}
		return nil, fmt.Errorf("create snapshot: %w", err)
	}

	for _, p := range []string{snapshotPath, memFilePath} {
		if err := validateArtifact(p); err != nil {
			if cerr := cleanup.Cleanup(); cerr != nil {
				vm.logger.WithError(cerr).Warn("snapshot cleanup failed")
			}
			return nil, fmt.Errorf("snapshot artifact incomplete %q: %w", p, err)
		}
	}

	// Compute SHA-256 checksums for all snapshot artifacts so they can be
	// verified before restore.
	checksums := make(map[string]string)
	for _, p := range []string{snapshotPath, memFilePath} {
		sum, err := computeChecksum(p)
		if err != nil {
			if cerr := cleanup.Cleanup(); cerr != nil {
				vm.logger.WithError(cerr).Warn("snapshot cleanup failed")
			}
			return nil, fmt.Errorf("compute checksum for %q: %w", p, err)
		}
		checksums[p] = sum
	}

	version := ""
	if info, err := vm.api.GetInstanceInfo(ctx); err == nil && info.VmmVersion != nil {
		version = *info.VmmVersion
	}

	info := &SnapshotInfo{
		ID:                 snapID,
		SourceVM:           vm.id,
		SnapshotPath:       snapshotPath,
		MemFilePath:        memFilePath,
		RootFSPath:         vm.cfg.RootFS,
		FirecrackerVersion: version,
		CreatedAt:          time.Now(),
		SourceState:        "Paused",
		Status:             snapshotStatusReady,
		MetadataVersion:    snapshotMetadataVersion,
		VCPU:               vm.cfg.VCPU,
		MemSizeMiB:         vm.cfg.MemSizeMiB,
		VsockCID:           vm.cfg.VsockCID,
		VsockPort:          vm.cfg.VsockPort,
		KernelImage:        vm.cfg.KernelImage,
		BootArgs:           vm.cfg.BootArgs,
		ArtifactChecksums:  checksums,
	}
	vm.mu.Lock()
	vm.snapshot = info
	vm.mu.Unlock()
	return info, nil
}

// CreateSnapshotWithStore creates an M6 snapshot with durable metadata and a
// self-contained overlay copy. The snapshot is stored under
// <snapRoot>/<snapID>/ with metadata.json, state.snap, mem.snap, and (for
// template-based VMs) a self-contained qcow2 overlay copy.
//
// The VM must be Paused. On success the VM resumes to Paused.
func (vm *VM) CreateSnapshotWithStore(ctx context.Context, snapStore *SnapshotStore,
	snapRoot string) (*SnapshotInfo, error) {

	if err := vm.sm.Transition(StatePaused, StateSnapshotting); err != nil {
		return nil, fmt.Errorf("snapshot requires VM %q to be paused: %w", vm.id, err)
	}
	defer func() {
		_ = vm.sm.Transition(StateSnapshotting, StatePaused)
	}()

	if snapStore == nil {
		return nil, fmt.Errorf("snapshot store not configured")
	}
	if err := snapStore.EnsureDirs(); err != nil {
		return nil, fmt.Errorf("ensure snapshot dirs: %w", err)
	}

	cleanup := newSnapshotCleanup(vm)

	snapID := uuid.NewString()
	snapDir := m6SnapshotDir(snapRoot, snapID)
	if err := os.MkdirAll(snapDir, 0o750); err != nil {
		return nil, fmt.Errorf("create snapshot dir: %w", err)
	}

	snapshotPath := snapStore.statePath(snapID)
	memFilePath := snapStore.memPath(snapID)
	cleanup.trackSnapshot(snapshotPath, memFilePath, snapDir)

	params := &models.SnapshotCreateParams{
		SnapshotPath: &snapshotPath,
		MemFilePath:  &memFilePath,
	}
	if err := vm.api.CreateSnapshot(ctx, params); err != nil {
		if cerr := cleanup.Cleanup(); cerr != nil {
			vm.logger.WithError(cerr).Warn("snapshot cleanup failed")
		}
		return nil, fmt.Errorf("create snapshot: %w", err)
	}

	for _, p := range []string{snapshotPath, memFilePath} {
		if err := validateArtifact(p); err != nil {
			if cerr := cleanup.Cleanup(); cerr != nil {
				vm.logger.WithError(cerr).Warn("snapshot cleanup failed")
			}
			return nil, fmt.Errorf("snapshot artifact incomplete %q: %w", p, err)
		}
	}

	// Compute SHA-256 checksums for all snapshot artifacts so they can be
	// verified before restore.
	checksums := make(map[string]string)
	for _, p := range []string{snapshotPath, memFilePath} {
		sum, err := computeChecksum(p)
		if err != nil {
			if cerr := cleanup.Cleanup(); cerr != nil {
				vm.logger.WithError(cerr).Warn("snapshot cleanup failed")
			}
			return nil, fmt.Errorf("compute checksum for %q: %w", p, err)
		}
		checksums[p] = sum
	}

	version := ""
	if info, err := vm.api.GetInstanceInfo(ctx); err == nil && info.VmmVersion != nil {
		version = *info.VmmVersion
	}

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
		ArtifactChecksums:  checksums,
	}

	// For template-based VMs, create a self-contained overlay copy so the
	// snapshot is independent of the source VM's overlay. qemu-img convert
	// reads through the qcow2 backing chain and writes all data to the new
	// image, making it self-contained.
	if vm.TemplateStore != nil && vm.LayerMgr != nil && vm.cfg.TemplateID != "" {
		layerInfo := vm.LayerMgr.LayerMetadata(vm.id)
		if layerInfo != nil {
			info.TemplateID = layerInfo.TemplateID
			info.BackingImagePath = layerInfo.BackingImagePath
			info.StorageFormat = layerInfo.Format

			snapOverlayPath := filepath.Join(snapDir, snapID+".qcow2")
			tmpOverlay := snapOverlayPath + ".tmp"
			cleanup.trackOverlay(snapOverlayPath, tmpOverlay)

			cmd := exec.Command("qemu-img", "convert", "-O", "qcow2", layerInfo.LayerPath, tmpOverlay)
			if out, err := cmd.CombinedOutput(); err != nil {
				if cerr := cleanup.Cleanup(); cerr != nil {
					vm.logger.WithError(cerr).Warn("snapshot cleanup failed")
				}
				return nil, fmt.Errorf("copy overlay for snapshot: %w\noutput: %s", err, string(out))
			}
			if err := os.Rename(tmpOverlay, snapOverlayPath); err != nil {
				if cerr := cleanup.Cleanup(); cerr != nil {
					vm.logger.WithError(cerr).Warn("snapshot cleanup failed")
				}
				return nil, fmt.Errorf("rename snapshot overlay: %w", err)
			}
			info.OverlayPath = snapOverlayPath
			info.NBDDevice = vm.nbdDevice

			overlaySum, err := computeChecksum(snapOverlayPath)
			if err != nil {
				if cerr := cleanup.Cleanup(); cerr != nil {
					vm.logger.WithError(cerr).Warn("snapshot cleanup failed")
				}
				return nil, fmt.Errorf("compute checksum for overlay %q: %w", snapOverlayPath, err)
			}
			info.ArtifactChecksums[snapOverlayPath] = overlaySum
		}
	}

	if err := snapStore.Create(info); err != nil {
		if cerr := cleanup.Cleanup(); cerr != nil {
			vm.logger.WithError(cerr).Warn("snapshot cleanup failed")
		}
		return nil, err
	}

	return info, nil
}

// validateArtifact checks that a file exists and is non-empty.
func validateArtifact(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.Size() == 0 {
		return fmt.Errorf("artifact is empty")
	}
	return nil
}

// computeChecksum returns the hex-encoded SHA-256 checksum of the file at path,
// computed via streaming I/O.
func computeChecksum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// verifyArtifactChecksums verifies that each artifact listed in snap's
// ArtifactChecksums matches its stored SHA-256 checksum. If ArtifactChecksums
// is empty (legacy snapshot), verification is skipped for backward
// compatibility.
func verifyArtifactChecksums(snap *SnapshotInfo) error {
	if len(snap.ArtifactChecksums) == 0 {
		return nil
	}
	for path, expected := range snap.ArtifactChecksums {
		actual, err := computeChecksum(path)
		if err != nil {
			return fmt.Errorf("checksum verification failed for %q: %w", path, err)
		}
		if actual != expected {
			return fmt.Errorf("checksum mismatch for %q: expected %s, got %s", path, expected, actual)
		}
	}
	return nil
}

// SnapshotStore manages durable snapshot metadata and artifacts. Snapshots are
// stored under <baseDir>/<snapshotID>/ with metadata.json, state.snap, and
// mem.snap. It is safe for concurrent use.
type SnapshotStore struct {
	baseDir string
}

// NewSnapshotStore creates a store that manages snapshot artifacts under
// baseDir using the M6 layout.
func NewSnapshotStore(baseDir string) *SnapshotStore {
	return &SnapshotStore{baseDir: baseDir}
}

// EnsureDirs creates the snapshot root directory.
func (s *SnapshotStore) EnsureDirs() error {
	return os.MkdirAll(s.baseDir, 0o750)
}

// snapshotDir returns the directory for a snapshot: <baseDir>/<snapID>.
func (s *SnapshotStore) snapshotDir(snapID string) string {
	return m6SnapshotDir(s.baseDir, snapID)
}

// metadataPath returns the path to a snapshot's metadata file.
func (s *SnapshotStore) metadataPath(snapID string) string {
	return filepath.Join(s.snapshotDir(snapID), snapshotMetadataFile)
}

// statePath returns the path to a snapshot's state file.
func (s *SnapshotStore) statePath(snapID string) string {
	return filepath.Join(s.snapshotDir(snapID), snapshotStateFile)
}

// memPath returns the path to a snapshot's memory file.
func (s *SnapshotStore) memPath(snapID string) string {
	return filepath.Join(s.snapshotDir(snapID), snapshotMemFile)
}

// Create persists a snapshot's metadata and validates its artifacts. The
// metadata is written atomically (temp file + rename) so partially published
// snapshots are never visible.
func (s *SnapshotStore) Create(snap *SnapshotInfo) error {
	snapDir := s.snapshotDir(snap.ID)
	if err := os.MkdirAll(snapDir, 0o750); err != nil {
		return fmt.Errorf("create snapshot dir: %w", err)
	}

	for _, p := range []string{snap.SnapshotPath, snap.MemFilePath} {
		if err := validateArtifact(p); err != nil {
			return fmt.Errorf("snapshot artifact incomplete %q: %w", p, err)
		}
	}
	if snap.OverlayPath != "" {
		if err := validateArtifact(snap.OverlayPath); err != nil {
			return fmt.Errorf("snapshot overlay incomplete %q: %w", snap.OverlayPath, err)
		}
	}

	snap.Status = snapshotStatusReady
	snap.MetadataVersion = snapshotMetadataVersion

	metaTmp := filepath.Join(snapDir, snapshotMetadataFile+".tmp")
	metaFinal := s.metadataPath(snap.ID)
	if err := writeMetadataAtomically(metaTmp, metaFinal, snap); err != nil {
		return fmt.Errorf("write snapshot metadata: %w", err)
	}
	return nil
}

// Get returns the snapshot metadata for snapID, or nil if not found.
func (s *SnapshotStore) Get(snapID string) *SnapshotInfo {
	data, err := os.ReadFile(s.metadataPath(snapID))
	if err != nil {
		return nil
	}
	var snap SnapshotInfo
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil
	}
	return &snap
}

// List returns metadata for all known snapshots.
func (s *SnapshotStore) List() ([]*SnapshotInfo, error) {
	entries, err := os.ReadDir(s.baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var snaps []*SnapshotInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(s.metadataPath(e.Name()))
		if err != nil {
			continue
		}
		var snap SnapshotInfo
		if err := json.Unmarshal(data, &snap); err != nil {
			continue
		}
		snaps = append(snaps, &snap)
	}
	return snaps, nil
}

// Validate checks that a snapshot's metadata and artifacts are intact. Returns
// an error if the snapshot is unknown or any artifact is missing or empty.
func (s *SnapshotStore) Validate(snapID string) error {
	snap := s.Get(snapID)
	if snap == nil {
		return fmt.Errorf("snapshot %q not found", snapID)
	}
	if snap.Status != snapshotStatusReady {
		return fmt.Errorf("snapshot %q is not ready (status=%q)", snapID, snap.Status)
	}
	for _, p := range []string{snap.SnapshotPath, snap.MemFilePath} {
		if err := validateArtifact(p); err != nil {
			return fmt.Errorf("snapshot artifact incomplete %q: %w", p, err)
		}
	}
	if snap.OverlayPath != "" {
		if err := validateArtifact(snap.OverlayPath); err != nil {
			return fmt.Errorf("snapshot overlay incomplete %q: %w", snap.OverlayPath, err)
		}
	}
	if err := verifyArtifactChecksums(snap); err != nil {
		return err
	}
	return nil
}

// Delete removes a snapshot's metadata and artifacts. It does NOT remove the
// template backing image or any VM's overlay; it only removes the snapshot's
// own overlay copy (if any) and state/memory files. It is safe to call after a
// failed restore attempt.
func (s *SnapshotStore) Delete(snapID string) error {
	snapDir := s.snapshotDir(snapID)
	snap := s.Get(snapID)
	var errs []error

	if snap != nil && snap.OverlayPath != "" {
		if err := os.Remove(snap.OverlayPath); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove overlay: %w", err))
		}
	}

	for _, p := range []string{s.metadataPath(snapID), s.statePath(snapID), s.memPath(snapID)} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove %q: %w", p, err))
		}
	}

	if err := os.Remove(snapDir); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("remove snapshot dir: %w", err))
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// Legacy SnapshotStore methods (M5 layout) preserved for Fork/restoreVM
// backward compatibility. These use the <base>/snapshots/<vmID>/ layout.

// SnapshotPath returns the on-disk location of a snapshot's state file (legacy layout).
func (s *SnapshotStore) SnapshotPath(vmID, snapID string) string {
	return filepath.Join(snapshotStoreDir(s.baseDir, vmID), snapID+".snapshot")
}

// MemPath returns the on-disk location of a snapshot's memory file (legacy layout).
func (s *SnapshotStore) MemPath(vmID, snapID string) string {
	return filepath.Join(snapshotStoreDir(s.baseDir, vmID), snapID+".mem")
}

// Remove deletes a snapshot's legacy-layout artifacts.
func (s *SnapshotStore) Remove(vmID, snapID string) error {
	dir := snapshotStoreDir(s.baseDir, vmID)
	var errs []error
	for _, ext := range []string{".snapshot", ".mem"} {
		p := filepath.Join(dir, snapID+ext)
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ForkOpts controls how a child VM is forked from a parent.
type ForkOpts struct {
	// CopyRootFS, when true, copies the parent's root filesystem into a new
	// child-owned writable image so the two VMs never share mutable disk.
	// When false, the child shares the parent's root filesystem read-only
	// (documented limitation: concurrent writes are unsafe).
	CopyRootFS bool

	// ChildConfig overrides for the child VM (ID, WorkDir, VsockPort, etc.).
	// Fields left zero inherit from the parent.
	ChildConfig Config
}

// Fork creates a new, independently managed VM from a snapshot of the parent.
// The parent must be Paused. Fork is an orchestration workflow: it snapshots
// the parent, then restores a new VM from that snapshot with its own identity,
// working directory, sockets, and (optionally) an isolated writable rootfs
// copy. The child has an independent lifecycle; destroying the parent never
// touches child resources. The child's vsock UDS path is overridden during
// snapshot restore so it does not collide with the parent's still-bound
// vsock socket.
func (vm *VM) Fork(ctx context.Context, opts ForkOpts) (*VM, error) {
	if vm.sm.State() != StatePaused {
		return nil, fmt.Errorf("fork requires parent VM %q to be paused, current state %q", vm.id, vm.sm.State())
	}

	snap, err := vm.createSnapshotLegacy(ctx)
	if err != nil {
		return nil, fmt.Errorf("fork snapshot: %w", err)
	}

	childCfg := opts.ChildConfig
	if childCfg.ID == "" {
		childCfg.ID = uuid.NewString()
	}
	if childCfg.WorkDir == "" {
		childCfg.WorkDir = filepath.Dir(vm.res.Dir)
	}
	if childCfg.KernelImage == "" {
		childCfg.KernelImage = vm.cfg.KernelImage
	}
	if childCfg.BootArgs == "" {
		childCfg.BootArgs = vm.cfg.BootArgs
	}
	if childCfg.VCPU == 0 {
		childCfg.VCPU = vm.cfg.VCPU
	}
	if childCfg.MemSizeMiB == 0 {
		childCfg.MemSizeMiB = vm.cfg.MemSizeMiB
	}
	if childCfg.VsockCID == 0 {
		childCfg.VsockCID = vm.cfg.VsockCID
	}
	if childCfg.VsockPort == 0 {
		childCfg.VsockPort = vm.cfg.VsockPort
	}
	if childCfg.StartTimeout == 0 {
		childCfg.StartTimeout = vm.cfg.StartTimeout
	}
	if childCfg.StopTimeout == 0 {
		childCfg.StopTimeout = vm.cfg.StopTimeout
	}
	if childCfg.StopGraceTimeout == 0 {
		childCfg.StopGraceTimeout = vm.cfg.StopGraceTimeout
	}

	if opts.CopyRootFS {
		childRootFS := filepath.Join(childCfg.resources(childCfg.ID).Dir, "rootfs.ext4")
		if err := os.MkdirAll(filepath.Dir(childRootFS), 0750); err != nil {
			return nil, fmt.Errorf("create child dir: %w", err)
		}
		if err := copyFile(vm.cfg.RootFS, childRootFS); err != nil {
			return nil, fmt.Errorf("copy rootfs for child: %w", err)
		}
		childCfg.RootFS = childRootFS
		childCfg.RootFSReadOnly = false
	} else {
		childCfg.RootFS = vm.cfg.RootFS
		childCfg.RootFSReadOnly = true
	}

	child, err := New(childCfg, vm.logger)
	if err != nil {
		return nil, fmt.Errorf("create child VM: %w", err)
	}

	if err := restoreVM(ctx, child, snap); err != nil {
		_ = child.destroy(ctx, true)
		return nil, fmt.Errorf("fork restore: %w", err)
	}

	return child, nil
}

// restoreVM boots a fresh Firecracker process for child, loads the given
// snapshot into it, and resumes the guest.
//
// Firecracker v1.17.0 requires LoadSnapshot to be called before any device
// configuration (machine config, boot source, drives). restoreVM therefore
// does NOT call configureDevices; it starts the process, loads the snapshot
// (which restores machine config, boot source, drives and the vsock
// host-socket path captured in the snapshot), then validates the guest is
// running. The child's vsock UDS path is overridden during LoadSnapshot using
// vsock_override so it does not collide with the parent's still-bound socket.
func restoreVM(ctx context.Context, child *VM, snap *SnapshotInfo) error {
	if err := child.sm.Transition(StateCreated, StatePreparing); err != nil {
		return err
	}

	cleanup := newSnapshotCleanup(child)

	if err := child.startProcessAndWait(ctx); err != nil {
		return fmt.Errorf("start restore Firecracker: %w", err)
	}
	cleanup.trackRestore(true, false, false, false)

	if err := verifyArtifactChecksums(snap); err != nil {
		if cerr := cleanup.Cleanup(); cerr != nil {
			child.logger.WithError(cerr).Warn("restore cleanup failed")
		}
		return fmt.Errorf("snapshot integrity check failed: %w", err)
	}

	if err := child.api.LoadSnapshotWithOverride(ctx, snap.SnapshotPath, snap.MemFilePath, true, child.res.VsockSock); err != nil {
		if cerr := cleanup.Cleanup(); cerr != nil {
			child.logger.WithError(cerr).Warn("restore cleanup failed")
		}
		return fmt.Errorf("load snapshot: %w", err)
	}

	// Bind the host-side vsock transport for the restored VM. Firecracker
	// v1.17.0 has already bound the vsock device to the child's unique UDS
	// path via vsock_override during LoadSnapshot; we only need the host-side
	// transport to dial it. Do NOT call configureVsock here because
	// PutGuestVsock is rejected after snapshot restoration ("not supported
	// after starting").
	child.mu.Lock()
	child.transport = NewTransport(child.res.VsockSock, child.cfg.VsockCID)
	child.mu.Unlock()
	cleanup.trackRestore(true, true, false, false)

	info, err := child.api.GetInstanceInfo(ctx)
	if err != nil {
		if cerr := cleanup.Cleanup(); cerr != nil {
			child.logger.WithError(cerr).Warn("restore cleanup failed")
		}
		return fmt.Errorf("inspect restored VM: %w", err)
	}
	if info.State == nil || *info.State != "Running" {
		if cerr := cleanup.Cleanup(); cerr != nil {
			child.logger.WithError(cerr).Warn("restore cleanup failed")
		}
		return fmt.Errorf("restored VM not running, state=%q", safeString(info.State))
	}

	child.mu.Lock()
	child.snapshot = snap
	child.mu.Unlock()
	if err := child.sm.Transition(StatePreparing, StateRunning); err != nil {
		if cerr := cleanup.Cleanup(); cerr != nil {
			child.logger.WithError(cerr).Warn("restore cleanup failed")
		}
		return err
	}

	return nil
}

// RestoreOpts controls how a VM is restored from an M6 snapshot.
type RestoreOpts struct {
	// ID is the restored VM's unique identifier. If empty, a UUID is generated.
	ID string

	// WorkDir is the base directory for the restored VM's working directory.
	// If empty, os.MkdirTemp is used.
	WorkDir string

	// VsockCID is the guest CID for the restored VM. If zero, a unique CID >= 3
	// is allocated.
	VsockCID uint32

	// VsockPort is the guest-side vsock port. If zero, inherits from snapshot metadata.
	VsockPort uint32

	// BinPath is the path to the Firecracker binary. If empty, resolved from PATH.
	BinPath string

	// StartTimeout bounds the wait for Firecracker to initialize its API socket.
	StartTimeout time.Duration

	// StopTimeout bounds the graceful shutdown wait before escalation.
	StopTimeout time.Duration

	// StopGraceTimeout bounds the final wait for process termination after escalation.
	StopGraceTimeout time.Duration
}

// RestoreSnapshot restores a VM from an M6 snapshot. It creates a new,
// independent Firecracker VM, maps the snapshot's overlay to an NBD device,
// starts Firecracker, loads the snapshot, and verifies the guest is running.
//
// The restored VM has unique host-side resources (ID, CID, API socket, vsock
// socket) and does not conflict with the source VM. The snapshot's overlay is
// mapped read-write via NBD; the template backing image remains immutable.
//
// The snapshot must be in the "ready" state and its artifacts must exist.
func RestoreSnapshot(ctx context.Context, store *SnapshotStore, snapID string,
	ts *template.Store, lm *storage.LayerManager, nm *storage.NBDManager,
	opts RestoreOpts, logger *logrus.Entry) (*VM, error) {

	if logger == nil {
		l := logrus.New()
		l.SetOutput(os.Stderr)
		logger = l.WithField("component", "vmm")
	}

	snap := store.Get(snapID)
	if snap == nil {
		return nil, fmt.Errorf("snapshot %q not found", snapID)
	}
	if err := store.Validate(snapID); err != nil {
		return nil, fmt.Errorf("snapshot %q validation failed: %w", snapID, err)
	}

	cfg := Config{
		ID:             opts.ID,
		KernelImage:    snap.KernelImage,
		BootArgs:       snap.BootArgs,
		VCPU:           snap.VCPU,
		MemSizeMiB:     snap.MemSizeMiB,
		VsockCID:       snap.VsockCID,
		VsockPort:      snap.VsockPort,
		WorkDir:        opts.WorkDir,
		BinPath:        opts.BinPath,
		TemplateID:     snap.TemplateID,
		RootFS:         snap.RootFSPath,
		RootFSReadOnly: false,
	}
	if opts.ID == "" {
		cfg.ID = uuid.NewString()
	}
	if opts.VsockCID >= 3 {
		cfg.VsockCID = opts.VsockCID
	}
	if opts.VsockPort != 0 {
		cfg.VsockPort = opts.VsockPort
	}
	if cfg.StartTimeout == 0 {
		cfg.StartTimeout = 10 * time.Second
	}
	if cfg.StopTimeout == 0 {
		cfg.StopTimeout = 15 * time.Second
	}
	if cfg.StopGraceTimeout == 0 {
		cfg.StopGraceTimeout = 5 * time.Second
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid restore config: %w", err)
	}

	vm, err := New(cfg, logger)
	if err != nil {
		return nil, fmt.Errorf("create restore VM: %w", err)
	}
	vm.TemplateStore = ts
	vm.LayerMgr = lm
	vm.NBDMgr = nm

	cleanup := newSnapshotCleanup(vm)

	if snap.TemplateID != "" && snap.OverlayPath != "" {
		if nm == nil {
			return nil, fmt.Errorf("snapshot %q requires template-based restore but no NBD manager configured", snapID)
		}
		nbdDevice, err := nm.Map(ctx, snap.OverlayPath, vm.id)
		if err != nil {
			if cerr := cleanup.Cleanup(); cerr != nil {
				vm.logger.WithError(cerr).Warn("restore cleanup failed")
			}
			return nil, fmt.Errorf("map snapshot overlay to NBD: %w", err)
		}
		vm.mu.Lock()
		vm.nbdDevice = nbdDevice
		vm.cfg.RootFS = nbdDevice
		vm.mu.Unlock()
		cleanup.trackRestore(false, false, true, true)
	}

	if err := vm.startProcessAndWait(ctx); err != nil {
		if cerr := cleanup.Cleanup(); cerr != nil {
			vm.logger.WithError(cerr).Warn("restore cleanup failed")
		}
		return nil, fmt.Errorf("start restore Firecracker: %w", err)
	}
	cleanup.trackRestore(true, false, true, true)

	if err := verifyArtifactChecksums(snap); err != nil {
		if cerr := cleanup.Cleanup(); cerr != nil {
			vm.logger.WithError(cerr).Warn("restore cleanup failed")
		}
		return nil, fmt.Errorf("snapshot integrity check failed: %w", err)
	}

	if err := vm.api.LoadSnapshotWithOverride(ctx, snap.SnapshotPath, snap.MemFilePath, true, vm.res.VsockSock); err != nil {
		if cerr := cleanup.Cleanup(); cerr != nil {
			vm.logger.WithError(cerr).Warn("restore cleanup failed")
		}
		return nil, fmt.Errorf("load snapshot: %w", err)
	}

	// Bind the host-side vsock transport for the restored VM. Firecracker
	// v1.17.0 has already bound the vsock device to the restored VM's unique
	// UDS path via vsock_override during LoadSnapshot; we only need the
	// host-side transport to dial it. Do NOT call configureVsock here because
	// PutGuestVsock is rejected after snapshot restoration ("not supported
	// after starting").
	vm.mu.Lock()
	vm.transport = NewTransport(vm.res.VsockSock, vm.cfg.VsockCID)
	vm.mu.Unlock()
	cleanup.trackRestore(true, true, true, true)

	info, err := vm.api.GetInstanceInfo(ctx)
	if err != nil {
		if cerr := cleanup.Cleanup(); cerr != nil {
			vm.logger.WithError(cerr).Warn("restore cleanup failed")
		}
		return nil, fmt.Errorf("inspect restored VM: %w", err)
	}
	if info.State == nil || *info.State != "Running" {
		if cerr := cleanup.Cleanup(); cerr != nil {
			vm.logger.WithError(cerr).Warn("restore cleanup failed")
		}
		return nil, fmt.Errorf("restored VM not running, state=%q", safeString(info.State))
	}

	vm.mu.Lock()
	vm.snapshot = snap
	vm.mu.Unlock()
	if err := vm.sm.Transition(StatePreparing, StateRunning); err != nil {
		if cerr := cleanup.Cleanup(); cerr != nil {
			vm.logger.WithError(cerr).Warn("restore cleanup failed")
		}
		return nil, fmt.Errorf("finalize restore: %w", err)
	}

	vm.logger.Info("VM restored from snapshot")
	return vm, nil
}

// copyFile copies src to dst, creating dst with 0644 permissions.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// safeString returns the string value of s, or "<nil>" if s is nil.
func safeString(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// writeJSONAtomically marshals v to tmpPath then renames to finalPath.
// writeMetadataAtomically publishes v as JSON to finalPath using a
// crash-durable sequence: marshal to a temp file in the same directory,
// write all data, fsync the file, close it, atomically rename into place,
// then fsync the containing directory where supported.
//
// Restrictive file permissions (0o600) are used so the metadata file is
// only readable/writable by the owning user. If any step fails, the temp
// file is removed and any previously published metadata at finalPath is
// preserved (rename is atomic and only replaces finalPath on success).
func writeMetadataAtomically(tmpPath, finalPath string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(finalPath)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}

	// Write to temp file with restrictive permissions.
	tmpFile, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}

	// Ensure the complete metadata is written.
	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		_ = os.Remove(tmpPath)
		return err
	}

	// Sync file contents to disk before renaming.
	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
		_ = os.Remove(tmpPath)
		return err
	}

	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	// Atomically rename temp file into place.
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	// Sync the containing directory where supported so the rename is
	// durable across crashes.
	if err := syncDir(dir); err != nil {
		return err
	}

	return nil
}

// syncDir fsyncs the directory at dir where supported. On Linux this
// persists the directory entry rename to disk.
func syncDir(dir string) error {
	df, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer df.Close()
	if err := df.Sync(); err != nil {
		return err
	}
	return nil
}

// snapshotStoreAdapter adapts *SnapshotStore to the storage.SnapshotStore
// interface so the storage package can manage snapshot references without
// importing vmm.
type snapshotStoreAdapter struct {
	store *SnapshotStore
}

// Get returns the snapshot reference for id, or ok=false if not found.
func (a *snapshotStoreAdapter) Get(id string) (*storage.SnapshotRef, bool) {
	s := a.store.Get(id)
	if s == nil {
		return nil, false
	}
	return &storage.SnapshotRef{
		ID:          s.ID,
		Status:      s.Status,
		OverlayPath: s.OverlayPath,
	}, true
}

// List returns references for all known snapshots.
func (a *snapshotStoreAdapter) List() ([]*storage.SnapshotRef, error) {
	snaps, err := a.store.List()
	if err != nil {
		return nil, err
	}
	var refs []*storage.SnapshotRef
	for _, s := range snaps {
		refs = append(refs, &storage.SnapshotRef{
			ID:          s.ID,
			Status:      s.Status,
			OverlayPath: s.OverlayPath,
		})
	}
	return refs, nil
}

// Delete removes a snapshot's metadata and artifacts.
func (a *snapshotStoreAdapter) Delete(id string) error {
	return a.store.Delete(id)
}

// StorageSnapshotStore returns a storage.SnapshotStore adapter wrapping this
// SnapshotStore.
func (s *SnapshotStore) StorageSnapshotStore() storage.SnapshotStore {
	return &snapshotStoreAdapter{store: s}
}

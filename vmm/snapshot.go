package vmm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"

	"github.com/omkar/sandbox/internal/storage"
	"github.com/omkar/sandbox/internal/template"
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

	snapDir := snapshotStoreDir(filepath.Dir(vm.res.Dir), vm.id)
	if err := os.MkdirAll(snapDir, 0750); err != nil {
		return nil, fmt.Errorf("create snapshot dir: %w", err)
	}

	snapID := uuid.NewString()
	snapshotPath := filepath.Join(snapDir, snapID+".snapshot")
	memFilePath := filepath.Join(snapDir, snapID+".mem")

	created := []string{}
	cleanup := func() {
		for _, p := range created {
			_ = os.Remove(p)
		}
		_ = os.Remove(snapshotPath)
		_ = os.Remove(memFilePath)
	}

	params := &models.SnapshotCreateParams{
		SnapshotPath: &snapshotPath,
		MemFilePath:  &memFilePath,
	}
	if err := vm.api.CreateSnapshot(ctx, params); err != nil {
		cleanup()
		return nil, fmt.Errorf("create snapshot: %w", err)
	}
	created = append(created, snapshotPath, memFilePath)

	for _, p := range created {
		if err := validateArtifact(p); err != nil {
			cleanup()
			return nil, fmt.Errorf("snapshot artifact incomplete %q: %w", p, err)
		}
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

	snapID := uuid.NewString()
	snapDir := m6SnapshotDir(snapRoot, snapID)
	if err := os.MkdirAll(snapDir, 0o750); err != nil {
		return nil, fmt.Errorf("create snapshot dir: %w", err)
	}

	snapshotPath := snapStore.statePath(snapID)
	memFilePath := snapStore.memPath(snapID)

	params := &models.SnapshotCreateParams{
		SnapshotPath: &snapshotPath,
		MemFilePath:  &memFilePath,
	}
	if err := vm.api.CreateSnapshot(ctx, params); err != nil {
		_ = os.Remove(snapshotPath)
		_ = os.Remove(memFilePath)
		return nil, fmt.Errorf("create snapshot: %w", err)
	}

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

	if err := snapStore.Create(info); err != nil {
		_ = os.Remove(snapshotPath)
		_ = os.Remove(memFilePath)
		if info.OverlayPath != "" {
			_ = os.Remove(info.OverlayPath)
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
	if err := writeJSONAtomically(metaTmp, metaFinal, snap); err != nil {
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
// touches child resources.
//
// Limitation: Firecracker v1.17.0 captures the vsock host-socket path in a
// snapshot and restores it during LoadSnapshot, and does not allow the vsock
// path to be changed before LoadSnapshot ("boot-specific resources") or after
// the microVM has started ("not supported after starting"). Because the parent
// must remain alive (Paused) for the fork, it still holds its vsock socket, so
// the child's LoadSnapshot fails with "Address in use" on the vsock backend.
// Fork therefore does NOT provide independent vsock for the child in v1.17.0;
// it is a safe subset (rootfs copy + independent identity/lifecycle) with this
// documented vsock limitation.
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
// running.
//
// Note: vsock cannot be reconfigured for the restored VM in v1.17.0 — it is
// rejected before LoadSnapshot ("boot-specific resources") and after the
// microVM has started ("not supported after starting"). The child therefore
// inherits the vsock host-socket path from the snapshot, which conflicts with
// a still-running parent. Fork with independent vsock is not feasible in
// v1.17.0 while the parent is alive (see Fork).
func restoreVM(ctx context.Context, child *VM, snap *SnapshotInfo) error {
	if err := child.sm.Transition(StateCreated, StatePreparing); err != nil {
		return err
	}
	if err := child.startProcessAndWait(ctx); err != nil {
		return err
	}

	if err := child.api.LoadSnapshot(ctx, &models.SnapshotLoadParams{
		SnapshotPath: &snap.SnapshotPath,
		MemFilePath:  snap.MemFilePath,
		ResumeVM:     true,
	}); err != nil {
		child.stopProcLocked()
		return fmt.Errorf("load snapshot: %w", err)
	}

	info, err := child.api.GetInstanceInfo(ctx)
	if err != nil {
		return fmt.Errorf("inspect restored VM: %w", err)
	}
	if info.State == nil || *info.State != "Running" {
		return fmt.Errorf("restored VM not running, state=%q", safeString(info.State))
	}

	child.mu.Lock()
	child.snapshot = snap
	child.mu.Unlock()
	if err := child.sm.Transition(StatePreparing, StateRunning); err != nil {
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

	if snap.TemplateID != "" && snap.OverlayPath != "" {
		if nm == nil {
			return nil, fmt.Errorf("snapshot %q requires template-based restore but no NBD manager configured", snapID)
		}
		nbdDevice, err := nm.Map(snap.OverlayPath, vm.id)
		if err != nil {
			_ = vm.destroy(ctx, true)
			return nil, fmt.Errorf("map snapshot overlay to NBD: %w", err)
		}
		vm.mu.Lock()
		vm.nbdDevice = nbdDevice
		vm.cfg.RootFS = nbdDevice
		vm.mu.Unlock()
	}

	if err := vm.startProcessAndWait(ctx); err != nil {
		_ = vm.destroy(ctx, true)
		return nil, fmt.Errorf("start restore Firecracker: %w", err)
	}

	if err := vm.api.LoadSnapshot(ctx, &models.SnapshotLoadParams{
		SnapshotPath: &snap.SnapshotPath,
		MemFilePath:  snap.MemFilePath,
		ResumeVM:     true,
	}); err != nil {
		vm.stopProcLocked()
		_ = vm.destroy(ctx, true)
		return nil, fmt.Errorf("load snapshot: %w", err)
	}

	info, err := vm.api.GetInstanceInfo(ctx)
	if err != nil {
		_ = vm.destroy(ctx, true)
		return nil, fmt.Errorf("inspect restored VM: %w", err)
	}
	if info.State == nil || *info.State != "Running" {
		_ = vm.destroy(ctx, true)
		return nil, fmt.Errorf("restored VM not running, state=%q", safeString(info.State))
	}

	vm.mu.Lock()
	vm.snapshot = snap
	vm.mu.Unlock()
	if err := vm.sm.Transition(StatePreparing, StateRunning); err != nil {
		_ = vm.destroy(ctx, true)
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
func writeJSONAtomically(tmpPath, finalPath string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, finalPath)
}

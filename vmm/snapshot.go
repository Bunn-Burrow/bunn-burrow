package vmm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/google/uuid"
)

// SnapshotInfo records the metadata and artifact locations for a snapshot.
type SnapshotInfo struct {
	ID                 string
	SourceVM           string
	SnapshotPath       string // microVM state file
	MemFilePath        string // guest memory file
	RootFSPath         string // root filesystem path used when the snapshot was taken
	FirecrackerVersion string
	CreatedAt          time.Time
	SourceState        string // Firecracker instance state when snapshotted (e.g. "Paused")
}

// snapshotStoreDir returns the directory holding snapshot artifacts for a VM
// under the given store root: <root>/snapshots/<vmID>.
func snapshotStoreDir(base, vmID string) string {
	return filepath.Join(base, "snapshots", vmID)
}

// CreateSnapshot snapshots the VM. Firecracker only supports snapshotting a
// paused VM, so the VM must be in the Paused host state. Two artifacts are
// produced: the microVM state file and the guest memory file. Incomplete
// artifacts are cleaned up on failure.
func (vm *VM) CreateSnapshot(ctx context.Context) (*SnapshotInfo, error) {
	if err := vm.sm.Transition(StatePaused, StateSnapshotting); err != nil {
		return nil, fmt.Errorf("snapshot requires VM %q to be paused: %w", vm.id, err)
	}
	// Always return to Paused when snapshot completes or fails.
	defer func() {
		_ = vm.sm.Transition(StateSnapshotting, StatePaused)
	}()

	// Store snapshots under the VM's work-dir base (a sibling of the VM's
	// private directory) so the artifacts survive destruction of the VM.
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

	// Validate artifact completeness.
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
	}
	vm.mu.Lock()
	vm.snapshot = info
	vm.mu.Unlock()
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

// SnapshotStore manages snapshot artifact storage and metadata.
type SnapshotStore struct {
	baseDir string
}

// NewSnapshotStore creates a store that manages snapshot artifact storage and
// metadata under baseDir.
func NewSnapshotStore(baseDir string) *SnapshotStore {
	return &SnapshotStore{baseDir: baseDir}
}

// SnapshotPath returns the on-disk location of a snapshot's state file.
func (s *SnapshotStore) SnapshotPath(vmID, snapID string) string {
	return filepath.Join(snapshotStoreDir(s.baseDir, vmID), snapID+".snapshot")
}

// MemPath returns the on-disk location of a snapshot's memory file.
func (s *SnapshotStore) MemPath(vmID, snapID string) string {
	return filepath.Join(snapshotStoreDir(s.baseDir, vmID), snapID+".mem")
}

// Remove deletes a snapshot's artifacts.
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

	// Snapshot the parent.
	snap, err := vm.CreateSnapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("fork snapshot: %w", err)
	}

	// Build child config.
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

	// Provide the child an isolated writable rootfs by default.
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

	// Restore the child from the snapshot.
	if err := restoreVM(ctx, child, snap); err != nil {
		// Defined failure cleanup: stop and destroy the partially created child.
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

	// Validate the restored VM state before reporting success.
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

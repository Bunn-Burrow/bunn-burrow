package storage

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Bunn-Burrow/bunn-burrow/internal/template"
)

// SnapshotRef is a storage-side summary of a snapshot, used by the
// SnapshotStore interface so the storage package does not import vmm.
type SnapshotRef struct {
	// ID is the snapshot identifier.
	ID string

	// Status is the snapshot lifecycle status ("ready", "creating", "failed").
	Status string

	// OverlayPath is the path to the snapshot's overlay copy, if any.
	OverlayPath string
}

// SnapshotStore is the interface StorageManager uses for snapshot operations.
// It is implemented by vmm.SnapshotStore via an adapter.
type SnapshotStore interface {
	// Get returns the snapshot reference for id, or ok=false if not found.
	Get(id string) (*SnapshotRef, bool)

	// List returns references for all known snapshots.
	List() ([]*SnapshotRef, error)

	// Delete removes a snapshot's metadata and artifacts.
	Delete(id string) error
}

// GCMode controls whether CollectGarbage only reports or also executes deletions.
type GCMode string

const (
	// GCModeDryRun reports what would be deleted without removing anything.
	GCModeDryRun GCMode = "dry-run"
	// GCModeExec executes garbage collection and removes orphaned resources.
	GCModeExec GCMode = "exec"
)

// GCOptions controls garbage collection behavior.
type GCOptions struct {
	// Mode is dry-run or exec. Defaults to dry-run.
	Mode GCMode

	// DeleteTempFiles removes *.tmp files left by interrupted operations.
	DeleteTempFiles bool

	// Logger receives structured GC log entries. If nil, logging is skipped.
	Logger Logger
}

// GCReport summarizes the results of a garbage collection run.
type GCReport struct {
	// Mode is the GC mode that was run.
	Mode GCMode

	// OrphanedOverlays are overlay resources with no valid owner.
	OrphanedOverlays []string

	// OrphanedSnapshotOverlays are snapshot overlay resources with no valid
	// snapshot metadata.
	OrphanedSnapshotOverlays []string

	// OrphanedVMResources are VM resources whose VM no longer exists.
	OrphanedVMResources []string

	// TempFiles are temporary files found and removed (or would be removed).
	TempFiles []string

	// WouldDelete lists resource paths that would be removed in exec mode.
	WouldDelete []string

	// Deleted lists resource paths that were actually removed.
	Deleted []string

	// Errors are non-fatal errors encountered during GC.
	Errors []error
}

// Logger is a minimal logging interface for storage operations.
type Logger interface {
	Info(msg string, fields map[string]any)
	Warn(msg string, fields map[string]any)
	Error(msg string, fields map[string]any)
}

// noopLogger discards all log entries.
type noopLogger struct{}

func (noopLogger) Info(msg string, fields map[string]any)  {}
func (noopLogger) Warn(msg string, fields map[string]any)  {}
func (noopLogger) Error(msg string, fields map[string]any) {}

// StorageManager coordinates persistent storage lifecycle: VM overlays, template
// backing images, snapshot overlays, NBD mappings, and garbage collection. It
// composes LayerManager, NBDManager, SnapshotStore, MetadataStore, and
// TemplateStore into a single lifecycle owner.
//
// Reference model:
//   - VM -> overlay (VM owns its overlay)
//   - VM -> template (VM acquires a template reference via TemplateStore)
//   - overlay -> backing image (overlay's backing image is reference-counted)
//   - snapshot overlay -> backing image (snapshot's backing image is
//     reference-counted)
//
// All metadata mutations are persisted atomically so recovery after a process
// restart can reconcile recorded state with actual filesystem/NBD state.
type StorageManager struct {
	mu        sync.Mutex
	rootDir   string
	layerMgr  *LayerManager
	nbdMgr    NBDManager
	snapStore SnapshotStore
	tmplStore *template.Store
	metaStore *MetadataStore
	logger    Logger

	// in-memory indexes (rebuilt from metadata store on Recover)
	resources    map[string]*StorageResource    // by resource ID
	vmOverlays   map[string]string              // vmID -> overlay resource ID
	templateVMs  map[string]map[string]struct{} // templateID -> set of VM IDs
	snapOverlays map[string]string              // snapID -> snapshot overlay resource ID
}

// NewStorageManager creates a StorageManager that composes the given storage
// components. All components must be non-nil for template-based operations;
// nil is allowed for direct-rootfs-only usage.
func NewStorageManager(rootDir string,
	layerMgr *LayerManager,
	nbdMgr NBDManager,
	snapStore SnapshotStore,
	tmplStore *template.Store,
) *StorageManager {
	return &StorageManager{
		rootDir:      rootDir,
		layerMgr:     layerMgr,
		nbdMgr:       nbdMgr,
		snapStore:    snapStore,
		tmplStore:    tmplStore,
		metaStore:    NewMetadataStore(rootDir),
		resources:    make(map[string]*StorageResource),
		vmOverlays:   make(map[string]string),
		templateVMs:  make(map[string]map[string]struct{}),
		snapOverlays: make(map[string]string),
	}
}

// NewStorageManagerWithMetadataStore creates a StorageManager using the
// supplied MetadataStore. Pass the same instance to ImageManager,
// VolumeManager, and other metadata consumers.
func NewStorageManagerWithMetadataStore(
	rootDir string,
	layerMgr *LayerManager,
	nbdMgr NBDManager,
	snapStore SnapshotStore,
	tmplStore *template.Store,
	metaStore *MetadataStore,
) *StorageManager {
	if metaStore == nil {
		metaStore = NewMetadataStore(rootDir)
	}

	return &StorageManager{
		rootDir:      rootDir,
		layerMgr:     layerMgr,
		nbdMgr:       nbdMgr,
		snapStore:    snapStore,
		tmplStore:    tmplStore,
		metaStore:    metaStore,
		resources:    make(map[string]*StorageResource),
		vmOverlays:   make(map[string]string),
		templateVMs:  make(map[string]map[string]struct{}),
		snapOverlays: make(map[string]string),
	}
}

// WithLogger returns a StorageManager with the given logger for structured
// storage lifecycle logging.
func (sm *StorageManager) WithLogger(logger Logger) *StorageManager {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.logger = logger
	return sm
}

// MetadataStore returns the shared MetadataStore used by this StorageManager.
func (sm *StorageManager) MetadataStore() *MetadataStore {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.metaStore
}

// EnsureDirs creates all required storage directories.
func (sm *StorageManager) EnsureDirs() error {
	if err := sm.metaStore.EnsureDirs(); err != nil {
		return fmt.Errorf("ensure metadata dirs: %w", err)
	}
	if sm.layerMgr != nil {
		if err := sm.layerMgr.EnsureDirs(); err != nil {
			return fmt.Errorf("ensure layer dirs: %w", err)
		}
	}
	if sm.snapStore != nil {
		// SnapshotStore.EnsireDirs is called by callers directly; the interface
		// does not expose it. Ensure the root dir exists.
		if err := os.MkdirAll(sm.rootDir, 0o750); err != nil {
			return fmt.Errorf("ensure snapshot root: %w", err)
		}
	}
	return nil
}

// log emits a structured log entry if a logger is configured.
func (sm *StorageManager) log(level, msg string, fields map[string]any) {
	if sm == nil || sm.logger == nil {
		return
	}
	all := map[string]any{}
	for k, v := range fields {
		all[k] = v
	}
	switch level {
	case "info":
		sm.logger.Info(msg, all)
	case "warn":
		sm.logger.Warn(msg, all)
	case "error":
		sm.logger.Error(msg, all)
	}
}

// RegisterVM registers a VM and its overlay resource. It acquires a reference
// to the template backing image (if templateID is set) and records the VM as a
// dependent of the template. Returns the overlay resource ID.
func (sm *StorageManager) RegisterVM(vmID, templateID, overlayPath, nbdDevice string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if vmID == "" {
		return fmt.Errorf("vm id is required")
	}

	now := time.Now()

	// Register the VM resource.
	vmRes := &StorageResource{
		ID:         "vm:" + vmID,
		Type:       ResourceTypeVM,
		VMID:       vmID,
		TemplateID: templateID,
		CreatedAt:  now,
		UpdatedAt:  now,
		State:      StateActive,
	}
	if err := sm.metaStore.Register(vmRes); err != nil {
		return err
	}
	sm.resources[vmRes.ID] = vmRes

	// Register the overlay resource if a path is given.
	if overlayPath != "" && sm.layerMgr != nil {
		overlayRes := &StorageResource{
			ID:             "overlay:" + vmID,
			Type:           ResourceTypeOverlay,
			Path:           overlayPath,
			VMID:           vmID,
			TemplateID:     templateID,
			CreatedAt:      now,
			UpdatedAt:      now,
			State:          StateActive,
			BackingImage:   sm.backingPathForTemplate(templateID),
			ReferenceCount: 1,
		}
		if err := sm.metaStore.Register(overlayRes); err != nil {
			// Roll back: remove VM resource from both metadata store and cache.
			_ = sm.metaStore.Delete(vmRes.ID)
			delete(sm.resources, vmRes.ID)
			return err
		}
		sm.resources[overlayRes.ID] = overlayRes
		sm.vmOverlays[vmID] = overlayRes.ID

		// Acquire backing image reference.
		if templateID != "" {
			sm.acquireTemplateRefLocked(templateID, vmID)
			sm.acquireBackingRefLocked(overlayRes.BackingImage, vmID)
		}
	}

	if templateID != "" {
		if sm.templateVMs[templateID] == nil {
			sm.templateVMs[templateID] = make(map[string]struct{})
		}
		sm.templateVMs[templateID][vmID] = struct{}{}
	}

	sm.log("info", "VM registered", map[string]any{
		"vm_id":        vmID,
		"template_id":  templateID,
		"overlay_path": overlayPath,
		"nbd_device":   nbdDevice,
	})
	return nil
}

// ReleaseVM releases a VM's overlay, backing image reference, and template
// reference. The VM resource transitions to deleted. It is idempotent.
func (sm *StorageManager) ReleaseVM(vmID string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	vmResID := "vm:" + vmID
	vmRes := sm.resources[vmResID]
	if vmRes == nil {
		vmRes = sm.metaStore.Get(vmResID)
	}
	if vmRes == nil {
		return nil // already released
	}

	templateID := vmRes.TemplateID

	// Release overlay.
	if overlayResID, ok := sm.vmOverlays[vmID]; ok {
		overlayRes := sm.resources[overlayResID]
		if overlayRes == nil {
			overlayRes = sm.metaStore.Get(overlayResID)
		}
		if overlayRes != nil && overlayRes.BackingImage != "" {
			sm.releaseBackingRefLocked(overlayRes.BackingImage, vmID)
		}
		_ = sm.metaStore.Delete(overlayResID)
		delete(sm.resources, overlayResID)
		delete(sm.vmOverlays, vmID)
	}

	// Release template reference.
	if templateID != "" {
		sm.releaseTemplateRefLocked(templateID, vmID)
		delete(sm.templateVMs[templateID], vmID)
		if len(sm.templateVMs[templateID]) == 0 {
			delete(sm.templateVMs, templateID)
		}
	}

	// Mark VM resource deleted.
	vmRes.State = StateDeleted
	vmRes.UpdatedAt = time.Now()
	_ = sm.metaStore.writeLocked(vmRes)
	delete(sm.resources, vmResID)

	sm.log("info", "VM released", map[string]any{
		"vm_id":       vmID,
		"template_id": templateID,
	})
	return nil
}

// backingPathForTemplate returns the backing image path for a template, or
// empty string if the template store or layer manager is unavailable.
func (sm *StorageManager) backingPathForTemplate(templateID string) string {
	if sm.layerMgr == nil || sm.tmplStore == nil {
		return ""
	}
	if tmpl := sm.tmplStore.Get(templateID); tmpl != nil {
		return sm.layerMgr.BackingImagePath(templateID)
	}
	return ""
}

// acquireTemplateRefLocked increments the template reference count. Caller must
// hold sm.mu.
func (sm *StorageManager) acquireTemplateRefLocked(templateID, vmID string) {
	if sm.tmplStore == nil {
		return
	}
	_ = sm.tmplStore.Acquire(templateID)
}

// releaseTemplateRefLocked decrements the template reference count. Caller must
// hold sm.mu.
func (sm *StorageManager) releaseTemplateRefLocked(templateID, vmID string) {
	if sm.tmplStore == nil {
		return
	}
	sm.tmplStore.Release(templateID)
}

// acquireBackingRefLocked increments a backing image reference count. Caller
// must hold sm.mu.
func (sm *StorageManager) acquireBackingRefLocked(backingPath, owner string) {
	if backingPath == "" {
		return
	}
	resID := "backing:" + backingPath
	res := sm.resources[resID]
	if res == nil {
		res = sm.metaStore.Get(resID)
	}
	if res == nil {
		res = &StorageResource{
			ID:             resID,
			Type:           ResourceTypeBacking,
			Path:           backingPath,
			CreatedAt:      time.Now(),
			UpdatedAt:      time.Now(),
			State:          StateActive,
			ReferenceCount: 0,
		}
		_ = sm.metaStore.Register(res)
		sm.resources[res.ID] = res
	}
	res.ReferenceCount++
	res.UpdatedAt = time.Now()
	_ = sm.metaStore.writeLocked(res)
}

// releaseBackingRefLocked decrements a backing image reference count. If the
// count reaches zero and no snapshot references it, the backing image resource
// transitions to deleted (the file itself is preserved — callers must not
// delete shared backing images). Caller must hold sm.mu.
func (sm *StorageManager) releaseBackingRefLocked(backingPath, owner string) {
	if backingPath == "" {
		return
	}
	resID := "backing:" + backingPath
	res := sm.resources[resID]
	if res == nil {
		res = sm.metaStore.Get(resID)
	}
	if res == nil {
		return
	}
	if res.ReferenceCount > 0 {
		res.ReferenceCount--
	}
	res.UpdatedAt = time.Now()
	if res.ReferenceCount == 0 && !sm.snapshotReferencesBackingLocked(backingPath) {
		res.State = StateDeleted
	}
	_ = sm.metaStore.writeLocked(res)
}

// snapshotReferencesBackingLocked reports whether any active overlay
// (VM or snapshot) references the given backing image. Caller must hold sm.mu.
func (sm *StorageManager) snapshotReferencesBackingLocked(backingPath string) bool {
	for _, res := range sm.resources {
		if res.Type == ResourceTypeOverlay && res.BackingImage == backingPath && res.State == StateActive {
			return true
		}
	}
	return false
}

// RegisterSnapshotOverlay registers a snapshot's self-contained overlay as a
// storage resource and acquires a backing image reference.
func (sm *StorageManager) RegisterSnapshotOverlay(snapID, templateID, overlayPath, backingImage string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if snapID == "" || overlayPath == "" {
		return fmt.Errorf("snapshot id and overlay path are required")
	}

	now := time.Now()
	overlayRes := &StorageResource{
		ID:             "snap-overlay:" + snapID,
		Type:           ResourceTypeOverlay,
		Path:           overlayPath,
		SnapshotID:     snapID,
		TemplateID:     templateID,
		CreatedAt:      now,
		UpdatedAt:      now,
		State:          StateActive,
		BackingImage:   backingImage,
		ReferenceCount: 1,
	}
	if err := sm.metaStore.Register(overlayRes); err != nil {
		return err
	}
	sm.resources[overlayRes.ID] = overlayRes
	sm.snapOverlays[snapID] = overlayRes.ID

	if backingImage != "" {
		sm.acquireBackingRefLocked(backingImage, snapID)
	}

	sm.log("info", "snapshot overlay registered", map[string]any{
		"snapshot_id":   snapID,
		"overlay_path":  overlayPath,
		"backing_image": backingImage,
	})
	return nil
}

// ReleaseSnapshotOverlay releases a snapshot overlay resource and its backing
// image reference.
func (sm *StorageManager) ReleaseSnapshotOverlay(snapID string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	overlayResID, ok := sm.snapOverlays[snapID]
	if !ok {
		return nil // already released
	}

	overlayRes := sm.resources[overlayResID]
	if overlayRes == nil {
		overlayRes = sm.metaStore.Get(overlayResID)
	}
	var backingImage string
	if overlayRes != nil {
		backingImage = overlayRes.BackingImage
	}

	// Remove the overlay from indexes BEFORE releasing the backing reference,
	// so snapshotReferencesBackingLocked does not count the overlay being
	// released as a live reference.
	delete(sm.resources, overlayResID)
	delete(sm.snapOverlays, snapID)
	_ = sm.metaStore.Delete(overlayResID)

	if backingImage != "" {
		sm.releaseBackingRefLocked(backingImage, snapID)
	}

	sm.log("info", "snapshot overlay released", map[string]any{
		"snapshot_id": snapID,
	})
	return nil
}

// DeleteSnapshot safely deletes a snapshot and releases its storage references.
// It removes the snapshot's own overlay copy, state, and memory files, but
// never removes the template backing image or any VM's overlay.
func (sm *StorageManager) DeleteSnapshot(ctx context.Context, snapID string) error {
	if sm.snapStore == nil {
		return fmt.Errorf("snapshot store not configured")
	}

	// Validate the snapshot exists.
	snap, ok := sm.snapStore.Get(snapID)
	if !ok {
		return fmt.Errorf("snapshot %q not found", snapID)
	}
	_ = snap

	// Mark snapshot overlay as deleting.
	sm.mu.Lock()
	if overlayResID, ok := sm.snapOverlays[snapID]; ok {
		if res := sm.resources[overlayResID]; res != nil {
			res.State = StateDeleting
			res.UpdatedAt = time.Now()
			_ = sm.metaStore.writeLocked(res)
		}
	}
	sm.mu.Unlock()

	// Delete snapshot metadata and artifacts via SnapshotStore.
	deleteErr := sm.snapStore.Delete(snapID)

	// Always release the snapshot overlay reference (backing image refcount),
	// even if snapshot artifact deletion failed. This prevents backing image
	// reference leaks.
	releaseErr := sm.ReleaseSnapshotOverlay(snapID)

	if deleteErr != nil {
		sm.mu.Lock()
		if overlayResID, ok := sm.snapOverlays[snapID]; ok {
			if res := sm.resources[overlayResID]; res != nil {
				res.State = StateFailed
				res.UpdatedAt = time.Now()
				_ = sm.metaStore.writeLocked(res)
			}
		}
		sm.mu.Unlock()
		return fmt.Errorf("delete snapshot %q: %w", snapID, deleteErr)
	}

	if releaseErr != nil {
		sm.log("warn", "failed to release snapshot overlay", map[string]any{
			"snapshot_id": snapID,
			"error":       releaseErr.Error(),
		})
	}

	sm.log("info", "snapshot deleted", map[string]any{
		"snapshot_id": snapID,
	})
	return nil
}

// CollectGarbage discovers orphaned storage resources and, in exec mode,
// removes them. It never deletes active VM storage, storage referenced by a
// valid snapshot, or backing images with dependent overlays.
func (sm *StorageManager) CollectGarbage(ctx context.Context, opts GCOptions) (*GCReport, error) {
	if opts.Mode == "" {
		opts.Mode = GCModeDryRun
	}

	report := &GCReport{Mode: opts.Mode}

	// Snapshot IDs known to the snapshot store.
	var snapIDs []string
	if sm.snapStore != nil {
		snaps, err := sm.snapStore.List()
		if err != nil {
			return nil, fmt.Errorf("list snapshots: %w", err)
		}
		snapIDs = make([]string, 0, len(snaps))
		for _, s := range snaps {
			snapIDs = append(snapIDs, s.ID)
		}
	}
	snapSet := make(map[string]struct{}, len(snapIDs))
	for _, id := range snapIDs {
		snapSet[id] = struct{}{}
	}

	// All registered resources.
	allRes, err := sm.metaStore.List()
	if err != nil {
		return nil, fmt.Errorf("list storage resources: %w", err)
	}

	// Rebuild in-memory indexes.
	sm.mu.Lock()
	sm.resources = make(map[string]*StorageResource, len(allRes))
	sm.vmOverlays = make(map[string]string, len(allRes))
	sm.snapOverlays = make(map[string]string, len(allRes))
	sm.templateVMs = make(map[string]map[string]struct{})
	for _, r := range allRes {
		sm.resources[r.ID] = r
		if r.Type == ResourceTypeOverlay && r.VMID != "" {
			sm.vmOverlays[r.VMID] = r.ID
		}
		if r.Type == ResourceTypeOverlay && r.SnapshotID != "" {
			sm.snapOverlays[r.SnapshotID] = r.ID
		}
		if r.Type == ResourceTypeVM && r.TemplateID != "" {
			if sm.templateVMs[r.TemplateID] == nil {
				sm.templateVMs[r.TemplateID] = make(map[string]struct{})
			}
			sm.templateVMs[r.TemplateID][r.VMID] = struct{}{}
		}
	}
	sm.mu.Unlock()

	// Scan for temp files.
	if opts.DeleteTempFiles {
		sm.collectTempFiles(ctx, report)
	}

	// Examine each registered resource for orphanhood.
	for _, res := range allRes {
		switch res.Type {
		case ResourceTypeOverlay:
			sm.gcOverlay(res, snapSet, report)
		case ResourceTypeVM:
			sm.gcVMResource(res, report)
		case ResourceTypeBacking:
			sm.gcBackingResource(res, report)
		}
	}

	// Scan disk for overlay files without registered resources (orphaned files).
	sm.collectOrphanedFiles(report)

	// Sort for deterministic output.
	sort.Strings(report.OrphanedOverlays)
	sort.Strings(report.OrphanedSnapshotOverlays)
	sort.Strings(report.OrphanedVMResources)
	sort.Strings(report.TempFiles)
	sort.Strings(report.WouldDelete)
	sort.Strings(report.Deleted)

	if opts.Mode == GCModeExec {
		for _, p := range report.WouldDelete {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				report.Errors = append(report.Errors, fmt.Errorf("remove %q: %w", p, err))
			} else {
				report.Deleted = append(report.Deleted, p)
			}
		}
		for _, p := range report.TempFiles {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				report.Errors = append(report.Errors, fmt.Errorf("remove temp %q: %w", p, err))
			} else {
				report.Deleted = append(report.Deleted, p)
			}
		}
	}

	sm.log("info", "garbage collection complete", map[string]any{
		"mode":               opts.Mode,
		"orphaned_overlays":  len(report.OrphanedOverlays),
		"orphaned_snapshots": len(report.OrphanedSnapshotOverlays),
		"temp_files":         len(report.TempFiles),
		"deleted":            len(report.Deleted),
		"errors":             len(report.Errors),
	})
	return report, nil
}

// collectTempFiles scans storage directories for *.tmp files left by
// interrupted operations.
func (sm *StorageManager) collectTempFiles(ctx context.Context, report *GCReport) {
	dirs := []string{}
	if sm.layerMgr != nil {
		dirs = append(dirs,
			filepath.Join(sm.rootDir, "backing"),
			filepath.Join(sm.rootDir, "layers"),
			filepath.Join(sm.rootDir, "metadata"),
		)
	}

	for _, dir := range dirs {
		if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if d.IsDir() {
				return nil
			}
			if strings.HasSuffix(path, ".tmp") {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
				}
				report.TempFiles = append(report.TempFiles, path)
				report.WouldDelete = append(report.WouldDelete, path)
			}
			return nil
		}); err != nil {
			report.Errors = append(report.Errors, fmt.Errorf("walk %q: %w", dir, err))
		}
	}
}

// collectOrphanedFiles scans disk for overlay/snapshot files that have no
// registered resource.
func (sm *StorageManager) collectOrphanedFiles(report *GCReport) {
	// Build set of registered paths.
	registered := make(map[string]struct{})
	sm.mu.Lock()
	for _, res := range sm.resources {
		if res.Path != "" {
			registered[res.Path] = struct{}{}
		}
	}
	sm.mu.Unlock()

	// Scan layers dir.
	if sm.layerMgr != nil {
		layersDir := filepath.Join(sm.rootDir, "layers")
		_ = filepath.WalkDir(layersDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".qcow2") {
				return nil
			}
			if _, ok := registered[path]; !ok {
				report.OrphanedOverlays = append(report.OrphanedOverlays, path)
				report.WouldDelete = append(report.WouldDelete, path)
			}
			return nil
		})
	}
}

// gcOverlay evaluates an overlay resource for orphanhood.
func (sm *StorageManager) gcOverlay(res *StorageResource, snapSet map[string]struct{}, report *GCReport) {
	// Active overlay with a valid VM owner -> keep.
	if res.State == StateActive && res.VMID != "" {
		return
	}
	// Active snapshot overlay with valid snapshot -> keep.
	if res.State == StateActive && res.SnapshotID != "" {
		if _, ok := snapSet[res.SnapshotID]; ok {
			return
		}
		// Snapshot metadata gone -> orphaned.
		report.OrphanedSnapshotOverlays = append(report.OrphanedSnapshotOverlays, res.Path)
		report.WouldDelete = append(report.WouldDelete, res.Path)
		return
	}
	// Failed/stopping/deleting overlay -> candidate for removal.
	if res.State != StateActive {
		report.OrphanedOverlays = append(report.OrphanedOverlays, res.Path)
		report.WouldDelete = append(report.WouldDelete, res.Path)
	}
}

// gcVMResource evaluates a VM resource for orphanhood.
func (sm *StorageManager) gcVMResource(res *StorageResource, report *GCReport) {
	// VM resources in active state are assumed to have a live VM (storage
	// alone cannot confirm process liveness). Only failed VMs are candidates.
	if res.State == StateActive {
		return
	}
	report.OrphanedVMResources = append(report.OrphanedVMResources, res.VMID)
}

// gcBackingResource evaluates a backing image resource. Backing images are
// never deleted by GC (they are shared and reference-counted); only failed
// ones are reported.
func (sm *StorageManager) gcBackingResource(res *StorageResource, report *GCReport) {
	if res.State == StateFailed {
		report.Errors = append(report.Errors, fmt.Errorf("backing image %q in failed state", res.Path))
	}
}

// ReconcileNBD delegates to the NBD manager to detect and clean stale NBD
// mappings. It is part of storage recovery.
func (sm *StorageManager) ReconcileNBD(ctx context.Context) (*NBDReconcileReport, error) {
	if sm.nbdMgr == nil {
		return nil, fmt.Errorf("NBD manager not configured")
	}
	return sm.nbdMgr.ReconcileNBD(ctx)
}

// Recover reconciles persistent storage metadata with actual filesystem and NBD
// state after an unexpected VMM restart. It:
//
//   - Reconciles stale NBD mappings (dead qemu-nbd processes).
//   - Marks overlay resources whose files are missing as failed.
//   - Marks VM resources whose overlays are missing as failed.
//   - Marks resources stuck in creating/deleting states as failed.
//   - Does NOT automatically delete snapshots or active VM storage.
//
// It does not automatically delete snapshots or active VM storage. Resources
// that cannot be reconciled are left in failed/orphaned state for operator
// review. Call CollectGarbage to reclaim proven-unused resources afterward.
func (sm *StorageManager) Recover(ctx context.Context) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// 1. Reconcile NBD mappings.
	if sm.nbdMgr != nil {
		if _, err := sm.nbdMgr.ReconcileNBD(ctx); err != nil {
			sm.log("warn", "NBD reconciliation warning", map[string]any{"error": err.Error()})
		}
	}

	// 2. Reload all resources from disk.
	allRes, err := sm.metaStore.List()
	if err != nil {
		return fmt.Errorf("list storage resources: %w", err)
	}
	sm.resources = make(map[string]*StorageResource, len(allRes))
	sm.vmOverlays = make(map[string]string, len(allRes))
	sm.snapOverlays = make(map[string]string, len(allRes))
	sm.templateVMs = make(map[string]map[string]struct{})
	for _, r := range allRes {
		sm.resources[r.ID] = r
		if r.Type == ResourceTypeOverlay && r.VMID != "" {
			sm.vmOverlays[r.VMID] = r.ID
		}
		if r.Type == ResourceTypeOverlay && r.SnapshotID != "" {
			sm.snapOverlays[r.SnapshotID] = r.ID
		}
		if r.Type == ResourceTypeVM && r.TemplateID != "" {
			if sm.templateVMs[r.TemplateID] == nil {
				sm.templateVMs[r.TemplateID] = make(map[string]struct{})
			}
			sm.templateVMs[r.TemplateID][r.VMID] = struct{}{}
		}
	}

	// 3. Mark resources stuck in creating state (interrupted operations) as failed.
	for _, res := range allRes {
		if res.State == StateCreating {
			err := sm.metaStore.Update(res.ID, func(r *StorageResource) error {
				if r.State == StateCreating {
					r.State = StateFailed
					r.UpdatedAt = time.Now()
				}
				return nil
			})
			if err != nil {
				sm.log("warn", "failed to mark resource as failed", map[string]any{
					"resource_id":   res.ID,
					"resource_type": res.Type,
					"error":         err.Error(),
				})
				continue
			}
			sm.log("warn", "resource stuck in creating state, marked failed", map[string]any{
				"resource_id":   res.ID,
				"resource_type": res.Type,
				"vm_id":         res.VMID,
				"snapshot_id":   res.SnapshotID,
				"path":          res.Path,
			})
		}
	}

	// 4. Mark resources stuck in deleting state (interrupted deletion) as failed.
	for _, res := range allRes {
		if res.State == StateDeleting {
			err := sm.metaStore.Update(res.ID, func(r *StorageResource) error {
				if r.State == StateDeleting {
					r.State = StateFailed
					r.UpdatedAt = time.Now()
				}
				return nil
			})
			if err != nil {
				sm.log("warn", "failed to mark resource as failed", map[string]any{
					"resource_id":   res.ID,
					"resource_type": res.Type,
					"error":         err.Error(),
				})
				continue
			}
			sm.log("warn", "resource stuck in deleting state, marked failed", map[string]any{
				"resource_id":   res.ID,
				"resource_type": res.Type,
				"vm_id":         res.VMID,
				"snapshot_id":   res.SnapshotID,
				"path":          res.Path,
			})
		}
	}

	// 5. Check overlay files exist; mark missing as failed.
	for _, res := range allRes {
		if res.Type != ResourceTypeOverlay || res.Path == "" {
			continue
		}
		if _, err := os.Stat(res.Path); err != nil {
			if os.IsNotExist(err) {
				err := sm.metaStore.Update(res.ID, func(r *StorageResource) error {
					if r.State == StateActive {
						r.State = StateFailed
						r.UpdatedAt = time.Now()
					}
					return nil
				})
				if err != nil {
					sm.log("warn", "failed to mark overlay as failed", map[string]any{
						"resource_id": res.ID,
						"error":       err.Error(),
					})
					continue
				}
				sm.log("warn", "overlay file missing, marked failed", map[string]any{
					"vm_id":        res.VMID,
					"snapshot_id":  res.SnapshotID,
					"overlay_path": res.Path,
				})
			}
		}
	}

	// 6. Mark VM resources as failed if their overlay is missing/failed.
	for _, res := range allRes {
		if res.Type != ResourceTypeVM {
			continue
		}
		overlayResID := sm.vmOverlays[res.VMID]
		if overlayResID == "" {
			continue
		}
		overlayRes := sm.resources[overlayResID]
		if overlayRes == nil {
			overlayRes = sm.metaStore.Get(overlayResID)
		}
		if overlayRes != nil && (overlayRes.State == StateFailed || overlayRes.State == StateDeleted) {
			err := sm.metaStore.Update(res.ID, func(r *StorageResource) error {
				if r.State == StateActive {
					r.State = StateFailed
					r.UpdatedAt = time.Now()
				}
				return nil
			})
			if err != nil {
				sm.log("warn", "failed to mark VM as failed", map[string]any{
					"resource_id": res.ID,
					"error":       err.Error(),
				})
				continue
			}
			sm.log("warn", "VM marked failed due to missing overlay", map[string]any{
				"vm_id": res.VMID,
			})
		}
	}

	sm.log("info", "storage recovery complete", map[string]any{
		"resources": len(allRes),
	})
	return nil
}

var _ = context.Background

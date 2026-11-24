package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// VolumeState describes the lifecycle state of a persistent volume.
type VolumeState string

const (
	// VolumeStateCreating marks a volume whose creation is in flight.
	VolumeStateCreating VolumeState = "creating"
	// VolumeStateActive marks a volume that exists but is not attached.
	VolumeStateActive VolumeState = "active"
	// VolumeStateAttached marks a volume currently attached to a VM.
	VolumeStateAttached VolumeState = "attached"
	// VolumeStateDeleting marks a volume whose deletion is in flight.
	VolumeStateDeleting VolumeState = "deleting"
	// VolumeStateFailed marks a volume whose last operation failed.
	VolumeStateFailed VolumeState = "failed"
	// VolumeStateDeleted marks a volume that has been removed.
	VolumeStateDeleted VolumeState = "deleted"
)

// Volume is a persistent, writable block storage resource that can be
// attached to a VM. Volumes are stored as qcow2 images and survive VM
// destruction. A volume may be attached to at most one VM at a time.
type Volume struct {
	// ID is the unique volume identifier.
	ID string `json:"id"`

	// SizeBytes is the volume capacity in bytes.
	SizeBytes int64 `json:"size_bytes"`

	// Format is the volume image format ("qcow2").
	Format string `json:"format"`

	// Path is the on-disk path to the volume's qcow2 image file.
	Path string `json:"path"`

	// State is the volume lifecycle state.
	State VolumeState `json:"state"`

	// CreatedAt is the time the volume was created.
	CreatedAt time.Time `json:"created_at"`

	// AttachedVMID is the ID of the VM this volume is attached to, if any.
	AttachedVMID string `json:"attached_vm_id,omitempty"`

	// AttachedDevice is the NBD device path the volume is mapped to, if attached.
	AttachedDevice string `json:"attached_device,omitempty"`

	// AttachedAt is the time the volume was attached, if currently attached.
	AttachedAt time.Time `json:"attached_at,omitempty"`

	// Metadata is optional user-defined key-value metadata.
	Metadata map[string]string `json:"metadata,omitempty"`

	// Labels are optional user-defined key-value labels.
	Labels map[string]string `json:"labels,omitempty"`
}

// VolumeRef is a lightweight reference to a volume for VM attachment.
type VolumeRef struct {
	// VolumeID is the ID of the volume to attach.
	VolumeID string

	// MountPath is the guest-side path where the volume should be available.
	// This is advisory; the guest agent is responsible for mounting the NBD
	// device at this path inside the guest.
	MountPath string

	// ReadOnly controls whether the volume is attached read-only.
	ReadOnly bool
}

// VolumeConfig holds configuration for the VolumeManager.
type VolumeConfig struct {
	// RootDir is the storage root directory. Volume images and metadata are
	// stored under <RootDir>/volumes/ and <RootDir>/resources/ respectively.
	RootDir string

	// DefaultSizeBytes is the default volume size when not specified.
	DefaultSizeBytes int64

	// MaxSizeBytes is the maximum allowed volume size.
	MaxSizeBytes int64

	// MinSizeBytes is the minimum allowed volume size.
	MinSizeBytes int64
}

// DefaultVolumeConfig returns sensible default volume configuration.
func DefaultVolumeConfig(rootDir string) VolumeConfig {
	return VolumeConfig{
		RootDir:          rootDir,
		DefaultSizeBytes: 10 * 1024 * 1024 * 1024,  // 10 GiB
		MaxSizeBytes:     100 * 1024 * 1024 * 1024, // 100 GiB
		MinSizeBytes:     1 * 1024 * 1024 * 1024,   // 1 GiB
	}
}

// VolumeManager manages the lifecycle of persistent volumes. It creates qcow2
// volume images, tracks their metadata in the MetadataStore, and attaches/
// detaches them to VMs via the NBDManager.
type VolumeManager struct {
	mu        sync.Mutex
	cfg       VolumeConfig
	layerMgr  *LayerManager
	nbdMgr    NBDManager
	metaStore *MetadataStore

	// volumes is the in-memory index of all known volumes, rebuilt from the
	// metadata store on Recover.
	volumes map[string]*Volume
	// attachments maps VM ID -> Volume ID for reverse lookup of which volume
	// a VM has attached (one volume per VM for simplicity).
	attachments map[string]string
}

// NewVolumeManager creates a VolumeManager. layerMgr and nbdMgr may be nil for
// metadata-only usage, but are required for actual volume creation and attach.
func NewVolumeManager(cfg VolumeConfig, layerMgr *LayerManager, nbdMgr NBDManager, metaStore *MetadataStore) *VolumeManager {
	if cfg.MinSizeBytes <= 0 {
		cfg.MinSizeBytes = DefaultVolumeConfig(cfg.RootDir).MinSizeBytes
	}
	if cfg.MaxSizeBytes <= cfg.MinSizeBytes {
		cfg.MaxSizeBytes = DefaultVolumeConfig(cfg.RootDir).MaxSizeBytes
	}
	if cfg.DefaultSizeBytes == 0 {
		cfg.DefaultSizeBytes = DefaultVolumeConfig(cfg.RootDir).DefaultSizeBytes
	}
	if cfg.DefaultSizeBytes < cfg.MinSizeBytes {
		cfg.DefaultSizeBytes = cfg.MinSizeBytes
	}
	if cfg.DefaultSizeBytes > cfg.MaxSizeBytes {
		cfg.DefaultSizeBytes = cfg.MaxSizeBytes
	}

	return &VolumeManager{
		cfg:         cfg,
		layerMgr:    layerMgr,
		nbdMgr:      nbdMgr,
		metaStore:   metaStore,
		volumes:     make(map[string]*Volume),
		attachments: make(map[string]string),
	}
}

// EnsureDirs creates the volumes directory.
func (m *VolumeManager) EnsureDirs() error {
	if err := os.MkdirAll(m.volumeDir(), 0o750); err != nil {
		return err
	}
	if m.metaStore != nil {
		if err := m.metaStore.EnsureDirs(); err != nil {
			return err
		}
	}
	return nil
}

// volumeDir returns the directory containing volume qcow2 images.
func (m *VolumeManager) volumeDir() string {
	return filepath.Join(m.cfg.RootDir, "volumes")
}

// volumePath returns the on-disk path for a volume's qcow2 image.
func (m *VolumeManager) volumePath(volumeID string) string {
	return filepath.Join(m.volumeDir(), volumeID+".qcow2")
}

// validateVolumeID checks that a volume ID is safe to use as a filename
// component and does not allow path traversal.
func validateVolumeID(id string) error {
	if id == "" {
		return errors.New("volume id is required")
	}
	if filepath.Base(id) != id {
		return fmt.Errorf("volume id %q must not contain path separators", id)
	}
	if id == "." || id == ".." {
		return fmt.Errorf("volume id %q is reserved", id)
	}
	return nil
}

// validateSize checks that a volume size is within configured limits.
func (m *VolumeManager) validateSize(size int64) error {
	if size <= 0 {
		return errors.New("volume size must be positive")
	}
	if size < m.cfg.MinSizeBytes {
		return fmt.Errorf("volume size %d bytes is below minimum %d bytes", size, m.cfg.MinSizeBytes)
	}
	if size > m.cfg.MaxSizeBytes {
		return fmt.Errorf("volume size %d bytes exceeds maximum %d bytes", size, m.cfg.MaxSizeBytes)
	}
	return nil
}

// resourceID returns the metadata store resource ID for a volume.
func volumeResourceID(volumeID string) string {
	return "volume:" + volumeID
}

// CreateVolume creates a new persistent volume with the given ID and size.
// The volume is created as a qcow2 image and its metadata is persisted.
// If size is 0, the configured default size is used.
func (m *VolumeManager) CreateVolume(volumeID string, sizeBytes int64, metadata, labels map[string]string) (*Volume, error) {
	if err := validateVolumeID(volumeID); err != nil {
		return nil, err
	}

	if sizeBytes == 0 {
		sizeBytes = m.cfg.DefaultSizeBytes
	}
	if err := m.validateSize(sizeBytes); err != nil {
		return nil, err
	}

	m.mu.Lock()
	if _, exists := m.volumes[volumeID]; exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("volume %q already exists", volumeID)
	}
	m.mu.Unlock()

	// Register the volume in the metadata store in the creating state.
	res := &StorageResource{
		ID:        volumeResourceID(volumeID),
		Type:      ResourceTypeVolume,
		State:     StateCreating,
		SizeBytes: sizeBytes,
		Labels:    labels,
	}
	if metadata != nil {
		res.Metadata = metadata
	}
	if err := m.metaStore.Register(res); err != nil {
		return nil, fmt.Errorf("register volume metadata: %w", err)
	}

	// Create the qcow2 volume image.
	volPath := m.volumePath(volumeID)
	if err := os.MkdirAll(m.volumeDir(), 0o750); err != nil {
		_ = m.failVolume(volumeID, fmt.Errorf("ensure volumes dir: %w", err))
		return nil, fmt.Errorf("ensure volumes dir: %w", err)
	}

	// Use qemu-img to create a qcow2 image of the specified size.
	tmpPath := volPath + ".tmp"
	defer os.Remove(tmpPath)

	// qemu-img create -o size=NNN creates a sparse qcow2 image.
	cmd := exec.Command("qemu-img", "create", "-f", "qcow2", "-o", fmt.Sprintf("size=%d", sizeBytes), tmpPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = m.failVolume(volumeID, fmt.Errorf("create qcow2 volume: %w\noutput: %s", err, string(out)))
		return nil, fmt.Errorf("create qcow2 volume: %w", err)
	}

	if err := os.Rename(tmpPath, volPath); err != nil {
		_ = m.failVolume(volumeID, fmt.Errorf("rename volume image: %w", err))
		return nil, fmt.Errorf("rename volume image: %w", err)
	}

	// Persist the active volume metadata.
	vol := &Volume{
		ID:        volumeID,
		SizeBytes: sizeBytes,
		Format:    "qcow2",
		Path:      volPath,
		State:     VolumeStateActive,
		CreatedAt: time.Now(),
		Metadata:  metadata,
		Labels:    labels,
	}

	if err := m.updateVolumeMetadata(vol); err != nil {
		_ = os.Remove(volPath)
		_ = m.failVolume(volumeID, fmt.Errorf("persist volume metadata: %w", err))
		return nil, fmt.Errorf("persist volume metadata: %w", err)
	}

	m.mu.Lock()
	m.volumes[volumeID] = vol
	m.mu.Unlock()

	return vol, nil
}

// InspectVolume returns the volume with the given ID, or nil if not found.
func (m *VolumeManager) InspectVolume(volumeID string) (*Volume, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if vol, ok := m.volumes[volumeID]; ok {
		return vol, nil
	}

	// Try loading from disk.
	res := m.metaStore.Get(volumeResourceID(volumeID))
	if res == nil {
		return nil, nil
	}
	return m.resourceToVolume(res), nil
}

// ListVolumes returns all known volumes.
func (m *VolumeManager) ListVolumes() ([]*Volume, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Start from the in-memory index (most up-to-date), then fill in any
	// volumes persisted to disk but not yet in memory.
	seen := make(map[string]bool)
	var volumes []*Volume
	for _, vol := range m.volumes {
		seen[vol.ID] = true
		volumes = append(volumes, vol)
	}

	resources, err := m.metaStore.List()
	if err != nil {
		return nil, err
	}
	for _, res := range resources {
		if res.Type != ResourceTypeVolume {
			continue
		}
		vid := strings.TrimPrefix(res.ID, "volume:")
		if seen[vid] {
			continue
		}
		vol := m.resourceToVolume(res)
		volumes = append(volumes, vol)
	}
	return volumes, nil
}

// DeleteVolume deletes a volume. It fails if the volume is currently attached
// to a VM. The volume image file and metadata are removed.
func (m *VolumeManager) DeleteVolume(volumeID string) error {
	m.mu.Lock()
	vol, ok := m.volumes[volumeID]
	if !ok {
		// Try loading from disk.
		res := m.metaStore.Get(volumeResourceID(volumeID))
		if res == nil {
			m.mu.Unlock()
			return fmt.Errorf("volume %q not found", volumeID)
		}
		vol = m.resourceToVolume(res)
	}
	if vol.State == VolumeStateAttached {
		m.mu.Unlock()
		return fmt.Errorf("volume %q is attached to VM %q; detach before deleting", volumeID, vol.AttachedVMID)
	}
	// Mark as deleting.
	vol.State = VolumeStateDeleting
	if err := m.updateVolumeMetadataLocked(vol); err != nil {
		m.mu.Unlock()
		return fmt.Errorf("mark volume deleting: %w", err)
	}
	delete(m.volumes, volumeID)
	// Remove any reverse attachment entry (attachments maps vmID->volumeID).
	for vmID, vid := range m.attachments {
		if vid == volumeID {
			delete(m.attachments, vmID)
			break
		}
	}
	m.mu.Unlock()

	// Remove the volume image file.
	if err := os.Remove(vol.Path); err != nil && !os.IsNotExist(err) {
		_ = m.failVolume(volumeID, fmt.Errorf("remove volume image: %w", err))
		return fmt.Errorf("remove volume image: %w", err)
	}

	// Delete metadata.
	if err := m.metaStore.Delete(volumeResourceID(volumeID)); err != nil {
		return fmt.Errorf("delete volume metadata: %w", err)
	}

	return nil
}

// AttachVolume attaches a volume to a VM by mapping its qcow2 image to an NBD
// block device. A volume may only be attached to one VM at a time; attaching
// an already-attached volume returns an error.
//
// ctx is used to cancel the NBD mapping operation. On cancellation, any
// partially created mapping is cleaned up.
func (m *VolumeManager) AttachVolume(ctx context.Context, volumeID, vmID string) (string, error) {
	if err := validateVolumeID(volumeID); err != nil {
		return "", err
	}
	if vmID == "" {
		return "", errors.New("vm id is required")
	}
	if m.nbdMgr == nil {
		return "", errors.New("nbd manager not configured")
	}

	m.mu.Lock()
	vol, ok := m.volumes[volumeID]
	if !ok {
		m.mu.Unlock()
		return "", fmt.Errorf("volume %q not found", volumeID)
	}
	if vol.State == VolumeStateAttached {
		m.mu.Unlock()
		return "", fmt.Errorf("volume %q is already attached to VM %q", volumeID, vol.AttachedVMID)
	}
	// Check that this VM doesn't already have a volume attached.
	if _, attached := m.attachments[vmID]; attached {
		m.mu.Unlock()
		return "", fmt.Errorf("VM %q already has a volume attached", vmID)
	}
	// Mark as attaching to prevent concurrent attaches (TOCTOU protection).
	vol.State = VolumeStateCreating // reuse "creating" as transient attaching state
	if err := m.updateVolumeMetadataLocked(vol); err != nil {
		m.mu.Unlock()
		return "", fmt.Errorf("mark volume attaching: %w", err)
	}
	m.mu.Unlock()

	// Map the volume image to an NBD device.
	devicePath, err := m.nbdMgr.Map(ctx, vol.Path, vmID)
	if err != nil {
		// Revert the attaching state back to active.
		m.mu.Lock()
		vol2 := m.volumes[volumeID]
		if vol2 != nil && vol2.State == VolumeStateCreating {
			vol2.State = VolumeStateActive
			_ = m.updateVolumeMetadataLocked(vol2)
		}
		m.mu.Unlock()
		return "", fmt.Errorf("map volume to NBD device: %w", err)
	}

	// Update volume metadata to attached state.
	m.mu.Lock()
	vol = m.volumes[volumeID] // reload in case it changed
	if vol == nil {
		// Volume was deleted while we were mapping; unmap the NBD device.
		m.mu.Unlock()
		_ = m.nbdMgr.UnmapContext(ctx, vmID)
		return "", fmt.Errorf("volume %q was deleted during attach", volumeID)
	}
	if vol.State == VolumeStateAttached {
		// Another goroutine attached this volume while we were mapping;
		// unmap our device and return an error.
		m.mu.Unlock()
		_ = m.nbdMgr.UnmapContext(ctx, vmID)
		return "", fmt.Errorf("volume %q is already attached to VM %q", volumeID, vol.AttachedVMID)
	}
	vol.State = VolumeStateAttached
	vol.AttachedVMID = vmID
	vol.AttachedDevice = devicePath
	vol.AttachedAt = time.Now()
	if err := m.updateVolumeMetadataLocked(vol); err != nil {
		m.mu.Unlock()
		_ = m.nbdMgr.UnmapContext(ctx, vmID)
		return "", fmt.Errorf("persist attach metadata: %w", err)
	}
	m.attachments[vmID] = volumeID
	m.mu.Unlock()

	return devicePath, nil
}

// DetachVolume detaches a volume from its currently attached VM by unmapping
// the NBD device.
func (m *VolumeManager) DetachVolume(volumeID string) error {
	m.mu.Lock()
	vol, ok := m.volumes[volumeID]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("volume %q not found", volumeID)
	}
	if vol.State != VolumeStateAttached || vol.AttachedVMID == "" {
		m.mu.Unlock()
		return fmt.Errorf("volume %q is not attached", volumeID)
	}
	vmID := vol.AttachedVMID
	devicePath := vol.AttachedDevice
	m.mu.Unlock()

	// Unmap the NBD device first. If this fails, we return an error WITHOUT
	// updating metadata, so the volume remains in "attached" state and the
	// operation can be retried safely.
	if m.nbdMgr != nil {
		if err := m.nbdMgr.UnmapContext(context.Background(), vmID); err != nil {
			return fmt.Errorf("unmap volume from NBD device: %w", err)
		}
	}

	// Update volume metadata to active state.
	m.mu.Lock()
	vol = m.volumes[volumeID]
	if vol != nil {
		vol.State = VolumeStateActive
		vol.AttachedVMID = ""
		vol.AttachedDevice = ""
		vol.AttachedAt = time.Time{}
		if err := m.updateVolumeMetadataLocked(vol); err != nil {
			m.mu.Unlock()
			// Metadata update failed after successful unmap. The NBD device
			// is unmapped but metadata still says "attached". This is an
			// inconsistent state that requires operator intervention or a
			// subsequent Recover() to reconcile.
			return fmt.Errorf("persist detach metadata: %w", err)
		}
	}
	delete(m.attachments, vmID)
	m.mu.Unlock()

	_ = devicePath
	return nil
}

// DetachVMVolumes detaches all volumes attached to the given VM.
func (m *VolumeManager) DetachVMVolumes(vmID string) error {
	m.mu.Lock()
	volumeID, ok := m.attachments[vmID]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	return m.DetachVolume(volumeID)
}

// GetVolumeForVM returns the volume attached to the given VM, or nil if none.
func (m *VolumeManager) GetVolumeForVM(vmID string) (*Volume, error) {
	m.mu.Lock()
	volumeID, ok := m.attachments[vmID]
	m.mu.Unlock()
	if !ok {
		return nil, nil
	}
	return m.InspectVolume(volumeID)
}

// updateVolumeMetadata persists volume metadata via the metadata store.
// Caller must hold m.mu.
func (m *VolumeManager) updateVolumeMetadataLocked(vol *Volume) error {
	res := &StorageResource{
		ID:        volumeResourceID(vol.ID),
		Type:      ResourceTypeVolume,
		Path:      vol.Path,
		SizeBytes: vol.SizeBytes,
		Labels:    vol.Labels,
		Metadata:  vol.Metadata,
	}
	// Preserve created time from existing resource if updating.
	if existing := m.metaStore.Get(volumeResourceID(vol.ID)); existing != nil {
		res.CreatedAt = existing.CreatedAt
		res.UpdatedAt = time.Now()
	} else {
		res.CreatedAt = vol.CreatedAt
		res.UpdatedAt = vol.CreatedAt
	}
	return m.metaStore.Update(volumeResourceID(vol.ID), func(r *StorageResource) error {
		r.Path = vol.Path
		r.SizeBytes = vol.SizeBytes
		r.State = VolumeLifecycleState(vol.State)
		r.Labels = vol.Labels
		r.Metadata = vol.Metadata
		r.UpdatedAt = time.Now()
		return nil
	})
}

// updateVolumeMetadata is a convenience wrapper that acquires the lock.
func (m *VolumeManager) updateVolumeMetadata(vol *Volume) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.updateVolumeMetadataLocked(vol)
}

// VolumeLifecycleState converts a VolumeState to a StorageResource LifecycleState.
func VolumeLifecycleState(vs VolumeState) LifecycleState {
	switch vs {
	case VolumeStateCreating:
		return StateCreating
	case VolumeStateActive:
		return StateActive
	case VolumeStateAttached:
		return StateActive // NBD attachment is runtime state, not persisted as separate lifecycle
	case VolumeStateDeleting:
		return StateDeleting
	case VolumeStateFailed:
		return StateFailed
	case VolumeStateDeleted:
		return StateDeleted
	default:
		return StateActive
	}
}

// LifecycleStateToVolumeState converts a StorageResource LifecycleState to a VolumeState.
// NBD attachment state is NOT persisted; volumes recovered from metadata are
// always "active" regardless of what the metadata state says, because the NBD
// mapping may have been lost across a process restart.
func LifecycleStateToVolumeState(ls LifecycleState) VolumeState {
	switch ls {
	case StateCreating:
		return VolumeStateCreating
	case StateActive:
		return VolumeStateActive
	case StateDeleting:
		return VolumeStateDeleting
	case StateFailed:
		return VolumeStateFailed
	case StateDeleted:
		return VolumeStateDeleted
	default:
		return VolumeStateActive
	}
}

// failVolume marks a volume as failed in the metadata store.
func (m *VolumeManager) failVolume(volumeID string, err error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if vol, ok := m.volumes[volumeID]; ok {
		vol.State = VolumeStateFailed
		_ = m.updateVolumeMetadataLocked(vol)
	}
	return err
}

// resourceToVolume converts a StorageResource to a Volume.
func (m *VolumeManager) resourceToVolume(res *StorageResource) *Volume {
	return &Volume{
		ID:        strings.TrimPrefix(res.ID, "volume:"),
		SizeBytes: res.SizeBytes,
		Format:    "qcow2",
		Path:      res.Path,
		State:     LifecycleStateToVolumeState(res.State),
		CreatedAt: res.CreatedAt,
		Metadata:  res.Metadata,
		Labels:    res.Labels,
	}
}

// Recover rebuilds in-memory state from the metadata store. Call after
// process restart to reconcile recorded volume state with actual state.
//
// Recovery does NOT trust metadata as proof that an NBD device is still
// attached: the NBD mapping is runtime state that may have been lost across
// a process restart. Volumes recorded as "attached" are recovered as "active"
// (not attached), and the caller should reconcile NBD state separately.
func (m *VolumeManager) Recover() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	resources, err := m.metaStore.List()
	if err != nil {
		return err
	}

	m.volumes = make(map[string]*Volume)
	m.attachments = make(map[string]string)

	for _, res := range resources {
		if res.Type != ResourceTypeVolume {
			continue
		}
		vol := m.resourceToVolume(res)
		m.volumes[vol.ID] = vol
		// Do NOT rebuild the attachment index from metadata. NBD mappings are
		// runtime state and may not survive a restart. The caller should
		// reconcile NBD state separately (e.g. via NBDManager.ReconcileNBD).
	}

	return nil
}

// Config returns the volume manager configuration.
func (m *VolumeManager) Config() VolumeConfig {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

// MetadataStore returns the shared MetadataStore used by this VolumeManager.
func (m *VolumeManager) MetadataStore() *MetadataStore {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.metaStore
}

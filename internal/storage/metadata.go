package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ResourceType classifies a persistent storage resource.
type ResourceType string

const (
	ResourceTypeTemplate ResourceType = "template"
	ResourceTypeBacking  ResourceType = "backing"
	ResourceTypeOverlay  ResourceType = "overlay"
	ResourceTypeSnapshot ResourceType = "snapshot"
	ResourceTypeVM       ResourceType = "vm"
	ResourceTypeVolume   ResourceType = "volume"
	ResourceTypeImage    ResourceType = "image"
)

// LifecycleState describes the lifecycle state of a storage resource.
type LifecycleState string

const (
	// StateCreating marks a resource whose creation is in flight.
	StateCreating LifecycleState = "creating"
	// StateActive marks a resource actively in use.
	StateActive LifecycleState = "active"
	// StateStopping marks a resource being released by its owner.
	StateStopping LifecycleState = "stopping"
	// StateDeleting marks a resource whose deletion is in flight.
	StateDeleting LifecycleState = "deleting"
	// StateOrphaned marks a resource with no valid owner.
	StateOrphaned LifecycleState = "orphaned"
	// StateFailed marks a resource whose last operation failed.
	StateFailed LifecycleState = "failed"
	// StateDeleted marks a resource that has been removed.
	StateDeleted LifecycleState = "deleted"
)

// StorageResource records durable metadata for a storage resource.
// Metadata is persisted atomically (temp file + rename) so interrupted writes
// never leave partially published metadata behind.
type StorageResource struct {
	// ID is the unique resource identifier.
	ID string `json:"id"`

	// Type is the resource type.
	Type ResourceType `json:"type"`

	// Path is the on-disk path to the resource artifact, if any.
	Path string `json:"path,omitempty"`

	// VMID is the ID of the VM that owns this resource, if any.
	VMID string `json:"vm_id,omitempty"`

	// SnapshotID is the ID of the snapshot that references this resource, if any.
	SnapshotID string `json:"snapshot_id,omitempty"`

	// TemplateID is the ID of the template this resource derives from, if any.
	TemplateID string `json:"template_id,omitempty"`

	// CreatedAt is the time the resource was first registered.
	CreatedAt time.Time `json:"created_at"`

	// UpdatedAt is the time the resource was last modified.
	UpdatedAt time.Time `json:"updated_at"`

	// State is the resource lifecycle state.
	State LifecycleState `json:"state"`

	// BackingImage is the path to the backing image this resource depends on, if any.
	BackingImage string `json:"backing_image,omitempty"`

	// ReferenceCount is the number of active owners of this resource.
	ReferenceCount int `json:"reference_count"`

	// SizeBytes is the resource capacity in bytes, if applicable (volumes).
	SizeBytes int64 `json:"size_bytes,omitempty"`

	// Labels are optional user-defined key-value labels.
	Labels map[string]string `json:"labels,omitempty"`

	// Metadata is optional user-defined key-value metadata.
	Metadata map[string]string `json:"metadata,omitempty"`
}

// resourceMetadataDir holds persistent storage resource metadata files.
const resourceMetadataDir = "resources"

// MetadataStore persists StorageResource metadata on disk. It is safe for
// concurrent use. Metadata survives process restarts so recovery can reconcile
// recorded state with actual filesystem/NBD/process state.
type MetadataStore struct {
	dir   string
	mu    sync.Mutex
	cache map[string]*StorageResource
}

// NewMetadataStore creates a MetadataStore that persists resources under
// <root>/resources/.
func NewMetadataStore(root string) *MetadataStore {
	return &MetadataStore{
		dir:   filepath.Join(root, resourceMetadataDir),
		cache: make(map[string]*StorageResource),
	}
}

// EnsureDirs creates the metadata directory.
func (s *MetadataStore) EnsureDirs() error {
	return os.MkdirAll(s.dir, 0o750)
}

// Register persists a new resource in the creating state. Returns an error if
// the ID already exists.
func (s *MetadataStore) Register(res *StorageResource) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.cache[res.ID]; ok {
		return fmt.Errorf("resource %q already registered", res.ID)
	}
	now := time.Now()
	res.CreatedAt = now
	res.UpdatedAt = now
	if res.State == "" {
		res.State = StateCreating
	}
	s.cache[res.ID] = res
	return s.writeLocked(res)
}

// Get returns the resource for id, or nil if not found. Metadata is read from
// disk so the store remains valid across process restarts.
func (s *MetadataStore) Get(id string) *StorageResource {
	s.mu.Lock()
	res, ok := s.cache[id]
	s.mu.Unlock()
	if ok {
		return res
	}
	return s.getFromDisk(id)
}

func (s *MetadataStore) getFromDisk(id string) *StorageResource {
	data, err := os.ReadFile(s.path(id))
	if err != nil {
		return nil
	}
	var res StorageResource
	if err := json.Unmarshal(data, &res); err != nil {
		return nil
	}
	return &res
}

// List returns all registered resources.
func (s *MetadataStore) List() ([]*StorageResource, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var resources []*StorageResource
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		var res StorageResource
		if err := json.Unmarshal(data, &res); err != nil {
			continue
		}
		resources = append(resources, &res)
	}
	return resources, nil
}

// Update mutates fields of an existing resource atomically. The updater
// receives the current resource and returns the modified copy. Returns an error
// if the resource is not found.
func (s *MetadataStore) Update(id string, updater func(*StorageResource) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, ok := s.cache[id]
	if !ok {
		// Try loading from disk.
		res = s.getFromDisk(id)
		if res == nil {
			return fmt.Errorf("resource %q not found", id)
		}
		s.cache[id] = res
	}
	if err := updater(res); err != nil {
		return err
	}
	res.UpdatedAt = time.Now()
	return s.writeLocked(res)
}

// Delete removes a resource's metadata file. It is idempotent.
func (s *MetadataStore) Delete(id string) error {
	s.mu.Lock()
	delete(s.cache, id)
	s.mu.Unlock()
	if err := os.Remove(s.path(id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// path returns the metadata file path for a resource id.
func (s *MetadataStore) path(id string) string {
	return filepath.Join(s.dir, id+".json")
}

// writeLocked marshals res to a temp file then renames atomically. Caller must
// hold s.mu.
func (s *MetadataStore) writeLocked(res *StorageResource) error {
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	tmpPath := filepath.Join(s.dir, res.ID+".json.tmp")
	finalPath := s.path(res.ID)
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, finalPath)
}

var _ = sync.Mutex{}

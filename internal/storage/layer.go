package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/omkar/sandbox/internal/template"
)

const (
	// cowFormat is the qcow2 format used for backing images and writable layers.
	cowFormat = "qcow2"

	// layerStateActive marks a layer currently owned by a VM.
	layerStateActive = "active"
	// layerStateRemoved marks a layer whose VM has been destroyed.
	layerStateRemoved = "removed"
)

// LayerInfo records durable metadata for a writable copy-on-write layer.
type LayerInfo struct {
	// LayerID is the unique layer identifier (equal to the owning VM ID).
	LayerID string `json:"layer_id"`

	// VMID is the ID of the VM that owns this layer.
	VMID string `json:"vm_id"`

	// TemplateID is the immutable template the layer is derived from.
	TemplateID string `json:"template_id"`

	// BackingImagePath is the path to the shared immutable qcow2 backing image.
	BackingImagePath string `json:"backing_image_path"`

	// LayerPath is the path to the VM-private writable qcow2 overlay.
	LayerPath string `json:"layer_path"`

	// Format is the layer image format ("qcow2").
	Format string `json:"format"`

	// CreatedAt is the time the layer was created.
	CreatedAt time.Time `json:"created_at"`

	// State is the layer lifecycle state ("active" or "removed").
	State string `json:"state"`
}

// LayerManager manages copy-on-write storage for VMs built from immutable
// templates. It maintains one immutable qcow2 backing image per template and
// one writable qcow2 overlay per VM. Each overlay declares the backing image as
// its qcow2 backing file, so Firecracker serves reads from the shared base and
// directs writes to the VM-private overlay. The base image is never opened for
// write by a VM.
type LayerManager struct {
	rootDir       string
	templateStore *template.Store
	mu            sync.Mutex
	// creating tracks in-flight backing-image conversions per template so
	// concurrent VM startups do not convert the same backing image twice.
	creating map[string]chan struct{}
}

// NewLayerManager creates a COW layer manager. rootDir must be absolute.
func NewLayerManager(rootDir string, ts *template.Store) *LayerManager {
	abs, err := filepath.Abs(rootDir)
	if err != nil {
		abs = rootDir
	}
	return &LayerManager{
		rootDir:       abs,
		templateStore: ts,
		creating:      make(map[string]chan struct{}),
	}
}

// EnsureDirs creates the backing image, layer, and metadata directories.
func (m *LayerManager) EnsureDirs() error {
	for _, d := range []string{
		filepath.Join(m.rootDir, backingDir),
		filepath.Join(m.rootDir, layersDir),
		filepath.Join(m.rootDir, metadataDir),
	} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return fmt.Errorf("create dir %q: %w", d, err)
		}
	}
	return nil
}

// BackingImagePath returns the path to the immutable qcow2 backing image for a
// template.
func (m *LayerManager) BackingImagePath(templateID string) string {
	return filepath.Join(m.rootDir, backingDir, templateID+".qcow2")
}

// LayerPath returns the path to a VM's writable qcow2 overlay.
func (m *LayerManager) LayerPath(vmID string) string {
	return filepath.Join(m.rootDir, layersDir, vmID+".qcow2")
}

func (m *LayerManager) metadataPath(vmID string) string {
	return filepath.Join(m.rootDir, metadataDir, vmID+".json")
}

// dirLayout returns the on-disk subdirectory names.
const (
	backingDir  = "backing"
	layersDir   = "layers"
	metadataDir = "metadata"
)

// EnsureBackingImage creates (or reuses) the immutable qcow2 backing image for
// a template by converting its ext4 rootfs. Only one conversion runs per
// template at a time; concurrent callers wait for the first to finish. The
// resulting image is chmod read-only and never opened for write by a VM.
func (m *LayerManager) EnsureBackingImage(templateID string) (string, error) {
	if m.templateStore == nil {
		return "", fmt.Errorf("template store not configured")
	}

	tmpl := m.templateStore.Get(templateID)
	if tmpl == nil {
		return "", fmt.Errorf("template %q not found", templateID)
	}

	finalPath := m.BackingImagePath(templateID)
	if _, err := os.Stat(finalPath); err == nil {
		return finalPath, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}

	// Serialize conversions per template so concurrent VM startups share one
	// backing image instead of converting it multiple times.
	m.mu.Lock()
	ch, creating := m.creating[templateID]
	if creating {
		m.mu.Unlock()
		select {
		case <-ch:
		case <-time.After(2 * time.Minute):
			return "", fmt.Errorf("timeout waiting for backing image %q", templateID)
		}
		if _, err := os.Stat(finalPath); err == nil {
			return finalPath, nil
		}
		return "", fmt.Errorf("backing image %q not available after concurrent creation", templateID)
	}
	ch = make(chan struct{})
	m.creating[templateID] = ch
	m.mu.Unlock()

	// Signal waiters and clean up the in-flight marker when conversion ends.
	defer func() {
		m.mu.Lock()
		delete(m.creating, templateID)
		close(ch)
		m.mu.Unlock()
	}()

	tmpPath := finalPath + ".tmp"
	defer os.Remove(tmpPath)

	cmd := exec.Command("qemu-img", "convert", "-O", cowFormat, tmpl.RootfsPath, tmpPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("convert template rootfs to qcow2 backing: %w\noutput: %s", err, string(out))
	}

	// Defense in depth: the backing image is immutable and read-only.
	if err := os.Chmod(tmpPath, 0o444); err != nil {
		return "", fmt.Errorf("chmod backing image read-only: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := os.Stat(finalPath); err == nil {
		// Another goroutine finished first; discard our temporary copy.
		return finalPath, nil
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return "", fmt.Errorf("rename backing image: %w", err)
	}
	return finalPath, nil
}

// CreateLayer creates a writable qcow2 overlay for vmID derived from the given
// template's backing image. The overlay is created atomically (temp file +
// rename) and its metadata is persisted durably. The template backing image is
// only read, never modified. Returns the overlay path.
func (m *LayerManager) CreateLayer(templateID, vmID string) (string, error) {
	if vmID == "" || filepath.Base(vmID) != vmID {
		return "", fmt.Errorf("invalid vm id %q", vmID)
	}
	if templateID == "" || filepath.Base(templateID) != templateID {
		return "", fmt.Errorf("invalid template id %q", templateID)
	}

	backingPath, err := m.EnsureBackingImage(templateID)
	if err != nil {
		return "", fmt.Errorf("ensure backing image: %w", err)
	}

	if err := os.MkdirAll(filepath.Join(m.rootDir, layersDir), 0o750); err != nil {
		return "", fmt.Errorf("create layers dir: %w", err)
	}

	layerPath := m.LayerPath(vmID)
	tmpPath := layerPath + ".tmp"
	defer os.Remove(tmpPath)

	// Create a qcow2 overlay with the backing image as its parent. Writes go to
	// the overlay; reads fall back to the shared read-only backing image.
	cmd := exec.Command("qemu-img", "create", "-f", cowFormat, "-b", backingPath, "-F", cowFormat, tmpPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("create qcow2 overlay: %w\noutput: %s", err, string(out))
	}

	if err := os.Rename(tmpPath, layerPath); err != nil {
		return "", fmt.Errorf("rename overlay: %w", err)
	}

	info := &LayerInfo{
		LayerID:          vmID,
		VMID:             vmID,
		TemplateID:       templateID,
		BackingImagePath: backingPath,
		LayerPath:        layerPath,
		Format:           cowFormat,
		CreatedAt:        time.Now(),
		State:            layerStateActive,
	}
	metaTmp := m.metadataPath(vmID) + ".tmp"
	metaFinal := m.metadataPath(vmID)
	if err := writeJSONAtomically(metaTmp, metaFinal, info); err != nil {
		_ = os.Remove(layerPath)
		return "", fmt.Errorf("write layer metadata: %w", err)
	}

	return layerPath, nil
}

// RemoveLayer removes a VM's writable overlay and its metadata. It is
// idempotent: removing a missing layer is not an error. Only resources owned by
// the VM are removed; the shared backing image is never touched.
func (m *LayerManager) RemoveLayer(vmID string) error {
	metaPath := m.metadataPath(vmID)
	if err := os.Remove(metaPath); err != nil && !os.IsNotExist(err) {
		return err
	}

	layerPath := m.LayerPath(vmID)
	if err := os.Remove(layerPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// LayerMetadata returns the durable metadata for a VM's layer, or nil if none.
func (m *LayerManager) LayerMetadata(vmID string) *LayerInfo {
	data, err := os.ReadFile(m.metadataPath(vmID))
	if err != nil {
		return nil
	}
	var info LayerInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil
	}
	return &info
}

// ListLayers returns metadata for all known layers.
func (m *LayerManager) ListLayers() ([]*LayerInfo, error) {
	entries, err := os.ReadDir(filepath.Join(m.rootDir, metadataDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var layers []*LayerInfo
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(m.rootDir, metadataDir, e.Name()))
		if err != nil {
			continue
		}
		var info LayerInfo
		if err := json.Unmarshal(data, &info); err != nil {
			continue
		}
		layers = append(layers, &info)
	}
	return layers, nil
}

// HasDependentLayers reports whether any active layer references the template.
// Used to protect backing images from deletion while layers exist.
func (m *LayerManager) HasDependentLayers(templateID string) bool {
	layers, err := m.ListLayers()
	if err != nil {
		return false
	}
	for _, l := range layers {
		if l.TemplateID == templateID && l.State == layerStateActive {
			return true
		}
	}
	return false
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

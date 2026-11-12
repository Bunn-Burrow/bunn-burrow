// Package image provides image management for Bunn Burrow execution
// environments. Images are immutable rootfs artifacts with metadata that can
// be versioned, referenced by templates, and used to boot VMs.
package image

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/omkar/sandbox/internal/storage"
)

// ImageConfig holds configuration for the ImageManager.
type ImageConfig struct {
	// RootDir is the storage root directory. Image files and metadata are
	// stored under <RootDir>/images/ and <RootDir>/resources/ respectively.
	RootDir string

	// MaxImageSizeBytes is the maximum allowed image size.
	MaxImageSizeBytes int64
}

// DefaultImageConfig returns sensible default image configuration.
func DefaultImageConfig(rootDir string) ImageConfig {
	return ImageConfig{
		RootDir:           rootDir,
		MaxImageSizeBytes: 50 * 1024 * 1024 * 1024, // 50 GiB
	}
}

// ImageManager manages the lifecycle of immutable rootfs images. It creates,
// registers, inspects, versions, and deletes images. Image metadata is
// persisted via the MetadataStore; image files are stored as immutable
// artifacts under the images directory.
type ImageManager struct {
	mu              sync.Mutex
	cfg             ImageConfig
	metaStore       *storage.MetadataStore
	templateChecker TemplateChecker

	// images is the in-memory index of all known images, rebuilt from the
	// metadata store on Recover.
	images map[string]*Image
}

// NewImageManager creates an ImageManager.
func NewImageManager(cfg ImageConfig, metaStore *storage.MetadataStore) *ImageManager {
	if cfg.MaxImageSizeBytes <= 0 {
		cfg.MaxImageSizeBytes = DefaultImageConfig(cfg.RootDir).MaxImageSizeBytes
	}
	return &ImageManager{
		cfg:       cfg,
		metaStore: metaStore,
		images:    make(map[string]*Image),
	}
}

// EnsureDirs creates the images directory.
func (m *ImageManager) EnsureDirs() error {
	return EnsureImageDirs(m.cfg.RootDir)
}

// SetTemplateChecker sets an optional template checker used to prevent
// deletion of images referenced by templates.
func (m *ImageManager) SetTemplateChecker(checker TemplateChecker) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.templateChecker = checker
}

// CreateImage creates a new image by copying the source file to the images
// directory. The image is immutable once created. If version is empty, "latest"
// is used.
//
// The sourceFile must be a readable filesystem path; it is copied atomically
// so a failed copy never leaves a partial image behind.
func (m *ImageManager) CreateImage(id, name, version string, sourceFile string, format ImageFormat, sizeBytes int64, metadata, labels map[string]string) (*Image, error) {
	if err := validateImageID(id); err != nil {
		return nil, err
	}
	if err := validateName(name); err != nil {
		return nil, err
	}
	if err := validateFormat(format); err != nil {
		return nil, err
	}
	if version == "" {
		version = "latest"
	}

	m.mu.Lock()
	if _, exists := m.images[id]; exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("image %q already exists", id)
	}
	// Check name+version uniqueness.
	vid := versionedImageID(name, version)
	if _, exists := m.images[vid]; exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("image %q@%q already exists", name, version)
	}
	m.mu.Unlock()

	if _, err := os.Stat(sourceFile); err != nil {
		return nil, fmt.Errorf("source file not statable: %w", err)
	}
	info, err := os.Stat(sourceFile)
	if err != nil {
		return nil, fmt.Errorf("stat source file: %w", err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("source file %q is a directory", sourceFile)
	}
	if sizeBytes <= 0 {
		sizeBytes = info.Size()
	}
	if sizeBytes > m.cfg.MaxImageSizeBytes {
		return nil, fmt.Errorf("image size %d bytes exceeds maximum %d bytes", sizeBytes, m.cfg.MaxImageSizeBytes)
	}

	// Register the image in the metadata store in the creating state.
	// Store name/version/format in metadata so they survive recovery.
	resMeta := map[string]string{}
	for k, v := range metadata {
		resMeta[k] = v
	}
	resMeta["name"] = name
	resMeta["version"] = version
	resMeta["format"] = string(format)

	res := &storage.StorageResource{
		ID:        imageResourceID(id),
		Type:      storage.ResourceTypeImage,
		State:     storage.StateCreating,
		SizeBytes: sizeBytes,
		Labels:    labels,
		Metadata:  resMeta,
	}
	if err := m.metaStore.Register(res); err != nil {
		return nil, fmt.Errorf("register image metadata: %w", err)
	}

	// Copy the source file to the images directory atomically.
	imgDir := imageDir(m.cfg.RootDir)
	if err := os.MkdirAll(imgDir, 0o750); err != nil {
		_ = m.failImage(id, fmt.Errorf("ensure images dir: %w", err))
		return nil, fmt.Errorf("ensure images dir: %w", err)
	}
	imgPath := imagePath(m.cfg.RootDir, id)
	tmpPath := imgPath + ".tmp"
	defer os.Remove(tmpPath)

	if err := copyFileAtomic(sourceFile, tmpPath, imgPath); err != nil {
		_ = m.failImage(id, fmt.Errorf("copy image file: %w", err))
		return nil, fmt.Errorf("copy image file: %w", err)
	}

	// Make the image read-only (defense in depth).
	if err := os.Chmod(imgPath, 0o444); err != nil {
		os.Remove(imgPath)
		_ = m.failImage(id, fmt.Errorf("chmod image read-only: %w", err))
		return nil, fmt.Errorf("chmod image read-only: %w", err)
	}

	// Compute checksum.
	checksum, err := sha256File(imgPath)
	if err != nil {
		os.Remove(imgPath)
		_ = m.failImage(id, fmt.Errorf("checksum image: %w", err))
		return nil, fmt.Errorf("checksum image: %w", err)
	}

	img := &Image{
		ID:        id,
		Name:      name,
		Version:   version,
		Format:    format,
		SizeBytes: sizeBytes,
		Path:      imgPath,
		Checksum:  checksum,
		State:     ImageStateActive,
		CreatedAt: time.Now(),
		Metadata:  metadata,
		Labels:    labels,
	}

	// Also register the versioned lookup key.
	if err := m.registerVersioned(img); err != nil {
		os.Remove(imgPath)
		_ = m.failImage(id, fmt.Errorf("register versioned image: %w", err))
		return nil, fmt.Errorf("register versioned image: %w", err)
	}

	if err := m.updateImageMetadata(img); err != nil {
		os.Remove(imgPath)
		_ = m.failImage(id, fmt.Errorf("persist image metadata: %w", err))
		return nil, fmt.Errorf("persist image metadata: %w", err)
	}

	m.mu.Lock()
	m.images[id] = img
	m.images[vid] = img // versioned lookup shares the same Image pointer
	m.mu.Unlock()

	return img, nil
}

// ImportImage imports an existing local file as an image. This is the primary
// source for locally-built rootfs images.
func (m *ImageManager) ImportImage(id, name, version, sourceFile string, format ImageFormat, metadata, labels map[string]string) (*Image, error) {
	return m.CreateImage(id, name, version, sourceFile, format, 0, metadata, labels)
}

// DeriveImage creates a new image by copying an existing image file. This
// supports image layering where a new image is derived from a base image.
func (m *ImageManager) DeriveImage(id, name, version, sourceImageID string, format ImageFormat, metadata, labels map[string]string) (*Image, error) {
	m.mu.Lock()
	source, ok := m.images[sourceImageID]
	if !ok {
		// Try versioned lookup (e.g. "ubuntu@22.04").
		parts := strings.SplitN(sourceImageID, "@", 2)
		if len(parts) == 2 {
			source, ok = m.images[versionedImageID(parts[0], parts[1])]
		}
		if !ok {
			m.mu.Unlock()
			return nil, fmt.Errorf("source image %q not found", sourceImageID)
		}
	}
	if source.State != ImageStateActive {
		m.mu.Unlock()
		return nil, fmt.Errorf("source image %q is not active (state=%s)", sourceImageID, source.State)
	}
	m.mu.Unlock()

	img, err := m.CreateImage(id, name, version, source.Path, format, source.SizeBytes, metadata, labels)
	if err != nil {
		return nil, err
	}
	// Set the parent reference after creation.
	m.mu.Lock()
	img.ParentID = source.ID
	_ = m.updateImageMetadataLocked(img)
	m.mu.Unlock()

	return img, nil
}

// InspectImage returns the image with the given ID, or nil if not found.
// The ID can be an image ID or a name@version reference.
func (m *ImageManager) InspectImage(id string) (*Image, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if img, ok := m.images[id]; ok {
		return img, nil
	}

	// Try loading from disk.
	res := m.metaStore.Get(imageResourceID(id))
	if res == nil {
		return nil, nil
	}
	return m.resourceToImage(res), nil
}

// InspectImageByNameVersion returns the image with the given name and version.
func (m *ImageManager) InspectImageByNameVersion(name, version string) (*Image, error) {
	return m.InspectImage(versionedImageID(name, version))
}

// ListImages returns all known images. Versioned lookup keys are deduplicated
// so each image appears once.
func (m *ImageManager) ListImages() ([]*Image, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	resources, err := m.metaStore.List()
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool)
	var images []*Image

	// Start from in-memory index (most up-to-date).
	for _, img := range m.images {
		if seen[img.ID] {
			continue
		}
		seen[img.ID] = true
		images = append(images, img)
	}

	// Fill in any images persisted to disk but not yet in memory.
	for _, res := range resources {
		if res.Type != storage.ResourceTypeImage {
			continue
		}
		id := strings.TrimPrefix(res.ID, "image:")
		// Skip versioned lookup keys (they share the same Image).
		if strings.Contains(id, "@") {
			continue
		}
		if seen[id] {
			continue
		}
		img := m.resourceToImage(res)
		seen[id] = true
		images = append(images, img)
	}
	return images, nil
}

// DeleteImage deletes an image. It fails if the image is referenced by any
// template. The image file and metadata are removed.
func (m *ImageManager) DeleteImage(id string) error {
	m.mu.Lock()
	img, ok := m.images[id]
	if !ok {
		// Try loading from disk.
		res := m.metaStore.Get(imageResourceID(id))
		if res == nil {
			m.mu.Unlock()
			return fmt.Errorf("image %q not found", id)
		}
		img = m.resourceToImage(res)
	}
	if img.State == ImageStateDeleting {
		m.mu.Unlock()
		return fmt.Errorf("image %q is already being deleted", id)
	}
	// Check if any template references this image.
	if m.templateChecker != nil {
		referenced, err := m.templateChecker.TemplateReferencesImage(id)
		if err != nil {
			m.mu.Unlock()
			return fmt.Errorf("check template references: %w", err)
		}
		if referenced {
			m.mu.Unlock()
			return fmt.Errorf("image %q is referenced by one or more templates; delete templates first", id)
		}
	}
	// Mark as deleting.
	img.State = ImageStateDeleting
	if err := m.updateImageMetadataLocked(img); err != nil {
		m.mu.Unlock()
		return fmt.Errorf("mark image deleting: %w", err)
	}
	delete(m.images, id)
	// Remove versioned lookup key.
	vid := versionedImageID(img.Name, img.Version)
	delete(m.images, vid)
	m.mu.Unlock()

	// Remove the image file.
	if err := os.Remove(img.Path); err != nil && !os.IsNotExist(err) {
		_ = m.failImage(id, fmt.Errorf("remove image file: %w", err))
		return fmt.Errorf("remove image file: %w", err)
	}

	// Delete versioned metadata.
	_ = m.metaStore.Delete(versionedImageID(img.Name, img.Version))

	// Delete metadata.
	if err := m.metaStore.Delete(imageResourceID(id)); err != nil {
		return fmt.Errorf("delete image metadata: %w", err)
	}

	return nil
}

// TemplateChecker is implemented by TemplateManager to check whether any
// template references a given image ID. Used to protect images from deletion
// while templates exist.
type TemplateChecker interface {
	// TemplateReferencesImage returns true if any template references the given
	// image ID.
	TemplateReferencesImage(imageID string) (bool, error)
}

// HasDependentTemplates reports whether any template references the given image ID.
// Used to protect images from deletion while templates exist.
func (m *ImageManager) HasDependentTemplates(imageID string, checker TemplateChecker) (bool, error) {
	return checker.TemplateReferencesImage(imageID)
}

// Recover rebuilds in-memory state from the metadata store. Call after process
// restart.
func (m *ImageManager) Recover() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	resources, err := m.metaStore.List()
	if err != nil {
		return err
	}

	m.images = make(map[string]*Image)

	// First pass: collect versioned key metadata (name, version, format) keyed
	// by base image ID. Versioned keys store complete metadata because
	// registerVersioned writes them with full metadata.
	versionedMeta := make(map[string]*storage.StorageResource)
	for _, res := range resources {
		if res.Type != storage.ResourceTypeImage {
			continue
		}
		id := strings.TrimPrefix(res.ID, "image:")
		if strings.Contains(id, "@") {
			versionedMeta[id] = res
		}
	}

	// Second pass: process base image keys. Use versioned key metadata to fill
	// in missing name/version when the base key has incomplete metadata.
	for _, res := range resources {
		if res.Type != storage.ResourceTypeImage {
			continue
		}
		id := strings.TrimPrefix(res.ID, "image:")
		if strings.Contains(id, "@") {
			continue
		}
		img := m.resourceToImage(res)
		// If name or version is missing, try to recover from the versioned key.
		if img.Name == "" || img.Version == "" {
			vid := versionedImageID(id, img.Version)
			if vres, ok := versionedMeta[vid]; ok {
				if img.Name == "" {
					img.Name = vres.Metadata["name"]
				}
				if img.Version == "" {
					img.Version = vres.Metadata["version"]
				}
				if img.Format == "" {
					img.Format = ImageFormat(vres.Metadata["format"])
				}
			}
		}
		m.images[img.ID] = img
		// Rebuild versioned lookup key.
		vid := versionedImageID(img.Name, img.Version)
		if vid != "image:@" {
			m.images[vid] = img
		}
	}

	return nil
}

// updateImageMetadata persists image metadata via the metadata store.
// Caller must hold m.mu.
func (m *ImageManager) updateImageMetadataLocked(img *Image) error {
	meta := map[string]string{}
	for k, v := range img.Metadata {
		meta[k] = v
	}
	meta["name"] = img.Name
	meta["version"] = img.Version
	meta["format"] = string(img.Format)

	res := &storage.StorageResource{
		ID:        imageResourceID(img.ID),
		Type:      storage.ResourceTypeImage,
		Path:      img.Path,
		SizeBytes: img.SizeBytes,
		Labels:    img.Labels,
		Metadata:  meta,
	}
	if existing := m.metaStore.Get(imageResourceID(img.ID)); existing != nil {
		res.CreatedAt = existing.CreatedAt
		res.UpdatedAt = time.Now()
	} else {
		res.CreatedAt = img.CreatedAt
		res.UpdatedAt = img.CreatedAt
	}
	return m.metaStore.Update(imageResourceID(img.ID), func(r *storage.StorageResource) error {
		r.Path = img.Path
		r.SizeBytes = img.SizeBytes
		r.State = imageStateToLifecycle(img.State)
		r.Labels = img.Labels
		r.Metadata = meta
		r.UpdatedAt = time.Now()
		return nil
	})
}

// updateImageMetadata is a convenience wrapper that acquires the lock.
func (m *ImageManager) updateImageMetadata(img *Image) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.updateImageMetadataLocked(img)
}

// registerVersioned registers the versioned lookup key for an image.
// Caller must hold m.mu.
func (m *ImageManager) registerVersioned(img *Image) error {
	meta := map[string]string{}
	for k, v := range img.Metadata {
		meta[k] = v
	}
	meta["name"] = img.Name
	meta["version"] = img.Version
	meta["format"] = string(img.Format)

	res := &storage.StorageResource{
		ID:        versionedImageID(img.Name, img.Version),
		Type:      storage.ResourceTypeImage,
		Path:      img.Path,
		SizeBytes: img.SizeBytes,
		Labels:    img.Labels,
		Metadata:  meta,
	}
	if existing := m.metaStore.Get(versionedImageID(img.Name, img.Version)); existing != nil {
		res.CreatedAt = existing.CreatedAt
		res.UpdatedAt = time.Now()
		return m.metaStore.Update(versionedImageID(img.Name, img.Version), func(r *storage.StorageResource) error {
			r.Path = img.Path
			r.SizeBytes = img.SizeBytes
			r.State = imageStateToLifecycle(img.State)
			r.Labels = img.Labels
			r.Metadata = meta
			r.UpdatedAt = time.Now()
			return nil
		})
	}
	res.CreatedAt = img.CreatedAt
	res.UpdatedAt = img.CreatedAt
	return m.metaStore.Register(res)
}

// failImage marks an image as failed in the metadata store.
func (m *ImageManager) failImage(id string, err error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if img, ok := m.images[id]; ok {
		img.State = ImageStateFailed
		_ = m.updateImageMetadataLocked(img)
	}
	return err
}

// resourceToImage converts a StorageResource to an Image.
func (m *ImageManager) resourceToImage(res *storage.StorageResource) *Image {
	img := &Image{
		ID:        strings.TrimPrefix(res.ID, "image:"),
		Name:      res.Metadata["name"],
		Version:   res.Metadata["version"],
		SizeBytes: res.SizeBytes,
		Format:    ImageFormat(res.Metadata["format"]),
		Path:      res.Path,
		State:     lifecycleToImageState(res.State),
		CreatedAt: res.CreatedAt,
		Metadata:  res.Metadata,
		Labels:    res.Labels,
	}
	return img
}

// Config returns the image manager configuration.
func (m *ImageManager) Config() ImageConfig {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

// imageStateToLifecycle converts an ImageState to a StorageResource LifecycleState.
func imageStateToLifecycle(s ImageState) storage.LifecycleState {
	switch s {
	case ImageStateCreating:
		return storage.StateCreating
	case ImageStateActive:
		return storage.StateActive
	case ImageStateFailed:
		return storage.StateFailed
	case ImageStateDeleting:
		return storage.StateDeleting
	case ImageStateDeleted:
		return storage.StateDeleted
	default:
		return storage.StateActive
	}
}

// lifecycleToImageState converts a StorageResource LifecycleState to an ImageState.
func lifecycleToImageState(s storage.LifecycleState) ImageState {
	switch s {
	case storage.StateCreating:
		return ImageStateCreating
	case storage.StateActive:
		return ImageStateActive
	case storage.StateDeleting:
		return ImageStateDeleting
	case storage.StateFailed:
		return ImageStateFailed
	case storage.StateDeleted:
		return ImageStateDeleted
	default:
		return ImageStateActive
	}
}

// copyFileAtomic copies src to tmpPath then renames to finalPath. On failure
// the temp file is removed so no partial artifact remains.
func copyFileAtomic(src, tmpPath, finalPath string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}

	if err := os.Rename(tmpPath, finalPath); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

// sha256File returns the SHA256 hex digest of the file at path.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Package image provides image management for Bunn Burrow execution
// environments. Images are immutable rootfs artifacts with metadata that can
// be versioned, referenced by templates, and used to boot VMs.
package image

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ImageState describes the lifecycle state of an image.
type ImageState string

const (
	// ImageStateCreating marks an image whose creation is in flight.
	ImageStateCreating ImageState = "creating"
	// ImageStateActive marks an image that is ready for use.
	ImageStateActive ImageState = "active"
	// ImageStateFailed marks an image whose creation failed.
	ImageStateFailed ImageState = "failed"
	// ImageStateDeleting marks an image whose deletion is in flight.
	ImageStateDeleting ImageState = "deleting"
	// ImageStateDeleted marks an image that has been removed.
	ImageStateDeleted ImageState = "deleted"
)

// ImageFormat describes the rootfs image format.
type ImageFormat string

const (
	// ImageFormatQCOW2 is the qcow2 disk image format.
	ImageFormatQCOW2 ImageFormat = "qcow2"
	// ImageFormatExt4 is the ext4 filesystem image format.
	ImageFormatExt4 ImageFormat = "ext4"
	// ImageFormatRaw is the raw disk image format.
	ImageFormatRaw ImageFormat = "raw"
)

// Image is an immutable rootfs artifact that can be used to boot VMs.
// Images are versioned; a new version creates a new Image record with the
// same name but a different version string.
type Image struct {
	// ID is the unique image identifier.
	ID string `json:"id"`

	// Name is the human-readable image name used for lookup.
	Name string `json:"name"`

	// Version is the image version (semver or arbitrary string).
	Version string `json:"version"`

	// Description is a human-readable description.
	Description string `json:"description"`

	// Architecture is the guest architecture (e.g. "amd64", "arm64").
	Architecture string `json:"architecture"`

	// OS is the guest operating system (e.g. "linux").
	OS string `json:"os"`

	// Format is the rootfs image format.
	Format ImageFormat `json:"format"`

	// SizeBytes is the image size in bytes.
	SizeBytes int64 `json:"size_bytes"`

	// Path is the on-disk path to the image file.
	Path string `json:"path"`

	// Checksum is the SHA256 hex digest of the image file.
	Checksum string `json:"checksum"`

	// State is the image lifecycle state.
	State ImageState `json:"state"`

	// CreatedAt is the time the image was created.
	CreatedAt time.Time `json:"created_at"`

	// Labels are optional user-defined key-value labels.
	Labels map[string]string `json:"labels,omitempty"`

	// Metadata is optional user-defined key-value metadata.
	Metadata map[string]string `json:"metadata,omitempty"`

	// ParentID references the parent image if this image was derived from
	// another (e.g. via layering). Empty for base images.
	ParentID string `json:"parent_id,omitempty"`
}

// IsBaseImage returns true if the image has no parent (it is a base image).
func (i *Image) IsBaseImage() bool {
	return i.ParentID == ""
}

// imageID returns the metadata store resource ID for an image.
func imageResourceID(id string) string {
	return "image:" + id
}

// versionedImageID returns the metadata store resource ID for a specific
// image name+version combination.
func versionedImageID(name, version string) string {
	return "image:" + name + "@" + version
}

// validateImageID checks that an image ID is safe to use as a filename
// component and does not allow path traversal.
func validateImageID(id string) error {
	if id == "" {
		return fmt.Errorf("image id is required")
	}
	if filepath.Base(id) != id {
		return fmt.Errorf("image id %q must not contain path separators", id)
	}
	if id == "." || id == ".." {
		return fmt.Errorf("image id %q is reserved", id)
	}
	return nil
}

// validateName checks that an image name is non-empty and safe.
func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("image name is required")
	}
	if filepath.Base(name) != name {
		return fmt.Errorf("image name %q must not contain path separators", name)
	}
	if name == "." || name == ".." {
		return fmt.Errorf("image name %q is reserved", name)
	}
	return nil
}

// validateFormat checks that the image format is recognized.
func validateFormat(f ImageFormat) error {
	switch f {
	case ImageFormatQCOW2, ImageFormatExt4, ImageFormatRaw:
		return nil
	default:
		return fmt.Errorf("unsupported image format %q", f)
	}
}

// imageDir returns the directory containing image files.
func imageDir(rootDir string) string {
	return filepath.Join(rootDir, "images")
}

// EnsureImageDirs creates the images directory.
func EnsureImageDirs(rootDir string) error {
	return os.MkdirAll(imageDir(rootDir), 0o750)
}

// imagePath returns the on-disk path for an image file.
func imagePath(rootDir string, imageID string) string {
	return filepath.Join(imageDir(rootDir), imageID+".img")
}

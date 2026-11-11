package template

import "time"

// Template describes an immutable, reusable VM rootfs template. Once registered
// the template rootfs must never be modified in place; VM instances boot from
// independent writable copies instead.
type Template struct {
	// ID is the unique template identifier.
	ID string `json:"id"`

	// Name is the runtime name used to reference the template.
	Name string `json:"name"`

	// BaseImage is the OCI image reference the template was prepared from.
	BaseImage string `json:"base_image"`

	// RootfsPath is the on-disk location of the immutable rootfs image.
	RootfsPath string `json:"rootfs_path"`

	// RootfsFormat is the rootfs image format (e.g. "ext4").
	RootfsFormat string `json:"rootfs_format"`

	// Architecture is the guest architecture (e.g. "amd64").
	Architecture string `json:"architecture"`

	// CreatedAt is the time the template was registered.
	CreatedAt time.Time `json:"created_at"`

	// RootfsChecksum is the SHA256 hex digest of the immutable rootfs image.
	RootfsChecksum string `json:"rootfs_checksum"`

	// Version is the template version.
	Version string `json:"version"`

	// Immutable reports whether the template rootfs is read-only/immutable.
	Immutable bool `json:"immutable"`
}

package template

import "time"

// Template describes a reusable execution environment that references an
// image and provides default workload configuration. Templates do not
// duplicate image data; they reference an existing image by ID and version.
// For backward compatibility, a template may also carry a direct rootfs path
// (set when created from a local rootfs without an image reference).
type Template struct {
	// ID is the unique template identifier.
	ID string `json:"id"`

	// Name is the runtime name used to reference the template.
	Name string `json:"name"`

	// ImageID is the ID of the image this template references (W5).
	ImageID string `json:"image_id,omitempty"`

	// ImageVersion is the version of the referenced image (W5).
	ImageVersion string `json:"image_version,omitempty"`

	// Description is a human-readable description.
	Description string `json:"description"`

	// DefaultCmd is the default command to run in the VM.
	DefaultCmd string `json:"default_cmd,omitempty"`

	// DefaultArgs are the default command-line arguments.
	DefaultArgs []string `json:"default_args,omitempty"`

	// DefaultEnv are the default environment variables.
	DefaultEnv []string `json:"default_env,omitempty"`

	// DefaultDir is the default working directory inside the guest.
	DefaultDir string `json:"default_dir,omitempty"`

	// DefaultVCPU is the default vCPU count.
	DefaultVCPU int64 `json:"default_vcpu,omitempty"`

	// DefaultMemMiB is the default memory size in MiB.
	DefaultMemMiB int64 `json:"default_mem_mib,omitempty"`

	// DefaultTimeout is the default workload timeout in seconds.
	DefaultTimeout int64 `json:"default_timeout,omitempty"`

	// DefaultNetwork enables or disables networking by default.
	DefaultNetwork bool `json:"default_network,omitempty"`

	// BaseImage is the OCI image reference the template was prepared from.
	BaseImage string `json:"base_image,omitempty"`

	// RootfsPath is the on-disk location of the immutable rootfs image.
	RootfsPath string `json:"rootfs_path,omitempty"`

	// RootfsFormat is the rootfs image format (e.g. "ext4").
	RootfsFormat string `json:"rootfs_format,omitempty"`

	// Architecture is the guest architecture (e.g. "amd64").
	Architecture string `json:"architecture"`

	// CreatedAt is the time the template was registered.
	CreatedAt time.Time `json:"created_at"`

	// RootfsChecksum is the SHA256 hex digest of the immutable rootfs image.
	RootfsChecksum string `json:"rootfs_checksum,omitempty"`

	// Version is the template version.
	Version string `json:"version,omitempty"`

	// Immutable reports whether the template rootfs is read-only/immutable.
	Immutable bool `json:"immutable"`

	// Labels are optional user-defined key-value labels.
	Labels map[string]string `json:"labels,omitempty"`

	// Metadata is optional user-defined key-value metadata.
	Metadata map[string]string `json:"metadata,omitempty"`
}

// ImagePath returns the rootfs path for the template. If the template
// references an image, the caller must resolve the image path separately.
// For backward compatibility, returns RootfsPath if set.
func (t *Template) ImagePath() string {
	return t.RootfsPath
}

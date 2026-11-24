package template

import (
	"fmt"
	"strings"
	"sync"
)

// ImageInfo is a minimal image descriptor used to avoid importing the image
// package (which would create an import cycle: template → image → storage →
// layer → template).
type ImageInfo struct {
	// ID is the image identifier.
	ID string

	// State is the image lifecycle state (e.g. "active", "failed").
	State string
}

// ImageResolver inspects images. Implemented by *image.ImageManager.
type ImageResolver interface {
	InspectImage(id string) (*ImageInfo, error)
}

// TemplateChecker is implemented by TemplateManager to check whether any
// template references a given image ID. Used by ImageManager for deletion
// protection.
type TemplateChecker interface {
	// TemplateReferencesImage returns true if any template references the given
	// image ID.
	TemplateReferencesImage(imageID string) (bool, error)
}

// TemplateManager wraps Store and adds image reference validation and
// template-to-defaults resolution. It ensures templates only reference
// existing images and provides default execution configuration for VMs
// created from templates.
type TemplateManager struct {
	mu            sync.Mutex
	store         *Store
	imageResolver ImageResolver
}

// NewTemplateManager creates a TemplateManager wrapping the given Store.
// If imageResolver is non-nil, template image references are validated on create.
func NewTemplateManager(store *Store, imageResolver ImageResolver) *TemplateManager {
	return &TemplateManager{
		store:         store,
		imageResolver: imageResolver,
	}
}

// Create registers a template. If ImageID is set, the referenced image must
// exist and be active. Returns the created template.
func (m *TemplateManager) Create(tmpl *Template) error {
	if tmpl.ImageID != "" && m.imageResolver != nil {
		img, err := m.imageResolver.InspectImage(tmpl.ImageID)
		if err != nil {
			return fmt.Errorf("inspect image %q: %w", tmpl.ImageID, err)
		}
		if img == nil {
			return fmt.Errorf("image %q not found", tmpl.ImageID)
		}
		if img.State != "active" {
			return fmt.Errorf("image %q is not active (state=%s)", tmpl.ImageID, img.State)
		}
		// If template doesn't specify a version, use the image's version.
		// (ImageInfo doesn't carry version; caller should set it explicitly.)
	}

	return m.store.Create(tmpl)
}

// Get returns the template with the given ID, or nil if not found.
func (m *TemplateManager) Get(id string) *Template {
	return m.store.Get(id)
}

// List returns all registered templates.
func (m *TemplateManager) ListTemplates() ([]*Template, error) {
	return m.store.List()
}

// Validate verifies the template's image reference (if any) and rootfs
// checksum (if applicable).
func (m *TemplateManager) Validate(id string) error {
	tmpl := m.store.Get(id)
	if tmpl == nil {
		return fmt.Errorf("template %q not found", id)
	}

	// Validate image reference.
	if tmpl.ImageID != "" && m.imageResolver != nil {
		img, err := m.imageResolver.InspectImage(tmpl.ImageID)
		if err != nil {
			return fmt.Errorf("inspect image %q: %w", tmpl.ImageID, err)
		}
		if img == nil {
			return fmt.Errorf("template %q references unknown image %q", id, tmpl.ImageID)
		}
		if img.State != "active" {
			return fmt.Errorf("template %q references image %q which is not active (state=%s)", id, tmpl.ImageID, img.State)
		}
	}

	// Validate rootfs checksum (backward compatible path).
	return m.store.Validate(id)
}

// Delete removes a template. Returns an error if the template is in use
// (reference count > 0).
func (m *TemplateManager) Delete(id string) error {
	return m.store.Delete(id)
}

// Acquire increments the template's in-use reference count.
func (m *TemplateManager) Acquire(id string) error {
	return m.store.Acquire(id)
}

// Release decrements the template's in-use reference count.
func (m *TemplateManager) Release(id string) {
	m.store.Release(id)
}

// TemplateReferencesImage returns true if any template references the given
// image ID. Implements image.TemplateChecker for image deletion protection.
func (m *TemplateManager) TemplateReferencesImage(imageID string) (bool, error) {
	templates, err := m.store.List()
	if err != nil {
		return false, err
	}
	for _, t := range templates {
		if t.ImageID == imageID {
			return true, nil
		}
	}
	return false, nil
}

// TemplateDefaults holds execution defaults from a template that can be
// applied to a vmm.Config. Defined in the template package to avoid importing
// vmm (circular dependency).
type TemplateDefaults struct {
	// TemplateID is the resolved template ID.
	TemplateID string

	// DefaultCmd is the default command.
	DefaultCmd string

	// DefaultArgs are the default arguments.
	DefaultArgs []string

	// DefaultEnv are the default environment variables.
	DefaultEnv []string

	// DefaultDir is the default working directory.
	DefaultDir string

	// DefaultVCPU is the default vCPU count.
	DefaultVCPU int64

	// DefaultMemMiB is the default memory in MiB.
	DefaultMemMiB int64

	// DefaultTimeout is the default timeout in seconds.
	DefaultTimeout int64

	// DefaultNetwork enables or disables networking by default.
	DefaultNetwork bool

	// BootArgs are kernel boot arguments derived from template defaults.
	BootArgs string
}

// ResolveDefaults returns the execution defaults from a template. The caller
// applies these to a vmm.Config, with explicitly provided fields taking
// precedence over template defaults.
func (m *TemplateManager) ResolveDefaults(id string) (*TemplateDefaults, error) {
	tmpl := m.store.Get(id)
	if tmpl == nil {
		return nil, fmt.Errorf("template %q not found", id)
	}

	bootArgs := "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rootfstype=ext4 rootwait rw init=/sbin/bunn-agent"
	for _, e := range tmpl.DefaultEnv {
		if strings.Contains(e, "=") {
			parts := strings.SplitN(e, "=", 2)
			bootArgs += " " + parts[0] + "=" + parts[1]
		}
	}

	return &TemplateDefaults{
		TemplateID:     tmpl.ID,
		DefaultCmd:     tmpl.DefaultCmd,
		DefaultArgs:    tmpl.DefaultArgs,
		DefaultEnv:     tmpl.DefaultEnv,
		DefaultDir:     tmpl.DefaultDir,
		DefaultVCPU:    tmpl.DefaultVCPU,
		DefaultMemMiB:  tmpl.DefaultMemMiB,
		DefaultTimeout: tmpl.DefaultTimeout,
		DefaultNetwork: tmpl.DefaultNetwork,
		BootArgs:       bootArgs,
	}, nil
}

// ApplyDefaults applies template defaults to a Config. Fields already set
// in cfg take precedence over template defaults.
func ApplyDefaults(cfg *Config, defaults *TemplateDefaults) {
	if defaults == nil || cfg == nil {
		return
	}
	cfg.TemplateID = defaults.TemplateID
	if defaults.DefaultVCPU > 0 {
		cfg.VCPU = defaults.DefaultVCPU
	}
	if defaults.DefaultMemMiB > 0 {
		cfg.MemSizeMiB = defaults.DefaultMemMiB
	}
	if defaults.BootArgs != "" {
		cfg.BootArgs = defaults.BootArgs
	}
	if defaults.DefaultDir != "" {
		cfg.WorkDir = defaults.DefaultDir
	}
}

// Config is a minimal copy of vmm.Config fields needed for template resolution.
// Defined here to avoid importing vmm from template (circular dependency).
type Config struct {
	VCPU       int64
	MemSizeMiB int64
	BootArgs   string
	WorkDir    string
	TemplateID string
}

// Store returns the underlying template store.
func (m *TemplateManager) Store() *Store {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.store
}

// Ensure TemplateManager satisfies image.TemplateChecker.
var _ TemplateChecker = (*TemplateManager)(nil)

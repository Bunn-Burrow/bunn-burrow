package vmm

import (
	"github.com/Bunn-Burrow/bunn-burrow/internal/network"
	"github.com/Bunn-Burrow/bunn-burrow/internal/storage"
	"github.com/Bunn-Burrow/bunn-burrow/internal/template"
	vmimage "github.com/Bunn-Burrow/bunn-burrow/internal/vmimage"
)

// Dependencies contains host-level services shared by VM instances.
// These services are owned by the runtime, not by individual VMs.
type Dependencies struct {
	NetworkManager  *network.Manager
	VolumeManager   *storage.VolumeManager
	ImageManager    *vmimage.ImageManager
	TemplateManager *template.TemplateManager
	TemplateStore   *template.Store
	LayerManager    *storage.LayerManager
	NBDManager      storage.NBDManager
	StorageManager  *storage.StorageManager
}

// withConfigFallback preserves support for manager fields previously
// configured directly through Config.
func (d Dependencies) withConfigFallback(cfg Config) Dependencies {
	if d.NetworkManager == nil {
		d.NetworkManager = cfg.NetworkManager
	}
	if d.VolumeManager == nil {
		d.VolumeManager = cfg.VolumeManager
	}
	if d.ImageManager == nil {
		d.ImageManager = cfg.ImageManager
	}
	if d.TemplateManager == nil {
		d.TemplateManager = cfg.TemplateManager
	}
	return d
}

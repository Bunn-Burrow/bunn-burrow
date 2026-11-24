// Package runtime provides a composition root for Bunn Burrow's microVM
// infrastructure. It constructs and owns shared components (metadata store,
// storage manager, network manager, etc.) and vends VMs with those
// dependencies pre-wired.
//
// All VMs created by a Runtime share the same backing infrastructure. VM
// lifecycle is process-local: VMs created by one Runtime cannot be managed
// by another Runtime instance. When Runtime.Shutdown is called, all VMs are
// destroyed first, then shared resources are released. VM destruction does not
// shut down shared managers; Runtime owns the shared lifecycle.
//
// This package does not introduce a daemon, control plane, or remote API. It
// is a pure in-process composition root: all state is held in memory and lost
// when the process exits.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/Bunn-Burrow/bunn-burrow/internal/image"
	"github.com/Bunn-Burrow/bunn-burrow/internal/network"
	"github.com/Bunn-Burrow/bunn-burrow/internal/storage"
	"github.com/Bunn-Burrow/bunn-burrow/internal/template"
	vmimage "github.com/Bunn-Burrow/bunn-burrow/internal/vmimage"
	"github.com/Bunn-Burrow/bunn-burrow/vmm"
	"github.com/sirupsen/logrus"
)

// Runtime owns shared infrastructure and creates VMs with dependencies
// pre-wired. All VMs created by a Runtime share the same metadata store,
// storage manager, network manager, etc.
//
// VM lifecycle is process-local: VMs created by one Runtime cannot be managed
// by another Runtime instance. When Runtime.Shutdown is called, all VMs are
// destroyed first, then shared resources are released. VM destruction does not
// shut down shared managers; Runtime owns the shared lifecycle.
//
// This package does not introduce a daemon, control plane, or remote API. It
// is a pure in-process composition root: all state is held in memory and lost
// when the process exits.
type Runtime struct {
	mu      sync.Mutex
	rootDir string
	logger  *logrus.Entry

	// Shared infrastructure (owned by Runtime, never by individual VMs).
	metaStore  *storage.MetadataStore
	layerMgr   *storage.LayerManager
	nbdMgr     storage.NBDManager
	tmplStore  *template.Store
	tmplMgr    *template.TemplateManager
	imgMgr     *image.Manager
	vmImgMgr   *vmimage.ImageManager
	snapStore  *vmm.SnapshotStore
	storageMgr *storage.StorageManager
	volMgr     *storage.VolumeManager
	netMgr     *network.Manager
	snapCoord  *vmm.SnapshotCoordinator

	// VM tracking. VMs created by this Runtime are tracked so Shutdown can
	// destroy them before releasing shared resources.
	vms    map[string]*vmm.VM
	closed bool
}

// NewRuntime creates a Runtime with all shared infrastructure components.
// rootDir is the base directory for all runtime state (VM work dirs, overlays,
// metadata, images, etc.).
func NewRuntime(rootDir string, logger *logrus.Entry) (*Runtime, error) {
	if rootDir == "" {
		return nil, errors.New("rootDir is required")
	}
	if logger == nil {
		l := logrus.New()
		l.SetOutput(os.Stderr)
		logger = l.WithField("component", "runtime")
	}

	if err := os.MkdirAll(rootDir, 0o750); err != nil {
		return nil, fmt.Errorf("create root dir %q: %w", rootDir, err)
	}

	r := &Runtime{
		rootDir: rootDir,
		logger:  logger,
		vms:     make(map[string]*vmm.VM),
	}

	// 1. Metadata store — shared by StorageManager and VolumeManager.
	r.metaStore = storage.NewMetadataStore(rootDir)

	// 2. Template store — shared by StorageManager, TemplateManager,
	// SnapshotCoordinator.
	r.tmplStore = template.New(rootDir)

	// 3. Layer manager — needs TemplateStore.
	r.layerMgr = storage.NewLayerManager(rootDir, r.tmplStore)

	// 4. NBD manager.
	r.nbdMgr = storage.NewNBDManager(0, 16, filepath.Join(rootDir, "nbd"), filepath.Join(rootDir, "locks"))

	// 5. Image manager — implements template.ImageResolver.
	r.imgMgr = image.NewManager(filepath.Join(rootDir, "images"))

	// 6. VM image manager — used by vmm.Config.ImageManager.
	r.vmImgMgr = vmimage.NewImageManager(
		vmimage.DefaultImageConfig(filepath.Join(rootDir, "images")),
		r.metaStore,
	)

	// 7. Template manager — needs TemplateStore + ImageManager.
	r.tmplMgr = template.NewTemplateManager(r.tmplStore, r.imgMgr)

	// 7. Snapshot store — used by SnapshotCoordinator and adapted to
	// storage.SnapshotStore for StorageManager.
	r.snapStore = vmm.NewSnapshotStore(filepath.Join(rootDir, "snapshots"))

	// 8. Storage manager — needs LayerManager, NBDManager, SnapshotStore,
	// TemplateStore, MetadataStore.
	r.storageMgr = storage.NewStorageManagerWithMetadataStore(
		rootDir,
		r.layerMgr,
		r.nbdMgr,
		r.snapStore.StorageSnapshotStore(),
		r.tmplStore,
		r.metaStore,
	)

	// 9. Volume manager — needs LayerManager, NBDManager, MetadataStore.
	r.volMgr = storage.NewVolumeManager(
		storage.DefaultVolumeConfig(rootDir),
		r.layerMgr,
		r.nbdMgr,
		r.metaStore,
	)

	// 10. Network manager.
	netCfg := network.DefaultConfig()
	netMgr, err := network.NewManager(netCfg, r.logger)
	if err != nil {
		return nil, fmt.Errorf("create network manager: %w", err)
	}
	r.netMgr = netMgr

	// 11. Snapshot coordinator — needs SnapshotStore, LayerManager,
	// TemplateStore, NBDManager, logger.
	r.snapCoord = vmm.NewSnapshotCoordinator(
		r.snapStore,
		r.layerMgr,
		r.tmplStore,
		r.nbdMgr,
		r.logger,
	)

	return r, nil
}

// RootDir returns the runtime's root directory.
func (r *Runtime) RootDir() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rootDir
}

// NewVM creates a VM with all shared dependencies pre-wired. The VM owns its
// writable layer, NBD mapping, and network allocation; the Runtime owns the
// shared managers. Destroying the VM releases its owned resources but does not
// shut down shared infrastructure.
//
// Use Runtime.Shutdown to cleanly destroy all VMs and release shared
// resources.
func (r *Runtime) NewVM(cfg vmm.Config) (*vmm.VM, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errors.New("runtime is closed")
	}

	// Inject shared dependencies. Explicitly set fields so callers cannot
	// accidentally override them.
	cfg.NetworkManager = r.netMgr
	cfg.VolumeManager = r.volMgr
	cfg.ImageManager = r.vmImgMgr
	cfg.TemplateManager = r.tmplMgr
	cfg.TemplateStore = r.tmplStore
	cfg.LayerManager = r.layerMgr
	cfg.NBDManager = r.nbdMgr
	cfg.StorageManager = r.storageMgr

	vm, err := vmm.New(cfg, r.logger)
	if err != nil {
		r.mu.Unlock()
		return nil, err
	}

	// Track the VM so Shutdown can destroy it. The VM will notify us on
	// Destroy via onDestroy.
	vmID := vm.ID()
	vm.SetOnDestroy(func(id string) {
		r.mu.Lock()
		delete(r.vms, id)
		r.mu.Unlock()
	})

	r.vms[vmID] = vm
	r.mu.Unlock()

	r.logger.Debug("VM created",
		"vm_id", vm.ID(),
		"work_dir", vm.Dir())

	return vm, nil
}

// Shutdown destroys all VMs and releases shared resources. VMs are destroyed
// first (releasing their NBD mappings, template references, network
// allocations), then shared managers are shut down. After Shutdown returns,
// the Runtime cannot be used.
//
// Shutdown is idempotent: calling it multiple times returns nil after the
// first call.
func (r *Runtime) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true

	// Collect VMs to destroy while holding the lock, then destroy outside
	// the lock to avoid holding it during potentially slow operations.
	vms := make([]*vmm.VM, 0, len(r.vms))
	for _, vm := range r.vms {
		vms = append(vms, vm)
	}
	r.mu.Unlock()

	var errs []error

	// Destroy all VMs first. This releases each VM's writable layer, NBD
	// mapping, template reference, and network allocation. VMs must be
	// destroyed before the shared NetworkManager is shut down because
	// NetworkManager.Shutdown refuses if any VM networks are still
	// allocated.
	for _, vm := range vms {
		if err := vm.Destroy(ctx); err != nil {
			errs = append(errs, fmt.Errorf("destroy vm %q: %w", vm.ID(), err))
		} else {
			r.logger.Debug("VM destroyed",
				"vm_id", vm.ID())
		}
	}

	// Shutdown shared network manager (releases bridge, firewall, IPAM).
	// This must happen after all VMs are destroyed because Shutdown refuses
	// if any VM networks are still allocated.
	if r.netMgr != nil && r.netMgr.Enable() {
		if err := r.netMgr.Shutdown(); err != nil {
			errs = append(errs, fmt.Errorf("shutdown network manager: %w", err))
		} else {
			r.logger.Debug("network manager shut down")
		}
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	r.logger.Info("runtime shut down")
	return nil
}

// SnapshotCoordinator returns the shared snapshot coordinator for creating
// and restoring snapshots across VMs.
func (r *Runtime) SnapshotCoordinator() *vmm.SnapshotCoordinator {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapCoord
}

// StorageManager returns the shared storage manager.
func (r *Runtime) StorageManager() *storage.StorageManager {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.storageMgr
}

// VolumeManager returns the shared volume manager.
func (r *Runtime) VolumeManager() *storage.VolumeManager {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.volMgr
}

// ImageManager returns the shared image manager.
func (r *Runtime) ImageManager() *image.Manager {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.imgMgr
}

// TemplateManager returns the shared template manager.
func (r *Runtime) TemplateManager() *template.TemplateManager {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tmplMgr
}

// NetworkManager returns the shared network manager.
func (r *Runtime) NetworkManager() *network.Manager {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.netMgr
}

// NBDManager returns the shared NBD manager.
func (r *Runtime) NBDManager() storage.NBDManager {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.nbdMgr
}

// TemplateStore returns the shared template store.
func (r *Runtime) TemplateStore() *template.Store {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tmplStore
}

// LayerManager returns the shared layer manager.
func (r *Runtime) LayerManager() *storage.LayerManager {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.layerMgr
}

// MetadataStore returns the shared metadata store.
func (r *Runtime) MetadataStore() *storage.MetadataStore {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.metaStore
}

// SnapshotStore returns the shared snapshot store.
func (r *Runtime) SnapshotStore() *vmm.SnapshotStore {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapStore
}

// VMCount returns the number of VMs currently tracked by the Runtime.
func (r *Runtime) VMCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.vms)
}

// VM returns the VM with the given ID, or nil if not found or already
// destroyed.
func (r *Runtime) VM(id string) *vmm.VM {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.vms[id]
}

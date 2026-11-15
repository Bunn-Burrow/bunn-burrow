package vmm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"

	"github.com/Bunn-Burrow/bunn-burrow/internal/network"
	"github.com/Bunn-Burrow/bunn-burrow/internal/storage"
	"github.com/Bunn-Burrow/bunn-burrow/internal/template"
	vmimage "github.com/Bunn-Burrow/bunn-burrow/internal/vmimage"
)

// VM owns a Firecracker microVM instance: its lifecycle state, process handle,
// API client, vsock transport, and per-VM resources.
type VM struct {
	id  string
	cfg Config
	res Resources

	sm *StateMachine

	mu        sync.Mutex
	api       API
	proc      *Process
	transport *Transport
	exitCh    chan struct{}
	exitErr   error
	snapshot  *SnapshotInfo
	logger    *logrus.Entry

	// TemplateStore resolves templates for template-based VM creation. Optional;
	// when nil, only direct rootfs creation is supported (backward compatible).
	TemplateStore *template.Store
	// LayerMgr creates per-VM writable layers. Optional; when nil, template
	// based VM creation is unavailable.
	LayerMgr *storage.LayerManager
	// NBDMgr maps qcow2 overlays to NBD block devices for Firecracker.
	// Optional; when nil, template-based VM creation passes the qcow2 path
	// directly (not supported by Firecracker natively).
	NBDMgr *storage.NBDManager
	// StorageManager tracks persistent storage resources for this VM.
	// Optional; when nil, storage lifecycle tracking is disabled.
	StorageManager *storage.StorageManager
	// VolumeManager manages persistent volumes for VM attachment. Optional;
	// when nil, volume attach/detach is unavailable.
	VolumeManager *storage.VolumeManager
	// ImageManager resolves images for VM boot. Optional; when nil, image-based
	// VM creation is unavailable.
	ImageManager *vmimage.ImageManager
	// TemplateManager manages templates for this VM. Optional; when nil,
	// template-based VM creation is unavailable.
	TemplateManager *template.TemplateManager
	// NetworkManager manages TAP/IPAM/firewall resources for VMs. Optional;
	// when nil and Network is set, the VM creates its own manager and owns it.
	NetworkManager *network.Manager

	// network is the allocated network for this VM, if networking is enabled.
	network *network.Network
	// ownNetworkMgr indicates whether this VM owns its NetworkManager and
	// should shut it down on Destroy.
	ownNetworkMgr bool

	// writableLayer is the path to this VM's writable layer copy when booted
	// from a template. It is cleared once the layer is released.
	writableLayer string
	// nbdDevice is the NBD device path (e.g. /dev/nbd0) assigned to this VM's
	// overlay. It is cleared once the NBD mapping is released.
	nbdDevice string
	// imageNBDDevice is the NBD device path for image-based boot. Cleared on
	// stop/destroy.
	imageNBDDevice string
	// attachedVolumes tracks NBD device paths for volumes attached to this VM.
	// They are detached on Stop/Destroy.
	attachedVolumes []string
}

// New creates a VM in the Created state. It validates configuration, allocates
// a unique VM ID and private working directory, and reserves unique API and
// vsock socket paths. Firecracker is not started.
func New(cfg Config, logger *logrus.Entry) (*VM, error) {
	if logger == nil {
		l := logrus.New()
		l.SetOutput(os.Stderr)
		logger = l.WithField("component", "vmm")
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	id := cfg.ID
	if id == "" {
		id = uuid.NewString()
	}
	if id == "." || id == ".." || filepath.Base(id) != id {
		return nil, fmt.Errorf("invalid VM ID %q: must be a single path component", id)
	}
	if cfg.WorkDir == "" {
		workDir, err := defaultVMWorkDir()
		if err != nil {
			return nil, err
		}
		cfg.WorkDir = workDir
	}
	if filepath.Clean(cfg.WorkDir) == string(os.PathSeparator) {
		return nil, errors.New("VM work directory cannot be the filesystem root")
	}
	workDir, err := filepath.Abs(cfg.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("resolve VM work directory %q: %w", cfg.WorkDir, err)
	}
	cfg.WorkDir = workDir

	vm := &VM{
		id:     id,
		cfg:    cfg,
		res:    cfg.resources(id),
		sm:     NewStateMachine(),
		exitCh: make(chan struct{}),
		logger: logger.WithField("vm", id),
	}

	// If networking is configured but no shared network manager is provided,
	// create one owned by this VM.
	if cfg.Network != nil && cfg.Network.Enabled && vm.NetworkManager == nil {
		mgr, err := network.NewManager(cfg.Network.toInternal(), logger)
		if err != nil {
			return nil, fmt.Errorf("create network manager: %w", err)
		}
		vm.NetworkManager = mgr
		vm.ownNetworkMgr = true
	}

	// Use the configured volume manager if provided.
	if cfg.VolumeManager != nil {
		vm.VolumeManager = cfg.VolumeManager
	}

	// Use the configured image manager if provided.
	if cfg.ImageManager != nil {
		vm.ImageManager = cfg.ImageManager
	}

	// Use the configured template manager if provided.
	if cfg.TemplateManager != nil {
		vm.TemplateManager = cfg.TemplateManager
	}

	// Create the user-owned VM storage base and an exclusive per-VM directory.
	// The exclusive mkdir prevents a reused ID from adopting or later deleting
	// files belonging to an earlier VM.
	if err := os.MkdirAll(cfg.WorkDir, 0700); err != nil {
		return nil, fmt.Errorf("create VM storage directory %q: %w", cfg.WorkDir, err)
	}
	if err := os.Mkdir(vm.res.Dir, 0700); err != nil {
		return nil, fmt.Errorf("create VM working directory: %w", err)
	}
	return vm, nil
}

// ID returns the VM's unique identifier.
func (vm *VM) ID() string { return vm.id }

// State returns the host-side lifecycle state.
func (vm *VM) State() State { return vm.sm.State() }

// Dir returns the VM's private working directory.
func (vm *VM) Dir() string { return vm.res.Dir }

// ConsoleLogPath returns the path to the file capturing Firecracker's stderr,
// which includes Firecracker diagnostics and the guest serial console output.
func (vm *VM) ConsoleLogPath() string {
	return filepath.Join(vm.res.Dir, "console.log")
}

// Vsock returns the per-VM vsock transport, or nil if not yet configured.
func (vm *VM) Vsock() *Transport {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	return vm.transport
}

// PID returns the Firecracker process PID, or 0 if not running.
func (vm *VM) PID() int {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if vm.proc == nil {
		return 0
	}
	return vm.proc.PID()
}

// Snapshot returns the most recent snapshot info for the VM, if any.
func (vm *VM) Snapshot() *SnapshotInfo {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	return vm.snapshot
}

// Start boots Firecracker, configures the VM, and starts the guest. It moves
// Created -> Preparing -> Running.
//
// Execution order: launch Firecracker, wait for its API socket (startProcessAndWait),
// then configure devices (machine config, boot source, root disk), configure vsock,
// and finally issue InstanceStart. On any failure the process is terminated and the
// VM transitions to Failed. Never reports the VM as running before startup succeeds.
func (vm *VM) Start(ctx context.Context) (startErr error) {
	if err := vm.sm.Transition(StateCreated, StatePreparing); err != nil {
		return fmt.Errorf("start: %w", err)
	}

	// Resolve the template and prepare per-VM writable storage before Firecracker
	// starts. If a layer is created but startup later fails, it is cleaned up by
	// the deferred removal below.
	var layerCreated bool
	// networkAllocated tracks whether network resources were allocated so they
	// can be released on failure.
	var networkAllocated bool
	// volumesAttached tracks whether volumes were attached so they can be
	// detached on failure.
	var volumesAttached bool
	// imageAttached tracks whether an image NBD device was mapped so it can
	// be unmapped on failure.
	var imageAttached bool
	defer func() {
		if layerCreated && vm.State() != StateRunning {
			vm.removeWritableLayer()
		}
		if networkAllocated && vm.State() != StateRunning {
			if err := vm.releaseNetwork(); err != nil {
				startErr = errors.Join(startErr, fmt.Errorf("release network on startup failure: %w", err))
			}
		}
		if vm.State() != StateRunning && vm.ownNetworkMgr {
			if err := vm.shutDownNetworkMgr(); err != nil {
				startErr = errors.Join(startErr, fmt.Errorf("shutdown network after startup failure: %w", err))
			}
		}
		if volumesAttached && vm.State() != StateRunning {
			if err := vm.detachVolumes(); err != nil {
				vm.logger.WithError(err).Warn("detach volumes on startup failure")
			}
		}
		if imageAttached && vm.State() != StateRunning {
			vm.unmapImage()
		}
	}()

	if vm.cfg.ImageID != "" {
		if err := vm.resolveImage(ctx); err != nil {
			vm.sm.TransitionTo(StateFailed)
			return err
		}
		imageAttached = true
	}

	if vm.cfg.TemplateID != "" {
		if err := vm.resolveTemplate(ctx); err != nil {
			vm.sm.TransitionTo(StateFailed)
			return err
		}
		layerCreated = true
	}

	// Allocate host network resources before starting Firecracker so privileged
	// setup failures are reported before the VM process is launched.
	if err := vm.allocateNetwork(ctx); err != nil {
		vm.sm.TransitionTo(StateFailed)
		return fmt.Errorf("allocate network: %w", err)
	}
	networkAllocated = true

	if err := vm.startProcessAndWait(ctx); err != nil {
		vm.sm.TransitionTo(StateFailed)
		return err
	}

	// Attach persistent volumes before device configuration so their NBD
	// block devices are available when Firecracker configures drives.
	if _, err := vm.attachVolumes(ctx); err != nil {
		vm.stopProcLocked()
		vm.sm.TransitionTo(StateFailed)
		return fmt.Errorf("attach volumes: %w", err)
	}
	volumesAttached = true

	if err := vm.configureDevices(ctx); err != nil {
		vm.stopProcLocked()
		vm.sm.TransitionTo(StateFailed)
		return fmt.Errorf("configure devices: %w", err)
	}

	if err := vm.configureVsock(ctx); err != nil {
		vm.stopProcLocked()
		vm.sm.TransitionTo(StateFailed)
		return fmt.Errorf("configure vsock: %w", err)
	}

	// Start the guest instance.
	if err := vm.api.CreateSyncAction(ctx, &models.InstanceActionInfo{
		ActionType: instanceActionPtr(models.InstanceActionInfoActionTypeInstanceStart),
	}); err != nil {
		vm.stopProcLocked()
		vm.sm.TransitionTo(StateFailed)
		return fmt.Errorf("start instance: %w", err)
	}

	vm.logger.Info("guest instance started",
		map[string]any{"vm_id": vm.id, "vcpu": vm.cfg.VCPU, "mem_mib": vm.cfg.MemSizeMiB})

	if err := vm.sm.Transition(StatePreparing, StateRunning); err != nil {
		vm.stopProcLocked()
		return fmt.Errorf("finalize start: %w", err)
	}

	vm.logger.Info("VM started",
		map[string]any{"vm_id": vm.id, "pid": vm.proc.PID(), "state": vm.State()})
	return nil
}

// startProcessAndWait launches Firecracker, waits for its API socket, and
// creates the API client. It does NOT configure any devices. This is the
// correct prefix for both a normal boot and a snapshot restore, because
// Firecracker v1.17.0 rejects device configuration (machine config, boot
// source, drives) before LoadSnapshot. On failure the launched process is
// terminated via stopProcLocked before returning an error.
func (vm *VM) startProcessAndWait(ctx context.Context) error {
	env := minimalEnv()

	bin := vm.cfg.BinPath
	if bin == "" {
		bin = resolveFirecrackerBin()
	}

	args := []string{
		"--api-sock", vm.res.APISock,
		"--id", vm.id,
	}
	if vm.cfg.LogFile != "" {
		args = append(args, "--log-path", vm.cfg.LogFile, "--level", "Debug")
	}

	proc, err := Start(bin, args, env, vm.ConsoleLogPath())
	if err != nil {
		return fmt.Errorf("launch firecracker: %w", err)
	}

	vm.mu.Lock()
	vm.proc = proc
	vm.mu.Unlock()

	// Detect unexpected process exits.
	go vm.monitorExit()

	if err := vm.waitForSocket(ctx); err != nil {
		vm.stopProcLocked()
		return fmt.Errorf("firecracker did not initialize: %w", err)
	}

	api, err := NewAPIClient(vm.res.APISock, vm.logger)
	if err != nil {
		vm.stopProcLocked()
		return fmt.Errorf("create API client: %w", err)
	}
	vm.mu.Lock()
	vm.api = api
	vm.mu.Unlock()

	return nil
}

// configureDevices applies machine configuration, boot source, and root disk.
// Must be called after the API client is live and before InstanceStart, and
// must NOT be called before LoadSnapshot when restoring a snapshot (Firecracker
// rejects boot-specific resource configuration prior to snapshot load). Sets
// VM CPU, memory, kernel arguments, and root drive read-only/writeback state.
func (vm *VM) configureDevices(ctx context.Context) error {
	if err := vm.api.PutMachineConfiguration(ctx, &models.MachineConfiguration{
		VcpuCount:  int64Ptr(vm.cfg.VCPU),
		MemSizeMib: int64Ptr(vm.cfg.MemSizeMiB),
		Smt:        boolPtr(false),
	}); err != nil {
		return err
	}

	bootArgs := vm.cfg.BootArgs
	if bootArgs == "" {
		bootArgs = "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rootfstype=ext4 rootwait rw init=/sbin/bunn-agent"
	}
	if vm.network != nil {
		if strings.Contains(bootArgs, "bunn_net=") {
			return fmt.Errorf("boot arguments already contain bunn_net configuration")
		}
		networkArg, err := vm.networkBootArg()
		if err != nil {
			return err
		}
		bootArgs = strings.TrimSpace(bootArgs) + " " + networkArg
	}
	if err := vm.api.PutGuestBootSource(ctx, &models.BootSource{
		KernelImagePath: &vm.cfg.KernelImage,
		BootArgs:        bootArgs,
	}); err != nil {
		return err
	}

	if err := vm.api.PutGuestDriveByID(ctx, "rootfs", &models.Drive{
		DriveID:      &rootfsDriveID,
		PathOnHost:   &vm.cfg.RootFS,
		IsRootDevice: boolPtr(true),
		IsReadOnly:   boolPtr(vm.cfg.RootFSReadOnly),
		CacheType:    cacheTypePtr(models.DriveCacheTypeWriteback),
		IoEngine:     ioEnginePtr(models.DriveIoEngineSync),
	}); err != nil {
		return err
	}

	// Attach persistent volumes as additional drives. Each volume's NBD device
	// is exposed to the guest as a writable block device.
	for i, devicePath := range vm.attachedVolumes {
		driveID := fmt.Sprintf("vol-%d", i)
		isReadOnly := false
		if err := vm.api.PutGuestDriveByID(ctx, driveID, &models.Drive{
			DriveID:      &driveID,
			PathOnHost:   &devicePath,
			IsRootDevice: boolPtr(false),
			IsReadOnly:   &isReadOnly,
			CacheType:    cacheTypePtr(models.DriveCacheTypeWriteback),
			IoEngine:     ioEnginePtr(models.DriveIoEngineSync),
		}); err != nil {
			return fmt.Errorf("configure volume drive %s: %w", driveID, err)
		}
	}

	// Attach the network interface if networking is configured. Firecracker
	// expects the host to create the TAP device before this call.
	if n := vm.network; n != nil {
		if err := vm.api.PutGuestNetworkInterfaceByID(ctx, n.IfaceID, &models.NetworkInterface{
			GuestMac:    n.MAC.String(),
			HostDevName: &n.TapName,
			IfaceID:     &n.IfaceID,
		}); err != nil {
			return fmt.Errorf("configure network interface: %w", err)
		}
	}

	return nil
}

func (vm *VM) networkBootArg() (string, error) {
	if vm.network == nil || vm.network.Subnet == nil {
		return "", fmt.Errorf("VM network is missing subnet information")
	}
	prefix, bits := vm.network.Subnet.Mask.Size()
	if bits != 32 || vm.network.IP.To4() == nil || vm.network.Gateway.To4() == nil {
		return "", fmt.Errorf("VM network must have IPv4 address, gateway, and subnet")
	}
	if !validGuestInterfaceName(vm.network.Interface) {
		return "", fmt.Errorf("invalid guest interface name %q", vm.network.Interface)
	}
	if len(vm.network.MAC) != 6 {
		return "", fmt.Errorf("VM network MAC address must contain 6 bytes")
	}
	if vm.network.MTU < 576 || vm.network.MTU > 65535 {
		return "", fmt.Errorf("invalid VM network MTU %d", vm.network.MTU)
	}
	if len(vm.network.DNS) == 0 {
		return "", fmt.Errorf("VM network has no DNS servers")
	}
	dns := make([]string, len(vm.network.DNS))
	for i, server := range vm.network.DNS {
		if server.To4() == nil {
			return "", fmt.Errorf("VM network DNS server %q is not IPv4", server)
		}
		dns[i] = server.String()
	}
	return fmt.Sprintf(
		"bunn_net=%s|%s|%s/%d|%s|%s|%d",
		vm.network.Interface,
		vm.network.MAC.String(),
		vm.network.IP.To4(),
		prefix,
		vm.network.Gateway.To4(),
		strings.Join(dns, ";"),
		vm.network.MTU,
	), nil
}

func validGuestInterfaceName(name string) bool {
	if name == "" || len(name) > 15 {
		return false
	}
	for _, char := range name {
		if !(char >= 'a' && char <= 'z') &&
			!(char >= 'A' && char <= 'Z') &&
			!(char >= '0' && char <= '9') &&
			char != '_' && char != '-' && char != '.' {
			return false
		}
	}
	return true
}

// configureVsock configures the vsock device using this VM's unique socket path
// and guest CID. Firecracker v1.17.0 permits vsock configuration before
// LoadSnapshot but rejects it after the microVM has started
// ("not supported after starting"). This binds vm.transport and is required
// for host-to-guest vsock connectivity before the guest is started.
func (vm *VM) configureVsock(ctx context.Context) error {
	if err := vm.api.PutGuestVsock(ctx, &models.Vsock{
		GuestCid: int64Ptr(int64(vm.cfg.VsockCID)),
		UdsPath:  &vm.res.VsockSock,
		VsockID:  "vsock0",
	}); err != nil {
		return err
	}
	vm.mu.Lock()
	vm.transport = NewTransport(vm.res.VsockSock, vm.cfg.VsockCID)
	vm.mu.Unlock()
	return nil
}

// allocateNetwork initializes the shared network manager (if not already
// initialized) and allocates network resources for this VM. It binds
// vm.network which is later used by configureDevices to attach the TAP to
// Firecracker.
func (vm *VM) allocateNetwork(ctx context.Context) error {
	if vm.cfg.Network == nil || !vm.cfg.Network.Enabled {
		return nil
	}

	mgr := vm.NetworkManager
	if mgr == nil {
		return fmt.Errorf("network manager not configured")
	}

	n, err := mgr.Allocate(vm.id)
	if err != nil {
		if vm.ownNetworkMgr {
			if cleanupErr := mgr.Shutdown(); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("shutdown network after allocation failure: %w", cleanupErr))
			}
		}
		return err
	}
	vm.network = n
	return nil
}

// releaseNetwork releases this VM's network resources. It is safe to call
// multiple times.
func (vm *VM) releaseNetwork() error {
	vm.mu.Lock()
	n := vm.network
	vm.mu.Unlock()

	if n == nil {
		return nil
	}

	mgr := vm.NetworkManager
	if mgr == nil {
		return fmt.Errorf("release VM network: network manager is missing")
	}
	if err := mgr.Release(vm.id); err != nil {
		return err
	}
	vm.mu.Lock()
	if vm.network == n {
		vm.network = nil
	}
	vm.mu.Unlock()
	return nil
}

// shutDownNetworkMgr shuts down an owned network manager, releasing shared
// resources (bridge, firewall rules).
func (vm *VM) shutDownNetworkMgr() error {
	if !vm.ownNetworkMgr || vm.NetworkManager == nil {
		return nil
	}
	return vm.NetworkManager.Shutdown()
}

// attachVolumes attaches all volumes listed in vm.cfg.Volumes to this VM.
// It maps each volume's qcow2 image to an NBD block device via the VolumeManager.
// Returns the list of NBD device paths that were attached.
func (vm *VM) attachVolumes(ctx context.Context) ([]string, error) {
	if vm.VolumeManager == nil || len(vm.cfg.Volumes) == 0 {
		return nil, nil
	}

	var attached []string
	for _, ref := range vm.cfg.Volumes {
		devicePath, err := vm.VolumeManager.AttachVolume(ctx, ref.VolumeID, vm.id)
		if err != nil {
			// Detach any volumes already attached for this VM on failure.
			for _, dev := range attached {
				_ = vm.detachVolumeByDevice(dev)
			}
			return nil, fmt.Errorf("attach volume %q: %w", ref.VolumeID, err)
		}
		vm.attachedVolumes = append(vm.attachedVolumes, devicePath)
		attached = append(attached, devicePath)
		vm.logger.Info("volume attached",
			map[string]any{"vm_id": vm.id, "volume_id": ref.VolumeID, "device": devicePath})
	}
	return attached, nil
}

// detachVolumes detaches all volumes currently attached to this VM.
func (vm *VM) detachVolumes() error {
	vm.mu.Lock()
	volumes := append([]string(nil), vm.attachedVolumes...)
	vm.attachedVolumes = nil
	vm.mu.Unlock()

	var errs []error
	for _, devicePath := range volumes {
		if err := vm.detachVolumeByDevice(devicePath); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// detachVolumeByDevice detaches a volume by finding the volume ID associated
// with the given NBD device path and calling VolumeManager.DetachVolume.
func (vm *VM) detachVolumeByDevice(devicePath string) error {
	if vm.VolumeManager == nil {
		return nil
	}

	// Find the volume ID for this device by inspecting volumes.
	// The VolumeManager doesn't have a reverse device->volume lookup, so we
	// iterate through known volumes. For simplicity, we use the VM's ID to
	// look up attached volumes via GetVolumeForVM.
	vol, err := vm.VolumeManager.GetVolumeForVM(vm.id)
	if err != nil {
		return err
	}
	if vol == nil || vol.AttachedDevice != devicePath {
		return nil // not our device or already detached
	}
	return vm.VolumeManager.DetachVolume(vol.ID)
}

// shutDownVolumeMgr is a no-op for now since VolumeManager is typically shared
// and not owned by a single VM.
func (vm *VM) shutDownVolumeMgr() error {
	return nil
}

// waitForSocket waits for the Firecracker API socket to accept requests, bounded
// by the VM's StartTimeout. It detects early process exits and returns an error
// if Firecracker exits before initializing its API socket.
func (vm *VM) waitForSocket(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, vm.cfg.StartTimeout)
	defer cancel()

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	api, err := NewAPIClient(vm.res.APISock, vm.logger)
	if err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-vm.proc.Exited():
			return fmt.Errorf("firecracker exited during startup: %w", vm.proc.ExitError())
		case <-ticker.C:
			if _, err := os.Stat(vm.res.APISock); err != nil {
				continue
			}
			if _, err := api.GetMachineConfiguration(context.Background()); err == nil {
				return nil
			}
		}
	}
}

// monitorExit watches for the Firecracker process exiting unexpectedly and
// transitions the VM to Failed when it does while running. Exits that occur
// during Stopping/Stopped/Destroying/Destroyed/Failed are treated as expected
// shutdowns and are not reported as failures. Closes vm.exitCh and records the
// exit error once the process has been reaped.
func (vm *VM) monitorExit() {
	vm.mu.Lock()
	proc := vm.proc
	vm.mu.Unlock()

	if proc == nil {
		return
	}

	<-proc.Exited()

	vm.mu.Lock()
	vm.exitErr = proc.ExitError()
	close(vm.exitCh)
	state := vm.sm.State()
	vm.mu.Unlock()

	switch state {
	case StateStopping, StateStopped, StateDestroying, StateDestroyed, StateFailed:
		return
	}
	vm.sm.TransitionTo(StateFailed)
	vm.logger.WithError(vm.exitErr).Error("Firecracker exited unexpectedly")
}

// stopProcLocked best-effort terminates the Firecracker process on failure
// paths. It sends SIGTERM, waits briefly, then falls back to Kill. Caller does
// not hold vm.mu.
func (vm *VM) stopProcLocked() {
	vm.mu.Lock()
	proc := vm.proc
	vm.mu.Unlock()
	if proc == nil {
		return
	}
	_ = proc.Signal(syscall.SIGTERM)
	select {
	case <-proc.Exited():
	case <-time.After(3 * time.Second):
		_ = proc.Kill()
	}
}

// Pause pauses the guest. Running -> Paused. Reverts to Running if the guest
// patch call fails. Returns an error if the VM is not in the Running state.
func (vm *VM) Pause(ctx context.Context) error {
	if err := vm.sm.Transition(StateRunning, StatePaused); err != nil {
		return fmt.Errorf("pause: %w", err)
	}
	if err := vm.api.PatchVM(ctx, &models.VM{State: vmStatePtr(models.VMStatePaused)}); err != nil {
		_ = vm.sm.Transition(StatePaused, StateRunning)
		return fmt.Errorf("pause guest: %w", err)
	}
	vm.logger.Info("VM paused")
	return nil
}

// Resume resumes a paused guest. Paused -> Running. Reverts to Paused if the
// guest patch call fails. Returns an error if the VM is not in the Paused state.
func (vm *VM) Resume(ctx context.Context) error {
	if err := vm.sm.Transition(StatePaused, StateRunning); err != nil {
		return fmt.Errorf("resume: %w", err)
	}
	if err := vm.api.PatchVM(ctx, &models.VM{State: vmStatePtr(models.VMStateResumed)}); err != nil {
		_ = vm.sm.Transition(StateRunning, StatePaused)
		return fmt.Errorf("resume guest: %w", err)
	}
	vm.logger.Info("VM resumed")
	return nil
}

// Stop gracefully stops the VM, escalating to process termination on timeout.
// Running/Paused -> Stopping -> Stopped. Idempotent: returns nil when already
// Stopped/Destroyed/Failed. Sends Ctrl+Alt+Del for graceful guest shutdown, then
// SIGTERM, then SIGKILL on successive timeouts. Waits for confirmed termination,
// releases runtime resources (sockets), and records the exit error. Never reports
// the VM as stopped before termination is confirmed.
func (vm *VM) Stop(ctx context.Context) error {
	vm.mu.Lock()
	state := vm.sm.State()
	switch state {
	case StateStopped, StateDestroyed, StateFailed:
		vm.mu.Unlock()
		return nil
	case StateRunning, StatePaused:
	case StateStopping:
		vm.mu.Unlock()
		select {
		case <-vm.exitCh:
			return nil
		case <-ctx.Done():
			return fmt.Errorf("stop: waiting for existing shutdown: %w", ctx.Err())
		}
	default:
		vm.mu.Unlock()
		return fmt.Errorf("stop: invalid state %q", state)
	}
	vm.mu.Unlock()

	if err := vm.sm.Transition(state, StateStopping); err != nil {
		return fmt.Errorf("stop: %w", err)
	}

	// Attempt graceful guest shutdown.
	_ = vm.api.CreateSyncAction(ctx, &models.InstanceActionInfo{
		ActionType: instanceActionPtr(models.InstanceActionInfoActionTypeSendCtrlAltDel),
	})

	// Wait for graceful shutdown.
	terminated := false
	select {
	case <-vm.exitCh:
		terminated = true
	case <-time.After(vm.cfg.StopTimeout):
	case <-ctx.Done():
		return ctx.Err()
	}

	if !terminated {
		// Escalate: send SIGTERM.
		if proc := vm.process(); proc != nil {
			_ = proc.Signal(syscall.SIGTERM)
		}
		select {
		case <-vm.exitCh:
			terminated = true
		case <-time.After(vm.cfg.StopGraceTimeout):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	if !terminated {
		// Escalate: send SIGKILL.
		if proc := vm.process(); proc != nil {
			_ = proc.Kill()
		}
		// Wait longer after SIGKILL; the process should exit quickly but may need
		// time to be reaped by the kernel.
		select {
		case <-vm.exitCh:
			terminated = true
		case <-time.After(vm.cfg.StopGraceTimeout):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	if terminated {
		vm.mu.Lock()
		if vm.proc != nil {
			vm.exitErr = vm.proc.ExitError()
		}
		vm.mu.Unlock()

		if err := vm.releaseRuntime(); err != nil {
			vm.logger.WithError(err).Warn("runtime release during stop")
		}

		var networkReleaseErr error
		if err := vm.releaseNetwork(); err != nil {
			vm.logger.WithError(err).Warn("network release during stop")
			networkReleaseErr = fmt.Errorf("stop: release network: %w", err)
		}

		if err := vm.detachVolumes(); err != nil {
			vm.logger.WithError(err).Warn("volume detach during stop")
		}

		vm.unmapImage()

		if err := vm.sm.Transition(StateStopping, StateStopped); err != nil {
			return fmt.Errorf("finalize stop: %w", err)
		}

		vm.logger.Info("VM stopped")
		return networkReleaseErr
	}

	// If we reach here, SIGKILL was sent but the process has not been reaped.
	// It may be a zombie or stuck in uninterruptible sleep. Report failure
	// without claiming the VM is stopped.
	proc := vm.process()
	fields := logrus.Fields{
		"state": vm.State(),
	}
	if proc != nil {
		fields["pid"] = proc.PID()
		fields["reaped"] = proc.Reaped()
		fields["exitErr"] = proc.ExitError()
	}
	vm.logger.WithFields(fields).Error("Firecracker did not terminate after SIGKILL escalation")
	return fmt.Errorf("stop: firecracker did not terminate after escalation")
}

// Destroy ensures the VM cannot keep running, closes sockets, and removes
// VM-owned temporary artifacts. Shared kernel/rootfs artifacts are preserved.
// Idempotent and safe to retry after partial cleanup. Attempts graceful shutdown
// first, force-terminates any still-running process, then removes socket files and
// the working-directory contents. Cleanup errors are collected and returned; they
// are never silently ignored.
func (vm *VM) Destroy(ctx context.Context) error {
	vm.mu.Lock()
	state := vm.sm.State()
	vm.mu.Unlock()

	if state == StateDestroyed {
		return nil
	}

	// Best-effort graceful shutdown first, while the state machine still
	// reflects a stoppable state.
	switch state {
	case StateRunning, StatePaused:
		_ = vm.Stop(ctx)
	case StateFailed:
		if proc := vm.process(); proc != nil {
			_ = proc.Kill()
		}
	}

	// Mark Destroying so an unexpected process exit is not reported as a
	// lifecycle failure; then force-terminate anything still running.
	vm.sm.TransitionTo(StateDestroying)

	if proc := vm.process(); proc != nil {
		_ = proc.Kill()
		select {
		case <-proc.Exited():
		case <-time.After(5 * time.Second):
			select {
			case <-proc.Exited():
			default:
				return fmt.Errorf("destroy: firecracker process %d did not terminate; VM resources were retained", proc.PID())
			}
		}
	}

	var errs []error
	if err := vm.releaseRuntime(); err != nil {
		errs = append(errs, fmt.Errorf("release runtime: %w", err))
	}
	if err := vm.releaseNetwork(); err != nil {
		errs = append(errs, fmt.Errorf("release network: %w", err))
	}
	if err := vm.detachVolumes(); err != nil {
		errs = append(errs, fmt.Errorf("detach volumes: %w", err))
	}
	if err := vm.removeOwnedArtifacts(); err != nil {
		errs = append(errs, fmt.Errorf("remove artifacts: %w", err))
	}

	vm.removeWritableLayer()
	vm.unmapImage()

	if err := vm.shutDownNetworkMgr(); err != nil {
		errs = append(errs, fmt.Errorf("shutdown network manager: %w", err))
	}

	vm.mu.Lock()
	vm.api = nil
	vm.proc = nil
	vm.transport = nil
	vm.mu.Unlock()

	vm.sm.TransitionTo(StateDestroyed)

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	vm.logger.Info("VM destroyed")
	return nil
}

// releaseRuntime drops transport/API references and removes socket files owned
// by this VM. It does not remove the working directory itself. Cleanup errors
// are collected and returned.
func (vm *VM) releaseRuntime() error {
	vm.mu.Lock()
	vm.transport = nil
	vm.api = nil
	sock := vm.res.APISock
	vsock := vm.res.VsockSock
	vm.mu.Unlock()

	var errs []error
	for _, p := range []string{sock, vsock} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// removeOwnedArtifacts removes the VM's private working directory and contents.
// Shared kernel and rootfs images are never removed.
func (vm *VM) removeOwnedArtifacts() error {
	dir := vm.res.Dir
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var errs []error
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if err := os.RemoveAll(p); err != nil {
			errs = append(errs, err)
		}
	}
	if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// resolveTemplate looks up the configured template, validates its integrity,
// acquires a reference, and creates this VM's independent writable layer. The
// VM's RootFS is then pointed at the layer copy so Firecracker boots from it.
// The template rootfs itself is never attached writably to any VM.
func (vm *VM) resolveTemplate(ctx context.Context) error {
	if vm.TemplateStore == nil {
		return fmt.Errorf("template id %q set but no template store configured", vm.cfg.TemplateID)
	}

	tmpl := vm.TemplateStore.Get(vm.cfg.TemplateID)
	if tmpl == nil {
		return fmt.Errorf("template %q not found", vm.cfg.TemplateID)
	}

	if err := vm.TemplateStore.Validate(vm.cfg.TemplateID); err != nil {
		return fmt.Errorf("template %q validation failed: %w", vm.cfg.TemplateID, err)
	}

	if err := vm.TemplateStore.Acquire(vm.cfg.TemplateID); err != nil {
		return fmt.Errorf("acquire template %q: %w", vm.cfg.TemplateID, err)
	}

	if vm.LayerMgr == nil {
		vm.TemplateStore.Release(vm.cfg.TemplateID)
		return fmt.Errorf("layer manager not configured")
	}

	if vm.NBDMgr == nil {
		vm.TemplateStore.Release(vm.cfg.TemplateID)
		return fmt.Errorf("NBD manager not configured")
	}

	layerPath, err := vm.LayerMgr.CreateLayer(vm.cfg.TemplateID, vm.id)
	if err != nil {
		vm.TemplateStore.Release(vm.cfg.TemplateID)
		return fmt.Errorf("create writable layer: %w", err)
	}

	// Map the qcow2 overlay to an NBD block device. Firecracker does not
	// understand qcow2 natively; the NBD device exposes the overlay as a raw
	// block device via qemu-nbd.
	nbdDevice, err := vm.NBDMgr.Map(ctx, layerPath, vm.id)
	if err != nil {
		vm.TemplateStore.Release(vm.cfg.TemplateID)
		_ = vm.LayerMgr.RemoveLayer(vm.id)
		return fmt.Errorf("map overlay to NBD device: %w", err)
	}

	vm.mu.Lock()
	vm.writableLayer = layerPath
	vm.nbdDevice = nbdDevice
	vm.cfg.RootFS = nbdDevice
	vm.mu.Unlock()

	vm.logger.Info("template resolved",
		map[string]any{"template_id": vm.cfg.TemplateID, "layer_path": layerPath, "nbd_device": nbdDevice})

	// Register the VM and its overlay with the storage manager for lifecycle
	// tracking and crash recovery.
	if vm.StorageManager != nil {
		if err := vm.StorageManager.RegisterVM(vm.id, vm.cfg.TemplateID, layerPath, nbdDevice); err != nil {
			// Roll back: ReleaseVM cleans up any partially registered resources
			// (VM resource, overlay resource, backing ref, template ref).
			_ = vm.StorageManager.ReleaseVM(vm.id)
			_ = vm.NBDMgr.Unmap(vm.id)
			return fmt.Errorf("register VM storage: %w", err)
		}
	}

	return nil
}

// removeWritableLayer releases this VM's writable layer, NBD mapping, and
// coordinates with the StorageManager to release template references and
// overlay metadata. It is idempotent and safe to call after partial creation.
func (vm *VM) removeWritableLayer() {
	vm.mu.Lock()
	layer := vm.writableLayer
	nbdDevice := vm.nbdDevice
	templateID := vm.cfg.TemplateID
	vm.writableLayer = ""
	vm.nbdDevice = ""
	vm.mu.Unlock()

	if layer == "" && nbdDevice == "" {
		return
	}

	vm.logger.Info("releasing VM writable layer",
		map[string]any{"vm_id": vm.id, "layer_path": layer, "nbd_device": nbdDevice, "template_id": templateID})

	// Unmap the NBD device (stops qemu-nbd) if one was mapped.
	if vm.NBDMgr != nil && nbdDevice != "" {
		_ = vm.NBDMgr.Unmap(vm.id)
	}

	if vm.StorageManager != nil {
		// The StorageManager owns overlay metadata, backing-image references,
		// and template references. ReleaseVM handles all of them atomically.
		_ = vm.StorageManager.ReleaseVM(vm.id)
		// Remove the layer file itself (metadata is deleted by ReleaseVM).
		if layer != "" {
			_ = os.Remove(layer)
		}
		return
	}

	// Backward-compatible path: no StorageManager. Remove layer metadata and
	// file, and release the template reference directly.
	if vm.LayerMgr != nil && layer != "" {
		_ = vm.LayerMgr.RemoveLayer(vm.id)
	}
	if vm.TemplateStore != nil {
		// The template ID is still in cfg.TemplateID at this point.
		vm.TemplateStore.Release(vm.cfg.TemplateID)
	}
}

// resolveImage resolves the configured image, maps its qcow2 file to an NBD
// block device, and sets vm.cfg.RootFS to the device path. The image is used
// as a read-only root device; VM writes go to an overlay if configured.
func (vm *VM) resolveImage(ctx context.Context) error {
	if vm.ImageManager == nil {
		return fmt.Errorf("image id %q set but no image manager configured", vm.cfg.ImageID)
	}
	if vm.NBDMgr == nil {
		return fmt.Errorf("image id %q set but no NBD manager configured", vm.cfg.ImageID)
	}

	img, err := vm.ImageManager.InspectImage(vm.cfg.ImageID)
	if err != nil {
		return fmt.Errorf("inspect image %q: %w", vm.cfg.ImageID, err)
	}
	if img == nil {
		return fmt.Errorf("image %q not found", vm.cfg.ImageID)
	}
	if img.State != vmimage.ImageStateActive {
		return fmt.Errorf("image %q is not active (state=%s)", vm.cfg.ImageID, img.State)
	}

	// Map the image qcow2 to an NBD block device.
	nbdDevice, err := vm.NBDMgr.Map(ctx, img.Path, vm.id)
	if err != nil {
		return fmt.Errorf("map image to NBD device: %w", err)
	}

	vm.mu.Lock()
	vm.imageNBDDevice = nbdDevice
	vm.cfg.RootFS = nbdDevice
	vm.mu.Unlock()

	vm.logger.Info("image resolved",
		map[string]any{"image_id": vm.cfg.ImageID, "image_path": img.Path, "nbd_device": nbdDevice})

	return nil
}

// unmapImage unmaps the image NBD device and clears the RootFS reference.
func (vm *VM) unmapImage() {
	vm.mu.Lock()
	nbdDevice := vm.imageNBDDevice
	vm.imageNBDDevice = ""
	vm.cfg.RootFS = ""
	vm.mu.Unlock()

	if nbdDevice == "" {
		return
	}

	if vm.NBDMgr != nil {
		_ = vm.NBDMgr.Unmap(vm.id)
	}
}

// process returns the current Firecracker process handle, or nil if not started.
// Caller does not hold vm.mu.
func (vm *VM) process() *Process {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	return vm.proc
}

// Wait blocks until the Firecracker process exits or ctx is cancelled. It
// surfaces unexpected process exits via vm.exitErr. Returns an error if the VM
// has not been started.
func (vm *VM) Wait(ctx context.Context) error {
	vm.mu.Lock()
	proc := vm.proc
	ch := vm.exitCh
	vm.mu.Unlock()
	if proc == nil {
		return errors.New("vm not started")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-ch:
		return vm.exitErr
	}
}

// IsRunning reports whether the VM is in the Running state.
func (vm *VM) IsRunning() bool { return vm.sm.State() == StateRunning }

// destroy is an internal destroy used by fork failure cleanup. If force is set,
// the Firecracker process is killed before cleanup. It releases runtime resources
// and removes owned artifacts, then transitions to Destroyed.
func (vm *VM) destroy(ctx context.Context, force bool) error {
	if force {
		if proc := vm.process(); proc != nil {
			_ = proc.Kill()
		}
	}
	_ = vm.releaseRuntime()
	_ = vm.removeOwnedArtifacts()
	vm.removeWritableLayer()
	vm.sm.TransitionTo(StateDestroyed)
	return nil
}

// helpers

func int64Ptr(v int64) *int64    { return &v }
func boolPtr(v bool) *bool       { return &v }
func stringPtr(v string) *string { return &v }
func vmStatePtr(v string) *string {
	return &v
}
func instanceActionPtr(v string) *string { return &v }
func cacheTypePtr(v string) *string      { return &v }
func ioEnginePtr(v string) *string       { return &v }

var rootfsDriveID = "rootfs"

func minimalEnv() []string {
	return []string{"PATH=/usr/local/bin:/usr/bin:/bin"}
}

func resolveFirecrackerBin() string {
	if p, err := exec.LookPath("firecracker"); err == nil {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "bin", "firecracker")
	}
	return "firecracker"
}

var _ = syscall.SIGTERM
var _ = context.Background

package vmm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"

	"github.com/omkar/sandbox/internal/storage"
	"github.com/omkar/sandbox/internal/template"
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

	// writableLayer is the path to this VM's writable layer copy when booted
	// from a template. It is cleared once the layer is released.
	writableLayer string
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

	vm := &VM{
		id:     id,
		cfg:    cfg,
		res:    cfg.resources(id),
		sm:     NewStateMachine(),
		exitCh: make(chan struct{}),
		logger: logger.WithField("vm", id),
	}

	// Create the VM's private working directory so Firecracker can place its
	// API socket and other per-VM artifacts there.
	if err := os.MkdirAll(vm.res.Dir, 0750); err != nil {
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
func (vm *VM) Start(ctx context.Context) error {
	if err := vm.sm.Transition(StateCreated, StatePreparing); err != nil {
		return fmt.Errorf("start: %w", err)
	}

	// Resolve the template and prepare per-VM writable storage before Firecracker
	// starts. If a layer is created but startup later fails, it is cleaned up by
	// the deferred removal below.
	var layerCreated bool
	defer func() {
		if layerCreated && vm.State() != StateRunning {
			vm.removeWritableLayer()
		}
	}()

	if vm.cfg.TemplateID != "" {
		if err := vm.resolveTemplate(ctx); err != nil {
			vm.sm.TransitionTo(StateFailed)
			return err
		}
		layerCreated = true
	}

	if err := vm.startProcessAndWait(ctx); err != nil {
		vm.sm.TransitionTo(StateFailed)
		return err
	}

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

	if err := vm.sm.Transition(StatePreparing, StateRunning); err != nil {
		vm.stopProcLocked()
		return fmt.Errorf("finalize start: %w", err)
	}

	vm.logger.Info("VM started")
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
	return nil
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

	select {
	case <-vm.exitCh:
		goto terminated
	case <-time.After(vm.cfg.StopTimeout):
	}

	if proc := vm.process(); proc != nil {
		_ = proc.Signal(syscall.SIGTERM)
	}
	select {
	case <-vm.exitCh:
		goto terminated
	case <-time.After(vm.cfg.StopGraceTimeout):
	}

	if proc := vm.process(); proc != nil {
		_ = proc.Kill()
	}
	select {
	case <-vm.exitCh:
		goto terminated
	case <-time.After(vm.cfg.StopGraceTimeout):
		proc := vm.process()

		fields := logrus.Fields{
			"state": vm.State(),
		}

		if proc != nil {
			fields["pid"] = proc.PID()
			fields["reaped"] = proc.Reaped()
			fields["exitErr"] = proc.ExitError()
		}

		vm.logger.WithFields(fields).Error(
			"Firecracker termination wait timed out",
		)

		return fmt.Errorf("stop: firecracker did not terminate after escalation")
	}

terminated:
	vm.mu.Lock()
	vm.exitErr = vm.proc.ExitError()
	vm.mu.Unlock()

	if err := vm.releaseRuntime(); err != nil {
		vm.logger.WithError(err).Warn("runtime release during stop")
	}

	if err := vm.sm.Transition(StateStopping, StateStopped); err != nil {
		return fmt.Errorf("finalize stop: %w", err)
	}

	vm.logger.Info("VM stopped")
	return nil
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
	case StateRunning, StatePaused, StateStopping:
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
		}
	}

	var errs []error
	if err := vm.releaseRuntime(); err != nil {
		errs = append(errs, fmt.Errorf("release runtime: %w", err))
	}
	if err := vm.removeOwnedArtifacts(); err != nil {
		errs = append(errs, fmt.Errorf("remove artifacts: %w", err))
	}

	vm.removeWritableLayer()

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

// removeOwnedArtifacts removes the VM's private working directory contents.
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

	layerPath, err := vm.LayerMgr.CreateLayer(vm.cfg.TemplateID, vm.id)
	if err != nil {
		vm.TemplateStore.Release(vm.cfg.TemplateID)
		return fmt.Errorf("create writable layer: %w", err)
	}

	vm.mu.Lock()
	vm.writableLayer = layerPath
	vm.cfg.RootFS = layerPath
	vm.mu.Unlock()

	return nil
}

// removeWritableLayer releases this VM's writable layer and the template
// reference it holds. It is idempotent and safe to call after partial creation.
func (vm *VM) removeWritableLayer() {
	vm.mu.Lock()
	layer := vm.writableLayer
	templateID := vm.cfg.TemplateID
	vm.writableLayer = ""
	vm.mu.Unlock()

	if layer == "" {
		return
	}
	if vm.LayerMgr != nil {
		_ = vm.LayerMgr.RemoveLayer(vm.id)
	}
	if vm.TemplateStore != nil && templateID != "" {
		vm.TemplateStore.Release(templateID)
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

func int64Ptr(v int64) *int64 { return &v }
func boolPtr(v bool) *bool    { return &v }
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
	return "/home/omkar/.local/bin/firecracker"
}

var _ = syscall.SIGTERM
var _ = context.Background

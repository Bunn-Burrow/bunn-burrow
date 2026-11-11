package storage

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// NBDDevice tracks a qemu-nbd mapping between a qcow2 image and an NBD block
// device. Firecracker does not understand qcow2 natively, so each VM's qcow2
// overlay is exposed to Firecracker as a raw block device via qemu-nbd.
type NBDDevice struct {
	// Device is the NBD device path (e.g. /dev/nbd0).
	Device string

	// Number is the NBD device minor number (e.g. 0 for /dev/nbd0).
	Number int

	// Image is the qcow2 image path mapped to the device.
	Image string

	// VMID is the ID of the VM that owns this mapping.
	VMID string

	// proc is the qemu-nbd process serving the device.
	proc *os.Process

	// pid is the qemu-nbd process PID, recorded so the process can be
	// validated across manager restarts.
	pid int

	// startedAt records when the mapping was established.
	startedAt time.Time
}

// NBDManager manages qemu-nbd processes that expose qcow2 images as NBD block
// devices. Each VM receives a unique NBD device so Firecracker can boot from
// the VM's writable qcow2 overlay. The backing image remains immutable.
//
// NBD device allocation is unique per manager instance: no two VMs share the
// same NBD device, preventing concurrent write conflicts. The manager does not
// persist mappings across process restarts; orphaned NBD devices from crashed
// VMs must be cleaned up manually via ReconcileNBD.
type NBDManager struct {
	mu      sync.Mutex
	devices map[int]*NBDDevice    // by NBD device number
	byVM    map[string]*NBDDevice // by VM ID
	nextNBD int
	maxNBD  int
	nbdDir  string
	// lockDir is the directory qemu-nbd uses for its lock socket files
	// (typically "/var/lock").
	lockDir string
}

// NewNBDManager creates an NBD manager that allocates devices from startNBD to
// maxNBD (exclusive). nbdDir is the directory containing /dev/nbd* devices
// (typically "/dev"). lockDir is the qemu-nbd lock directory.
func NewNBDManager(startNBD, maxNBD int, nbdDir, lockDir string) *NBDManager {
	if startNBD < 0 {
		startNBD = 0
	}
	if maxNBD <= startNBD {
		maxNBD = startNBD + 16
	}
	if nbdDir == "" {
		nbdDir = "/dev"
	}
	if lockDir == "" {
		lockDir = "/var/lock"
	}
	return &NBDManager{
		devices: make(map[int]*NBDDevice),
		byVM:    make(map[string]*NBDDevice),
		nextNBD: startNBD,
		maxNBD:  maxNBD,
		nbdDir:  nbdDir,
		lockDir: lockDir,
	}
}

// Map exposes the qcow2 image at imagePath as an NBD block device owned by
// vmID. It allocates a unique NBD device, starts qemu-nbd to serve the image,
// waits for the device to become ready, and returns the device path (e.g.
// /dev/nbd0). The returned path is safe to pass to Firecracker as PathOnHost.
// The qemu-nbd process must remain running for the device to remain valid;
// call Unmap when the VM no longer needs it.
//
// Context cancellation aborts the operation. On cancellation, any partially
// created mapping is cleaned up.
func (m *NBDManager) Map(ctx context.Context, imagePath, vmID string) (string, error) {
	m.mu.Lock()
	if d, ok := m.byVM[vmID]; ok {
		m.mu.Unlock()
		return d.Device, nil // already mapped for this VM
	}

	number, err := m.allocateNBD()
	if err != nil {
		m.mu.Unlock()
		return "", err
	}
	device := filepath.Join(m.nbdDir, fmt.Sprintf("nbd%d", number))

	d := &NBDDevice{
		Device:    device,
		Number:    number,
		Image:     imagePath,
		VMID:      vmID,
		startedAt: time.Now(),
	}
	m.devices[number] = d
	m.byVM[vmID] = d
	m.mu.Unlock()

	// Start qemu-nbd to serve the image over the NBD device. The process runs
	// in the foreground and must stay alive while Firecracker uses the device.
	if err := m.startQemuNBD(d); err != nil {
		m.mu.Lock()
		delete(m.devices, number)
		delete(m.byVM, vmID)
		m.mu.Unlock()
		return "", fmt.Errorf("start qemu-nbd for %q: %w", imagePath, err)
	}

	// Wait for the NBD device to become ready, honoring context cancellation.
	if err := m.waitForDeviceCtx(ctx, d.Device); err != nil {
		m.Unmap(vmID)
		return "", fmt.Errorf("NBD device %q not ready: %w", device, err)
	}

	// Verify the qemu-nbd process is serving the expected image by checking
	// its /proc/PID/cmdline. This catches cases where qemu-nbd started but
	// is not actually serving our image (e.g. wrong arguments, permission
	// denied).
	if err := m.verifyMapping(d); err != nil {
		m.Unmap(vmID)
		return "", fmt.Errorf("NBD mapping verification failed for %q: %w", imagePath, err)
	}

	return device, nil
}

// UnmapContext disconnects the NBD device owned by vmID and stops the qemu-nbd
// process. It is idempotent: unmapping an unknown VM is not an error. The
// NBD device number is released for reuse. Context cancellation is honored
// for the termination wait.
func (m *NBDManager) UnmapContext(ctx context.Context, vmID string) error {
	m.mu.Lock()
	d, ok := m.byVM[vmID]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	delete(m.devices, d.Number)
	delete(m.byVM, vmID)
	m.mu.Unlock()

	if d.proc != nil {
		_ = d.proc.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() {
			_, _ = d.proc.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-ctx.Done():
			_ = d.proc.Kill()
			<-done
		}
	}
	// Best-effort removal of only this manager's own qemu-nbd lock file. Never
	// remove lock files that may belong to other applications.
	lockPath := filepath.Join(m.lockDir, fmt.Sprintf("qemu-nbd-nbd%d", d.Number))
	_ = os.Remove(lockPath)

	return nil
}

// Unmap disconnects the NBD device owned by vmID and stops the qemu-nbd
// process. It is idempotent: unmapping an unknown VM is not an error. The
// NBD device number is released for reuse.
func (m *NBDManager) Unmap(vmID string) error {
	return m.UnmapContext(context.Background(), vmID)
}

// DevicePath returns the NBD device path for vmID, or empty string if unmapped.
func (m *NBDManager) DevicePath(vmID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d, ok := m.byVM[vmID]; ok {
		return d.Device
	}
	return ""
}

// IsMapped reports whether vmID currently has an NBD device mapped.
func (m *NBDManager) IsMapped(vmID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.byVM[vmID]
	return ok
}

// allocateNBD finds and marks the next free NBD device number.
func (m *NBDManager) allocateNBD() (int, error) {
	for i := 0; i < m.maxNBD-m.nextNBD; i++ {
		n := (m.nextNBD + i) % m.maxNBD
		if _, ok := m.devices[n]; !ok {
			m.nextNBD = (n + 1) % m.maxNBD
			return n, nil
		}
	}
	return -1, fmt.Errorf("no free NBD devices (range 0-%d)", m.maxNBD-1)
}

// startQemuNBD launches qemu-nbd to connect image to device. The process runs
// in the foreground and is stored in d.proc so it can be terminated on unmap.
func (m *NBDManager) startQemuNBD(d *NBDDevice) error {
	cmd := exec.Command("qemu-nbd", "-f", "qcow2", "-c", d.Device, d.Image)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("launch qemu-nbd: %w", err)
	}
	d.proc = cmd.Process
	d.pid = d.proc.Pid
	return nil
}

// waitForDeviceCtx polls the NBD device path until it is statable as a block
// device, bounded by a short timeout and honoring context cancellation.
func (m *NBDManager) waitForDeviceCtx(ctx context.Context, device string) error {
	deadline := time.Now().Add(10 * time.Second)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			fi, err := os.Stat(device)
			if err == nil && (fi.Mode()&os.ModeDevice) != 0 {
				return nil
			}
		case <-time.After(time.Until(deadline)):
			return fmt.Errorf("device %q not ready within timeout", device)
		}
	}
}

// verifyMapping confirms the qemu-nbd process for device d is serving the
// expected image by reading /proc/PID/cmdline and checking that the image
// path appears as an argument. This catches cases where qemu-nbd started
// but is not actually serving our image.
func (m *NBDManager) verifyMapping(d *NBDDevice) error {
	if d.proc == nil {
		return fmt.Errorf("qemu-nbd process is nil")
	}
	pid := d.proc.Pid
	cmdlinePath := filepath.Join("/proc", fmt.Sprintf("%d", pid), "cmdline")
	data, err := os.ReadFile(cmdlinePath)
	if err != nil {
		return fmt.Errorf("read qemu-nbd cmdline: %w", err)
	}
	// /proc/PID/cmdline uses null bytes as argument separators.
	args := strings.Split(string(data), "\x00")
	for _, arg := range args {
		if arg == d.Image {
			return nil // image path found in qemu-nbd arguments
		}
	}
	return fmt.Errorf("qemu-nbd (pid=%d) is not serving image %q", pid, d.Image)
}

// ProcessIsQemuNBD checks whether the process with the given PID is a
// qemu-nbd process by reading /proc/PID/cmdline. This protects against PID
// reuse where a different process has taken over a qemu-nbd PID.
func ProcessIsQemuNBD(pid int) bool {
	cmdlinePath := filepath.Join("/proc", fmt.Sprintf("%d", pid), "cmdline")
	data, err := os.ReadFile(cmdlinePath)
	if err != nil {
		return false
	}
	args := strings.Split(string(data), "\x00")
	for _, arg := range args {
		if strings.Contains(arg, "qemu-nbd") {
			return true
		}
	}
	return false
}

// DeviceIsBlock reports whether the given path is a block device.
func DeviceIsBlock(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeDevice) != 0
}

// nbdLockPath returns the qemu-nbd lock socket path for a device number.
func nbdLockPath(lockDir string, number int) string {
	return filepath.Join(lockDir, fmt.Sprintf("qemu-nbd-nbd%d", number))
}

// NBDMappingRecord describes a live NBD mapping discovered from system state.
type NBDMappingRecord struct {
	// Device is the NBD device path (e.g. /dev/nbd0).
	Device string
	// Number is the NBD device minor number.
	Number int
	// LockPath is the qemu-nbd lock socket path, if it exists.
	LockPath string
	// HasLock reports whether a qemu-nbd lock socket exists for this device.
	HasLock bool
}

// ListNBDDevices scans nbdDir for NBD block devices and returns their records.
// lockDir is the directory used for qemu-nbd lock socket files.
func ListNBDDevices(nbdDir, lockDir string) ([]NBDMappingRecord, error) {
	entries, err := os.ReadDir(nbdDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var records []NBDMappingRecord
	for _, e := range entries {
		name := e.Name()
		// Only match /dev/nbdN where N is one or more digits.
		// Reject names like "nbd" (no number) or "nbd0p1" (partition suffix).
		if len(name) < 4 || name[:3] != "nbd" {
			continue
		}
		suffix := name[3:]
		allDigits := true
		for _, c := range suffix {
			if c < '0' || c > '9' {
				allDigits = false
				break
			}
		}
		if !allDigits {
			continue
		}
		number := 0
		_, err := fmt.Sscanf(suffix, "%d", &number)
		if err != nil {
			continue
		}
		device := filepath.Join(nbdDir, name)
		records = append(records, NBDMappingRecord{
			Device:   device,
			Number:   number,
			LockPath: nbdLockPath(lockDir, number),
		})
	}
	return records, nil
}

// ReconcileNBD detects stale NBD mappings owned by this manager and cleans them
// up. It inspects actual system state (NBD devices, qemu-nbd lock files,
// qemu-nbd processes) rather than relying solely on in-memory mappings.
//
// For each NBD device that has a lock file but is not tracked by this manager:
//   - If the lock socket is stale (no live qemu-nbd process owns it), the
//     device is considered orphaned and its lock file is removed.
//   - If the lock socket is live, the device is assumed to be owned by another
//     application and is left alone.
//
// For each mapping tracked by this manager whose qemu-nbd process has exited:
//   - The mapping is removed from in-memory state and the NBD device number is
//     released.
//
// Returns a report of devices still mapped (in-memory), devices found orphaned
// (lock file removed), and devices left alone (owned by others).
func (m *NBDManager) ReconcileNBD(ctx context.Context) (*NBDReconcileReport, error) {
	m.mu.Lock()
	tracked := make(map[int]*NBDDevice, len(m.devices))
	for n, d := range m.devices {
		tracked[n] = d
	}
	m.mu.Unlock()

	report := &NBDReconcileReport{}

	// Scan actual NBD devices.
	devices, err := ListNBDDevices(m.nbdDir, m.lockDir)
	if err != nil {
		return nil, err
	}

	for _, rec := range devices {
		if _, tracked := tracked[rec.Number]; tracked {
			continue // this manager tracks this device; handled below
		}

		// Not tracked by this manager. Check if a lock file exists.
		if rec.LockPath == "" {
			continue
		}
		if _, err := os.Stat(rec.LockPath); err != nil {
			if os.IsNotExist(err) {
				continue // no lock, nothing to do
			}
			continue
		}
		rec.HasLock = true

		// A lock file exists but this manager doesn't own the device. We
		// conservatively leave the device alone rather than disconnect a
		// device that may belong to another application. The lock file itself
		// is harmless.
		report.OrphanedLocks = append(report.OrphanedLocks, rec.Device)
	}

	// Check tracked mappings: if the qemu-nbd process has exited, clean up.
	for _, d := range tracked {
		select {
		case <-ctx.Done():
			return report, ctx.Err()
		default:
		}

		if d.proc != nil {
			// Check process identity: Signal(0) checks if the PID exists, but
			// PID reuse could mean a different process now owns it. Verify the
			// process is actually qemu-nbd before considering it active.
			if err := d.proc.Signal(syscall.Signal(0)); err != nil {
				// Process has exited; remove the stale mapping.
				if err := m.Unmap(d.VMID); err != nil {
					report.Errors = append(report.Errors, fmt.Errorf("unmap stale NBD for VM %q: %w", d.VMID, err))
				} else {
					report.Cleaned = append(report.Cleaned, d.Device)
				}
			} else if ProcessIsQemuNBD(d.proc.Pid) {
				report.Active = append(report.Active, d.Device)
			} else {
				// PID was reused by a non-qemu-nbd process; treat as stale.
				if err := m.Unmap(d.VMID); err != nil {
					report.Errors = append(report.Errors, fmt.Errorf("unmap stale NBD (PID reused) for VM %q: %w", d.VMID, err))
				} else {
					report.Cleaned = append(report.Cleaned, d.Device)
				}
			}
		} else {
			// No process handle: cannot verify liveness. If the NBD device no
			// longer exists as a block device, the mapping is stale.
			if !DeviceIsBlock(d.Device) {
				if err := m.Unmap(d.VMID); err != nil {
					report.Errors = append(report.Errors, fmt.Errorf("unmap stale NBD (no device) for VM %q: %w", d.VMID, err))
				} else {
					report.Cleaned = append(report.Cleaned, d.Device)
				}
			} else {
				// Device exists but we have no process handle; cannot confirm
				// ownership. Leave it alone to avoid disconnecting a foreign
				// process.
				report.Active = append(report.Active, d.Device)
			}
		}
	}

	return report, nil
}

// NBDReconcileReport reports the results of ReconcileNBD.
type NBDReconcileReport struct {
	// Active are NBD devices currently served by live qemu-nbd processes owned
	// by this manager.
	Active []string
	// Cleaned are NBD devices whose stale qemu-nbd processes were terminated and
	// mappings removed.
	Cleaned []string
	// OrphanedLocks are NBD devices with lock files not owned by this manager
	// (left alone).
	OrphanedLocks []string
	// Errors are non-fatal errors encountered during reconciliation.
	Errors []error
}

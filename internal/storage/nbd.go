package storage

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
}

// NBDManager manages qemu-nbd processes that expose qcow2 images as NBD block
// devices. Each VM receives a unique NBD device so Firecracker can boot from
// the VM's writable qcow2 overlay. The backing image remains immutable.
//
// NBD device allocation is unique per manager instance: no two VMs share the
// same NBD device, preventing concurrent write conflicts. The manager does not
// persist mappings across process restarts; orphaned NBD devices from crashed
// VMs must be cleaned up manually.
type NBDManager struct {
	mu      sync.Mutex
	devices map[int]*NBDDevice    // by NBD device number
	byVM    map[string]*NBDDevice // by VM ID
	nextNBD int
	maxNBD  int
	nbdDir  string
}

// NewNBDManager creates an NBD manager that allocates devices from startNBD to
// maxNBD (exclusive). nbdDir is the directory containing /dev/nbd* devices
// (typically "/dev").
func NewNBDManager(startNBD, maxNBD int, nbdDir string) *NBDManager {
	if startNBD < 0 {
		startNBD = 0
	}
	if maxNBD <= startNBD {
		maxNBD = startNBD + 16
	}
	if nbdDir == "" {
		nbdDir = "/dev"
	}
	return &NBDManager{
		devices: make(map[int]*NBDDevice),
		byVM:    make(map[string]*NBDDevice),
		nextNBD: startNBD,
		maxNBD:  maxNBD,
		nbdDir:  nbdDir,
	}
}

// Map exposes the qcow2 image at imagePath as an NBD block device owned by
// vmID. It allocates a unique NBD device, starts qemu-nbd to serve the image,
// waits for the device to become ready, and returns the device path (e.g.
// /dev/nbd0). The returned path is safe to pass to Firecracker as PathOnHost.
// The qemu-nbd process must remain running for the device to remain valid;
// call Unmap when the VM no longer needs it.
func (m *NBDManager) Map(imagePath, vmID string) (string, error) {
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
		Device: device,
		Number: number,
		Image:  imagePath,
		VMID:   vmID,
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

	// Wait briefly for the NBD device to become ready before returning.
	if err := m.waitForDevice(d.Device); err != nil {
		m.Unmap(vmID)
		return "", fmt.Errorf("NBD device %q not ready: %w", device, err)
	}

	return device, nil
}

// Unmap disconnects the NBD device owned by vmID and stops the qemu-nbd
// process. It is idempotent: unmapping an unknown VM is not an error. The
// NBD device number is released for reuse.
func (m *NBDManager) Unmap(vmID string) error {
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
		case <-time.After(5 * time.Second):
			_ = d.proc.Kill()
		}
	}
	// Best-effort removal of the qemu-nbd lock file.
	lockPath := filepath.Join("/var/lock", fmt.Sprintf("qemu-nbd-nbd%d", d.Number))
	_ = os.Remove(lockPath)

	return nil
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
	return nil
}

// waitForDevice polls the NBD device path until it is statable as a block
// device, bounded by a short timeout.
func (m *NBDManager) waitForDevice(device string) error {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		fi, err := os.Stat(device)
		if err == nil && (fi.Mode()&os.ModeDevice) != 0 {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("device %q not ready within timeout", device)
}

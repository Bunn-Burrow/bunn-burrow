package vmm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Config holds the configuration required to create and start a Firecracker
// microVM. All paths must be absolute and point at existing artifacts unless
// otherwise noted.
type Config struct {
	// ID is the unique VM identifier. If empty, a UUID is generated on Create.
	ID string

	// KernelImage is the path to an uncompressed ELF kernel image.
	KernelImage string

	// RootFS is the path to the guest root filesystem image.
	RootFS string

	// BootArgs are the kernel command-line arguments.
	BootArgs string

	// VCPU is the number of vCPUs (1 or an even number, max 32).
	VCPU int64

	// MemSizeMiB is the guest memory size in MiB.
	MemSizeMiB int64

	// VsockCID is the vsock Context Identifier for the guest.
	VsockCID uint32

	// VsockPort is the guest-side vsock port host connections target.
	VsockPort uint32

// WorkDir is the base directory under which per-VM working directories are
// created. If empty, os.MkdirTemp is used.
WorkDir string

// BinPath is the path to the Firecracker binary. If empty, "firecracker" is
// resolved from PATH, falling back to a well-known location.
BinPath string

// LogFile is where Firecracker writes its log.
LogFile string

// RootFSReadOnly makes the guest root filesystem read-only. Used when several
// VMs share one rootfs image (e.g. an unforked snapshot) to prevent concurrent
// writes.
RootFSReadOnly bool

	// StartTimeout bounds the wait for Firecracker to initialize its API socket.
	StartTimeout time.Duration

	// StopTimeout bounds the graceful shutdown wait before escalation.
	StopTimeout time.Duration

	// StopGraceTimeout bounds the final wait for process termination after
	// escalation.
	StopGraceTimeout time.Duration
}

// DefaultConfig returns a Config with sane defaults. Callers must still set
// KernelImage, RootFS and WorkDir before use.
func DefaultConfig() Config {
	return Config{
		VCPU:             2,
		MemSizeMiB:       1024,
		VsockCID:         3,
		VsockPort:        1024,
		StartTimeout:     10 * time.Second,
		StopTimeout:      15 * time.Second,
		StopGraceTimeout: 5 * time.Second,
	}
}

// Validate checks that the configuration is sufficient to boot a VM. It does
// not check Firecracker itself.
func (c Config) Validate() error {
	var errs []error

	if c.KernelImage == "" {
		errs = append(errs, errors.New("kernel image path is required"))
	} else if _, err := os.Stat(c.KernelImage); err != nil {
		errs = append(errs, fmt.Errorf("kernel image not statable: %w", err))
	}

	if c.RootFS == "" {
		errs = append(errs, errors.New("root filesystem path is required"))
	} else if _, err := os.Stat(c.RootFS); err != nil {
		errs = append(errs, fmt.Errorf("root filesystem not statable: %w", err))
	}

	if c.VCPU < 1 {
		errs = append(errs, errors.New("vcpu count must be at least 1"))
	}
	if c.VCPU > 32 {
		errs = append(errs, errors.New("vcpu count must not exceed 32"))
	}
	if c.VCPU != 1 && c.VCPU%2 != 0 {
		errs = append(errs, errors.New("vcpu count must be 1 or an even number"))
	}

	if c.MemSizeMiB < 1 {
		errs = append(errs, errors.New("memory size must be at least 1 MiB"))
	}

	if c.VsockCID < 3 {
		errs = append(errs, errors.New("vsock CID must be at least 3"))
	}

	if c.VsockPort == 0 {
		errs = append(errs, errors.New("vsock port must be non-zero"))
	}

	if c.StartTimeout <= 0 {
		c.StartTimeout = 10 * time.Second
	}
	if c.StopTimeout <= 0 {
		c.StopTimeout = 15 * time.Second
	}
	if c.StopGraceTimeout <= 0 {
		c.StopGraceTimeout = 5 * time.Second
	}

	return errors.Join(errs...)
}

// vmDir returns the per-VM working directory.
func (c Config) vmDir(id string) string {
	if c.WorkDir != "" {
		return filepath.Join(c.WorkDir, id)
	}
	return filepath.Join(os.TempDir(), "vmm", id)
}

// Resources returns the filesystem paths owned by a VM instance.
type Resources struct {
	// Dir is the per-VM working directory.
	Dir string
	// APISock is the Firecracker control socket path.
	APISock string
	// VsockSock is the per-VM vsock UDS path configured for the guest.
	VsockSock string
	// Log is the Firecracker log path.
	Log string
}

// resources returns the VM-owned resource paths.
func (c Config) resources(id string) Resources {
	dir := c.vmDir(id)
	return Resources{
		Dir:       dir,
		APISock:   filepath.Join(dir, "api.socket"),
		VsockSock: filepath.Join(dir, "vsock.sock"),
		Log:       filepath.Join(dir, "firecracker.log"),
	}
}

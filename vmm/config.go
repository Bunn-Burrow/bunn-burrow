package vmm

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/Bunn-Burrow/bunn-burrow/internal/network"
	"github.com/Bunn-Burrow/bunn-burrow/internal/storage"
	"github.com/Bunn-Burrow/bunn-burrow/internal/template"
	vmimage "github.com/Bunn-Burrow/bunn-burrow/internal/vmimage"
)

// GuestAgentVsockPort is the port the bundled Guest Agent listens on.
const GuestAgentVsockPort uint32 = 8080

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
	// created. If empty, it defaults to ~/.local/share/bunn-burrow/vms.
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

	// TemplateID optionally references a registered template to use as the VM's
	// rootfs source. When set, the template's rootfs is copied to a per-VM writable
	// layer before boot and RootFS need not be set directly. When empty, direct
	// rootfs creation is used (backward compatible).
	TemplateID string

	// ImageID optionally references a registered image to use as the VM's
	// rootfs source. When set, the image's qcow2 file is used as the root
	// device and RootFS need not be set directly. When empty, direct rootfs
	// creation is used (backward compatible).
	ImageID string

	// Network optionally configures networking for the VM. When nil, the VM
	// has no network interface (vsock only).
	Network *NetworkConfig

	// NetworkManager is an optional shared network manager for TAP/IPAM/firewall.
	// When nil, the VM creates its own manager if Network is set.
	NetworkManager *network.Manager

	// VolumeManager manages persistent volumes for VM attachment. Optional;
	// when nil, volume attach/detach is unavailable.
	VolumeManager *storage.VolumeManager

	// ImageManager resolves images for VM boot. Optional; when nil, image-based
	// VM creation is unavailable.
	ImageManager *vmimage.ImageManager

	// TemplateManager manages templates for this VM. Optional; when nil,
	// template-based VM creation is unavailable.
	TemplateManager *template.TemplateManager

	// Volumes lists volumes to attach to this VM at startup. Each volume is
	// attached before guest execution and detached on VM stop/destroy.
	Volumes []storage.VolumeRef

	// StartTimeout bounds the wait for Firecracker to initialize its API socket.
	StartTimeout time.Duration

	// StopTimeout bounds the graceful shutdown wait before escalation.
	StopTimeout time.Duration

	// StopGraceTimeout bounds the final wait for process termination after
	// escalation.
	StopGraceTimeout time.Duration
}

// DefaultConfig returns a Config with sane defaults. Callers must still set
// KernelImage and RootFS before use.
func DefaultConfig() Config {
	return Config{
		VCPU:             2,
		MemSizeMiB:       1024,
		VsockCID:         3,
		VsockPort:        GuestAgentVsockPort,
		StartTimeout:     10 * time.Second,
		StopTimeout:      15 * time.Second,
		StopGraceTimeout: 5 * time.Second,
	}
}

// NetworkConfig holds VM networking settings. It is a vmm-level (exported)
// type so external modules can configure networking without importing the
// internal network package.
type NetworkConfig struct {
	// Enabled enables VM networking.
	Enabled bool

	// Bridge is the Linux bridge name VM TAPs attach to.
	Bridge string

	// CIDR is the pool from which VM IPs are statically allocated.
	CIDR string

	// Gateway is the bridge/gateway IP for the VM subnet.
	Gateway net.IP

	// DNS contains IPv4 nameservers configured inside the guest.
	DNS []net.IP

	// NATInterface is the host interface used for outbound masquerade.
	NATInterface string

	// Firewall enables scoped IPv4 NAT and forwarding rules.
	Firewall bool

	// MTU for TAP devices.
	MTU int
}

// toInternal converts a vmm NetworkConfig to an internal network.Config.
func (c *NetworkConfig) toInternal() network.Config {
	return network.Config{
		Enabled:      c.Enabled,
		Bridge:       c.Bridge,
		CIDR:         c.CIDR,
		Gateway:      c.Gateway,
		DNS:          c.DNS,
		NATInterface: c.NATInterface,
		Firewall:     c.Firewall,
		MTU:          c.MTU,
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

	if c.RootFS == "" && c.TemplateID == "" {
		errs = append(errs, errors.New("root filesystem path or template id is required"))
	} else if c.RootFS != "" {
		if _, err := os.Stat(c.RootFS); err != nil {
			errs = append(errs, fmt.Errorf("root filesystem not statable: %w", err))
		}
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

func defaultVMWorkDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home directory for VM storage: %w", err)
	}
	if home == "" {
		return "", errors.New("resolve user home directory for VM storage: home directory is empty")
	}
	return filepath.Join(home, ".local", "share", "bunn-burrow", "vms"), nil
}

// vmDir returns the per-VM working directory. WorkDir must already be resolved
// to an explicit or user-home-based VM storage directory by New.
func (c Config) vmDir(id string) string {
	return filepath.Join(c.WorkDir, id)
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

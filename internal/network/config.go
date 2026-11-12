package network

import (
	"errors"
	"fmt"
	"net"
)

// Config holds network configuration for Bunn Burrow VMs.
type Config struct {
	// Enabled enables VM networking. When false, no network interface is
	// attached to VMs and they only have vsock connectivity.
	Enabled bool

	// Bridge is the Linux bridge name VM TAPs attach to. Created if missing.
	Bridge string

	// CIDR is the pool from which VM IPs are statically allocated.
	CIDR string

	// Gateway is the bridge/gateway IP for the VM subnet. If empty, the first
	// usable IP in the CIDR is used.
	Gateway net.IP

	// NATInterface is the host interface used for outbound masquerade.
	// If empty, the default route interface is used.
	NATInterface string

	// Firewall enables nftables filtering (Layer-2 VM isolation + NAT).
	Firewall bool

	// MTU for TAP devices.
	MTU int
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		Bridge:   "br-bunn",
		CIDR:     "10.11.0.0/16",
		MTU:      1500,
		Firewall: true,
	}
}

// Validate checks the configuration is sufficient for networking.
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.Bridge == "" {
		return errors.New("bridge name is required when networking is enabled")
	}
	_, _, err := net.ParseCIDR(c.CIDR)
	if err != nil {
		return fmt.Errorf("invalid CIDR %q: %w", c.CIDR, err)
	}
	if c.MTU == 0 {
		c.MTU = 1500
	}
	return nil
}

// Network represents a VM's allocated network resources.
type Network struct {
	// VMID is the VM this network belongs to.
	VMID string

	// TapName is the host-side TAP device name (e.g. "tap-bunn-vm1").
	TapName string

	// TapDevice is the host-side TAP device path (e.g. "/dev/tap0").
	TapDevice string

	// MAC is the MAC address presented to the guest.
	MAC net.HardwareAddr

	// IP is the VM's static IP address.
	IP net.IP

	// Gateway is the subnet gateway IP.
	Gateway net.IP

	// IfaceID is the guest interface identifier (e.g. "eth0").
	IfaceID string

	// Subnet is the CIDR block the VM belongs to.
	Subnet *net.IPNet
}

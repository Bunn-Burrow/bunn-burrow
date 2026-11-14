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

	// DNS contains IPv4 nameservers passed to guests using this network.
	DNS []net.IP

	// NATInterface is the host interface used for outbound masquerade.
	// If empty, the default route interface is used.
	NATInterface string

	// Firewall enables IPv4 NAT and forwarding. Bridge port isolation is
	// configured independently for every VM TAP.
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
	_, subnet, err := net.ParseCIDR(c.CIDR)
	if err != nil {
		return fmt.Errorf("invalid CIDR %q: %w", c.CIDR, err)
	}
	if subnet.IP.To4() == nil {
		return fmt.Errorf("network CIDR %q is not IPv4", c.CIDR)
	}
	if c.MTU != 0 && (c.MTU < 576 || c.MTU > 65535) {
		return fmt.Errorf("MTU must be between 576 and 65535, got %d", c.MTU)
	}
	for _, server := range c.DNS {
		if server.To4() == nil {
			return fmt.Errorf("DNS server %q is not an IPv4 address", server)
		}
	}
	return nil
}

// Network represents a VM's allocated network resources.
type Network struct {
	// VMID is the VM this network belongs to.
	VMID string

	// TapName is the host-side TAP device name (e.g. "tap-bunn-vm1").
	TapName string

	// MAC is the MAC address presented to the guest.
	MAC net.HardwareAddr

	// IP is the VM's static IP address.
	IP net.IP

	// Gateway is the subnet gateway IP.
	Gateway net.IP

	// DNS contains the nameservers configured inside the guest.
	DNS []net.IP

	// IfaceID is the guest interface identifier (e.g. "eth0").
	IfaceID string

	// Interface is the guest-visible Linux interface name.
	Interface string

	// MTU is the guest interface MTU.
	MTU int

	// Subnet is the CIDR block the VM belongs to.
	Subnet *net.IPNet
}

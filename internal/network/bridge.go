package network

import (
	"bytes"
	"fmt"
	"net"
	"os"

	"github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
)

// BridgeManager creates and manages a Linux bridge for VM TAPs.
type BridgeManager struct {
	name      string
	logger    *logrus.Entry
	ops       bridgeLinkOps
	created   bool
	useHelper bool
}

type bridgeLinkOps interface {
	LinkByName(name string) (netlink.Link, error)
	LinkSetMaster(link netlink.Link, master netlink.Link) error
	LinkSetNoMaster(link netlink.Link) error
	LinkSetIsolated(link netlink.Link, mode bool) error
}

type netlinkBridgeLinkOps struct{}

func (netlinkBridgeLinkOps) LinkByName(name string) (netlink.Link, error) {
	return netlink.LinkByName(name)
}

func (netlinkBridgeLinkOps) LinkSetMaster(link netlink.Link, master netlink.Link) error {
	return netlink.LinkSetMaster(link, master)
}

func (netlinkBridgeLinkOps) LinkSetNoMaster(link netlink.Link) error {
	return netlink.LinkSetNoMaster(link)
}

func (netlinkBridgeLinkOps) LinkSetIsolated(link netlink.Link, mode bool) error {
	return netlink.LinkSetIsolated(link, mode)
}

// NewBridgeManager creates a BridgeManager for the named bridge.
func NewBridgeManager(name string, logger *logrus.Entry) *BridgeManager {
	if logger == nil {
		logger = logrus.NewEntry(logrus.New())
	}
	return &BridgeManager{
		name:      name,
		logger:    logger.WithField("component", "network-bridge"),
		ops:       netlinkBridgeLinkOps{},
		useHelper: true,
	}
}

// Ensure creates the bridge if it does not exist, assigns its gateway address,
// and brings it up.
func (b *BridgeManager) Ensure(ip net.IP, subnet *net.IPNet) error {
	if ip.To4() == nil || subnet == nil || subnet.IP.To4() == nil {
		return fmt.Errorf("bridge gateway and subnet must be IPv4")
	}
	if b.useHelper && os.Geteuid() != 0 {
		response, err := callNetworkHelper(helperRequest{
			Version:   helperProtocol,
			Operation: "bridge.ensure",
			Bridge:    b.name,
			Gateway:   ip.To4().String(),
			CIDR:      subnet.String(),
		})
		b.created = response.BridgeCreated
		return err
	}
	address := &netlink.Addr{IPNet: &net.IPNet{
		IP:   append(net.IP(nil), ip.To4()...),
		Mask: append(net.IPMask(nil), subnet.Mask...),
	}}

	existing, err := netlink.LinkByName(b.name)
	if err == nil {
		if _, ok := existing.(*netlink.Bridge); !ok {
			return fmt.Errorf("link %q exists but is not a bridge", b.name)
		}
		if err := b.ensureAddress(existing, address); err != nil {
			return err
		}
		if err := netlink.LinkSetUp(existing); err != nil {
			return fmt.Errorf("bring bridge %q up: %w", b.name, err)
		}
		return nil
	}
	if _, ok := err.(netlink.LinkNotFoundError); !ok {
		return fmt.Errorf("lookup bridge %q: %w", b.name, err)
	}

	br := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{Name: b.name, Alias: "bunn-burrow"},
	}
	if err := netlink.LinkAdd(br); err != nil {
		return fmt.Errorf("create bridge %q: %w", b.name, err)
	}
	b.created = true

	l, err := netlink.LinkByName(b.name)
	if err != nil {
		return fmt.Errorf("lookup bridge %q: %w", b.name, err)
	}

	if err := b.ensureAddress(l, address); err != nil {
		if cleanupErr := netlink.LinkDel(l); cleanupErr != nil {
			return fmt.Errorf("%w (remove partially created bridge: %v)", err, cleanupErr)
		}
		b.created = false
		return err
	}

	if err := netlink.LinkSetUp(l); err != nil {
		if cleanupErr := netlink.LinkDel(l); cleanupErr != nil {
			return fmt.Errorf("bring bridge %q up: %w (remove partially created bridge: %v)", b.name, err, cleanupErr)
		}
		b.created = false
		return fmt.Errorf("bring bridge %q up: %w", b.name, err)
	}

	b.logger.Info("bridge created", "bridge", b.name, "ip", ip, "subnet", subnet)
	return nil
}

func (b *BridgeManager) ensureAddress(link netlink.Link, address *netlink.Addr) error {
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("list bridge %q addresses: %w", b.name, err)
	}
	for _, current := range addrs {
		if !current.IPNet.IP.Equal(address.IPNet.IP) {
			continue
		}
		if bytes.Equal(current.IPNet.Mask, address.IPNet.Mask) {
			return nil
		}
		if err := netlink.AddrDel(link, &current); err != nil {
			return fmt.Errorf("replace bridge %q address %s: %w", b.name, address.IPNet, err)
		}
	}
	if err := netlink.AddrAdd(link, address); err != nil {
		return fmt.Errorf("assign IP %s to bridge %q: %w", address.IPNet, b.name, err)
	}
	return nil
}

// AddPort attaches a TAP link to the bridge and isolates it from other ports.
func (b *BridgeManager) AddPort(tapName string) error {
	if b.useHelper && os.Geteuid() != 0 {
		_, err := callNetworkHelper(helperRequest{
			Version:   helperProtocol,
			Operation: "bridge.add-port",
			Bridge:    b.name,
			Tap:       tapName,
		})
		return err
	}
	tap, err := b.ops.LinkByName(tapName)
	if err != nil {
		return fmt.Errorf("lookup TAP %q: %w", tapName, err)
	}
	br, err := b.ops.LinkByName(b.name)
	if err != nil {
		return fmt.Errorf("lookup bridge %q: %w", b.name, err)
	}
	if _, ok := br.(*netlink.Bridge); !ok {
		return fmt.Errorf("link %q exists but is not a bridge", b.name)
	}
	if err := b.ops.LinkSetMaster(tap, br); err != nil {
		return fmt.Errorf("attach TAP %q to bridge %q: %w", tapName, b.name, err)
	}
	if err := b.ops.LinkSetIsolated(tap, true); err != nil {
		if cleanupErr := b.ops.LinkSetNoMaster(tap); cleanupErr != nil {
			return fmt.Errorf("isolate TAP %q: %w (detach from bridge: %v)", tapName, err, cleanupErr)
		}
		return fmt.Errorf("isolate TAP %q: %w", tapName, err)
	}
	b.logger.Info("TAP attached to bridge", "tap", tapName, "bridge", b.name)
	return nil
}

// Delete removes the bridge if it exists.
func (b *BridgeManager) Delete() error {
	if b.useHelper && os.Geteuid() != 0 {
		_, err := callNetworkHelper(helperRequest{
			Version:   helperProtocol,
			Operation: "bridge.delete",
			Bridge:    b.name,
		})
		if err == nil {
			b.created = false
		}
		return err
	}
	l, err := netlink.LinkByName(b.name)
	if err != nil {
		if _, ok := err.(netlink.LinkNotFoundError); ok {
			b.created = false
			return nil
		}
		return fmt.Errorf("lookup bridge %q: %w", b.name, err)
	}
	if l.Attrs().Alias != "bunn-burrow" {
		b.created = false
		return nil
	}
	links, err := netlink.LinkList()
	if err != nil {
		return fmt.Errorf("list links before deleting bridge %q: %w", b.name, err)
	}
	for _, attached := range links {
		if attached.Attrs().MasterIndex == l.Attrs().Index {
			return nil
		}
	}
	if err := netlink.LinkDel(l); err != nil {
		return fmt.Errorf("delete bridge %q: %w", b.name, err)
	}
	b.created = false
	b.logger.Info("bridge deleted", "bridge", b.name)
	return nil
}

func (b *BridgeManager) deleteIfCreated() error {
	return b.Delete()
}

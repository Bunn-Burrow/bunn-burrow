package network

import (
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
	"github.com/sirupsen/logrus"
)

// BridgeManager creates and manages a Linux bridge for VM TAPs.
type BridgeManager struct {
	name   string
	logger *logrus.Entry
}

// NewBridgeManager creates a BridgeManager for the named bridge.
func NewBridgeManager(name string, logger *logrus.Entry) *BridgeManager {
	if logger == nil {
		logger = logrus.NewEntry(logrus.New())
	}
	return &BridgeManager{name: name, logger: logger.WithField("component", "network-bridge")}
}

// Ensure creates the bridge if it does not exist and brings it up.
func (b *BridgeManager) Ensure(ip net.IP) error {
	existing, err := netlink.LinkByName(b.name)
	if err == nil {
		// Bridge already exists; ensure it is up.
		if err := netlink.LinkSetUp(existing); err != nil {
			return fmt.Errorf("bring bridge %q up: %w", b.name, err)
		}
		return nil
	}

	br := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{Name: b.name},
	}
	if err := netlink.LinkAdd(br); err != nil {
		return fmt.Errorf("create bridge %q: %w", b.name, err)
	}

	l, err := netlink.LinkByName(b.name)
	if err != nil {
		return fmt.Errorf("lookup bridge %q: %w", b.name, err)
	}

	if ip != nil {
		addr := &netlink.Addr{IPNet: &net.IPNet{IP: ip, Mask: net.CIDRMask(24, 32)}}
		if err := netlink.AddrAdd(l, addr); err != nil {
			return fmt.Errorf("assign IP %s to bridge %q: %w", ip, b.name, err)
		}
	}

	if err := netlink.LinkSetUp(l); err != nil {
		return fmt.Errorf("bring bridge %q up: %w", b.name, err)
	}

	b.logger.Info("bridge created", "bridge", b.name, "ip", ip)
	return nil
}

// AddPort attaches a TAP link to the bridge.
func (b *BridgeManager) AddPort(tapName string) error {
	tap, err := netlink.LinkByName(tapName)
	if err != nil {
		return fmt.Errorf("lookup TAP %q: %w", tapName, err)
	}
	br, err := netlink.LinkByName(b.name)
	if err != nil {
		return fmt.Errorf("lookup bridge %q: %w", b.name, err)
	}
	if err := netlink.LinkSetMaster(tap, br.(*netlink.Bridge)); err != nil {
		return fmt.Errorf("attach TAP %q to bridge %q: %w", tapName, b.name, err)
	}
	b.logger.Info("TAP attached to bridge", "tap", tapName, "bridge", b.name)
	return nil
}

// Delete removes the bridge if it exists.
func (b *BridgeManager) Delete() error {
	l, err := netlink.LinkByName(b.name)
	if err != nil {
		if _, ok := err.(netlink.LinkNotFoundError); ok {
			return nil
		}
		return fmt.Errorf("lookup bridge %q: %w", b.name, err)
	}
	if err := netlink.LinkDel(l); err != nil {
		return fmt.Errorf("delete bridge %q: %w", b.name, err)
	}
	b.logger.Info("bridge deleted", "bridge", b.name)
	return nil
}

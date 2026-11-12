package network

import (
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
	"github.com/sirupsen/logrus"
)

// TapManager creates and removes TAP devices for VMs.
type TapManager struct {
	logger *logrus.Entry
}

// NewTapManager creates a TapManager.
func NewTapManager(logger *logrus.Entry) *TapManager {
	if logger == nil {
		logger = logrus.NewEntry(logrus.New())
	}
	return &TapManager{logger: logger.WithField("component", "network-tap")}
}

// Create creates a TAP device with the given name, MAC address, and MTU.
// It returns the device path (/dev/tapN) and the link index.
func (t *TapManager) Create(name string, mac net.HardwareAddr, mtu int) (string, int, error) {
	link := &netlink.Tuntap{
		LinkAttrs: netlink.LinkAttrs{
			Name:         name,
			HardwareAddr: mac,
			MTU:          mtu,
		},
		Mode: netlink.TUNTAP_MODE_TAP,
	}

	if err := netlink.LinkAdd(link); err != nil {
		return "", 0, fmt.Errorf("create TAP %q: %w", name, err)
	}

	l, err := netlink.LinkByName(name)
	if err != nil {
		return "", 0, fmt.Errorf("lookup TAP %q: %w", name, err)
	}

	if mac != nil {
		if err := netlink.LinkSetHardwareAddr(l, mac); err != nil {
			return "", 0, fmt.Errorf("set MAC on TAP %q: %w", name, err)
		}
	}

	if mtu > 0 {
		if err := netlink.LinkSetMTU(l, mtu); err != nil {
			return "", 0, fmt.Errorf("set MTU on TAP %q: %w", name, err)
		}
	}

	if err := netlink.LinkSetUp(l); err != nil {
		return "", 0, fmt.Errorf("bring TAP %q up: %w", name, err)
	}

	tapDevice := fmt.Sprintf("/dev/tap%d", l.Attrs().Index)
	t.logger.Info("TAP device created",
		"tap", name, "device", tapDevice, "mac", mac, "mtu", mtu)

	return tapDevice, l.Attrs().Index, nil
}

// Delete removes a TAP device by name.
func (t *TapManager) Delete(name string) error {
	l, err := netlink.LinkByName(name)
	if err != nil {
		if _, ok := err.(netlink.LinkNotFoundError); ok {
			return nil
		}
		return fmt.Errorf("lookup TAP %q: %w", name, err)
	}
	if err := netlink.LinkDel(l); err != nil {
		return fmt.Errorf("delete TAP %q: %w", name, err)
	}
	t.logger.Info("TAP device deleted", "tap", name)
	return nil
}

// MAC generates a predictable MAC address from a VM ID.
func MAC(vmID string) net.HardwareAddr {
	// Use a locally-administered, unicast MAC (first byte 0x02).
	mac := net.HardwareAddr{0x02, 0x11, 0x22, 0x33, 0x44, 0x55}
	copy(mac[2:], []byte(vmID)[:4])
	return mac
}

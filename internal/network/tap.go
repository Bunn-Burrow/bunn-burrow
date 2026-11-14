package network

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
)

// TapManager creates and removes TAP devices for VMs.
type TapManager struct {
	logger    *logrus.Entry
	useHelper bool
}

// NewTapManager creates a TapManager.
func NewTapManager(logger *logrus.Entry) *TapManager {
	if logger == nil {
		logger = logrus.NewEntry(logrus.New())
	}
	return &TapManager{logger: logger.WithField("component", "network-tap"), useHelper: true}
}

// Create creates a TAP interface with the given name, MAC address, and MTU.
func (t *TapManager) Create(name string, mac net.HardwareAddr, mtu int) error {
	return t.CreateForNetwork(name, mac, mtu, nil)
}

func (t *TapManager) CreateForNetwork(name string, mac net.HardwareAddr, mtu int, ip net.IP) error {
	pid := os.Getpid()
	startTime, err := processStartTime(pid)
	if err != nil {
		return fmt.Errorf("identify network owner process: %w", err)
	}
	address := ""
	if ip != nil {
		if ip.To4() == nil {
			return fmt.Errorf("TAP lease address %q is not IPv4", ip)
		}
		address = ip.To4().String()
	}
	if t.useHelper && os.Geteuid() != 0 {
		_, err := callNetworkHelper(helperRequest{
			Version:        helperProtocol,
			Operation:      "tap.create",
			Tap:            name,
			MAC:            mac.String(),
			MTU:            mtu,
			IP:             address,
			OwnerPID:       pid,
			OwnerStartTime: startTime,
		})
		return err
	}
	return t.create(name, mac, mtu, networkTapAlias(pid, startTime, address))
}

func (t *TapManager) create(name string, mac net.HardwareAddr, mtu int, alias string) error {
	link := &netlink.Tuntap{
		LinkAttrs: netlink.LinkAttrs{
			Name:         name,
			HardwareAddr: mac,
			MTU:          mtu,
			Alias:        alias,
		},
		Mode: netlink.TUNTAP_MODE_TAP,
	}

	if err := netlink.LinkAdd(link); err != nil {
		return fmt.Errorf("create TAP %q: %w", name, err)
	}

	l, err := netlink.LinkByName(name)
	if err != nil {
		return t.cleanupAfterCreateFailure(name, fmt.Errorf("lookup TAP %q: %w", name, err))
	}

	if mac != nil {
		if err := netlink.LinkSetHardwareAddr(l, mac); err != nil {
			return t.cleanupAfterCreateFailure(name, fmt.Errorf("set MAC on TAP %q: %w", name, err))
		}
	}

	if mtu > 0 {
		if err := netlink.LinkSetMTU(l, mtu); err != nil {
			return t.cleanupAfterCreateFailure(name, fmt.Errorf("set MTU on TAP %q: %w", name, err))
		}
	}

	if err := netlink.LinkSetUp(l); err != nil {
		return t.cleanupAfterCreateFailure(name, fmt.Errorf("bring TAP %q up: %w", name, err))
	}

	t.logger.Info("TAP device created",
		"tap", name, "index", l.Attrs().Index, "mac", mac, "mtu", mtu)

	return nil
}

func (t *TapManager) cleanupAfterCreateFailure(name string, cause error) error {
	if err := t.Delete(name); err != nil {
		return errors.Join(cause, fmt.Errorf("clean up TAP %q after creation failure: %w", name, err))
	}
	return cause
}

// Delete removes a TAP device by name.
func (t *TapManager) Delete(name string) error {
	if t.useHelper && os.Geteuid() != 0 {
		_, err := callNetworkHelper(helperRequest{
			Version:   helperProtocol,
			Operation: "tap.delete",
			Tap:       name,
		})
		return err
	}
	l, err := netlink.LinkByName(name)
	if err != nil {
		if _, ok := err.(netlink.LinkNotFoundError); ok {
			return nil
		}
		return fmt.Errorf("lookup TAP %q: %w", name, err)
	}
	if _, ok := l.(*netlink.Tuntap); !ok || !strings.HasPrefix(l.Attrs().Alias, "bunn-burrow:") {
		return fmt.Errorf("refusing to delete TAP %q not owned by Bunn Burrow", name)
	}
	if err := netlink.LinkDel(l); err != nil {
		return fmt.Errorf("delete TAP %q: %w", name, err)
	}
	t.logger.Info("TAP device deleted", "tap", name)
	return nil
}

func (t *TapManager) Reconcile() error {
	if t.useHelper && os.Geteuid() != 0 {
		_, err := callNetworkHelper(helperRequest{
			Version:   helperProtocol,
			Operation: "tap.reconcile",
		})
		return err
	}
	links, err := netlink.LinkList()
	if err != nil {
		return fmt.Errorf("list links for stale TAP recovery: %w", err)
	}
	for _, link := range links {
		if _, ok := link.(*netlink.Tuntap); !ok || !strings.HasPrefix(link.Attrs().Alias, "bunn-burrow:") {
			continue
		}
		pid, startTime, _, err := parseNetworkTapAlias(link.Attrs().Alias)
		if err != nil {
			return fmt.Errorf("inspect owned TAP %q: %w", link.Attrs().Name, err)
		}
		currentStartTime, processErr := processStartTime(pid)
		if processErr == nil && currentStartTime == startTime {
			continue
		}
		if err := netlink.LinkDel(link); err != nil {
			return fmt.Errorf("remove stale TAP %q: %w", link.Attrs().Name, err)
		}
		t.logger.WithField("tap", link.Attrs().Name).Info("stale TAP removed")
	}
	return nil
}

func networkTapAlias(pid int, startTime, ip string) string {
	return "bunn-burrow:" + strconv.Itoa(pid) + ":" + startTime + ":" + ip
}

func parseNetworkTapAlias(alias string) (int, string, string, error) {
	fields := strings.Split(alias, ":")
	if len(fields) != 4 || fields[0] != "bunn-burrow" {
		return 0, "", "", fmt.Errorf("invalid owner alias %q", alias)
	}
	pid, err := strconv.Atoi(fields[1])
	if err != nil || pid <= 0 {
		return 0, "", "", fmt.Errorf("invalid process ID in owner alias %q", alias)
	}
	if fields[2] == "" {
		return 0, "", "", fmt.Errorf("missing process start time in owner alias %q", alias)
	}
	if fields[3] != "" && net.ParseIP(fields[3]).To4() == nil {
		return 0, "", "", fmt.Errorf("invalid lease address in owner alias %q", alias)
	}
	return pid, fields[2], fields[3], nil
}

func processStartTime(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	closeParen := strings.LastIndexByte(string(data), ')')
	if closeParen < 0 || closeParen+1 >= len(data) {
		return "", fmt.Errorf("invalid process stat for PID %d", pid)
	}
	fields := strings.Fields(string(data[closeParen+1:]))
	if len(fields) <= 19 {
		return "", fmt.Errorf("incomplete process stat for PID %d", pid)
	}
	return fields[19], nil
}

// MAC generates a predictable MAC address from a VM ID.
func MAC(vmID string) net.HardwareAddr {
	// Use a locally-administered, unicast MAC (first byte 0x02).
	mac := net.HardwareAddr{0x02, 0x11, 0x22, 0x33, 0x44, 0x55}
	hash := sha256.Sum256([]byte(vmID))
	copy(mac[2:], hash[:4])
	return mac
}

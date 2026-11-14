package guestagent

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/vishvananda/netlink"
)

type guestNetworkConfig struct {
	iface   string
	mac     net.HardwareAddr
	address *net.IPNet
	gateway net.IP
	dns     []net.IP
	mtu     int
}

// ConfigureNetworkFromCmdline configures the guest interface when bunn_net is present.
func ConfigureNetworkFromCmdline(cmdlinePath string) error {
	cmdline, err := os.ReadFile(cmdlinePath)
	if err != nil {
		return fmt.Errorf("read kernel command line: %w", err)
	}

	var value string
	for _, field := range strings.Fields(string(cmdline)) {
		if !strings.HasPrefix(field, "bunn_net=") {
			continue
		}
		if value != "" {
			return fmt.Errorf("kernel command line contains multiple bunn_net options")
		}
		value = strings.TrimPrefix(field, "bunn_net=")
	}
	if value == "" {
		return nil
	}

	config, err := parseGuestNetwork(value)
	if err != nil {
		return err
	}
	if err := configureGuestNetwork(config); err != nil {
		return fmt.Errorf("configure guest network: %w", err)
	}
	return nil
}

func parseGuestNetwork(value string) (*guestNetworkConfig, error) {
	fields := strings.Split(value, "|")
	if len(fields) == 6 {
		return parseFullGuestNetwork(fields)
	}

	legacyFields := strings.Split(value, ",")
	if len(legacyFields) < 3 {
		return nil, fmt.Errorf("invalid bunn_net value: expected address, gateway, and at least one DNS server")
	}

	return parseFullGuestNetwork([]string{
		"eth0",
		"",
		legacyFields[0],
		legacyFields[1],
		strings.Join(legacyFields[2:], ";"),
		"1500",
	})
}

func parseFullGuestNetwork(fields []string) (*guestNetworkConfig, error) {
	if len(fields) != 6 {
		return nil, fmt.Errorf("invalid bunn_net value: expected interface, MAC, address, gateway, DNS, and MTU")
	}
	if !validGuestInterfaceName(fields[0]) {
		return nil, fmt.Errorf("invalid guest interface name %q", fields[0])
	}
	var mac net.HardwareAddr
	if fields[1] != "" {
		parsed, err := net.ParseMAC(fields[1])
		if err != nil || len(parsed) != 6 {
			return nil, fmt.Errorf("invalid guest MAC address %q", fields[1])
		}
		mac = parsed
	}

	address, subnet, err := net.ParseCIDR(fields[2])
	if err != nil || address.To4() == nil {
		return nil, fmt.Errorf("invalid guest IPv4 address %q", fields[2])
	}
	subnet.IP = append(net.IP(nil), address.To4()...)

	gateway := net.ParseIP(fields[3]).To4()
	if gateway == nil || !subnet.Contains(gateway) {
		return nil, fmt.Errorf("invalid guest IPv4 gateway %q for %s", fields[3], subnet)
	}

	dnsFields := strings.Split(fields[4], ";")
	if len(dnsFields) == 0 || dnsFields[0] == "" {
		return nil, fmt.Errorf("guest DNS server list is empty")
	}
	dns := make([]net.IP, 0, len(dnsFields))
	for _, server := range dnsFields {
		ip := net.ParseIP(server).To4()
		if ip == nil {
			return nil, fmt.Errorf("invalid guest IPv4 DNS server %q", server)
		}
		dns = append(dns, append(net.IP(nil), ip...))
	}
	mtu, err := strconv.Atoi(fields[5])
	if err != nil || mtu < 576 || mtu > 65535 {
		return nil, fmt.Errorf("invalid guest MTU %q", fields[5])
	}
	return &guestNetworkConfig{
		iface:   fields[0],
		mac:     mac,
		address: subnet,
		gateway: append(net.IP(nil), gateway...),
		dns:     dns,
		mtu:     mtu,
	}, nil
}

func configureGuestNetwork(config *guestNetworkConfig) error {
	link, err := netlink.LinkByName(config.iface)
	if err != nil {
		return fmt.Errorf("find %s: %w", config.iface, err)
	}
	if len(config.mac) > 0 && !bytes.Equal(link.Attrs().HardwareAddr, config.mac) {
		if err := netlink.LinkSetHardwareAddr(link, config.mac); err != nil {
			return fmt.Errorf("set MAC on %s: %w", config.iface, err)
		}
	}
	if err := netlink.LinkSetMTU(link, config.mtu); err != nil {
		return fmt.Errorf("set MTU on %s: %w", config.iface, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bring %s up: %w", config.iface, err)
	}
	if err := netlink.AddrReplace(link, &netlink.Addr{IPNet: config.address}); err != nil {
		return fmt.Errorf("assign %s to %s: %w", config.address, config.iface, err)
	}
	if err := netlink.RouteReplace(&netlink.Route{
		LinkIndex: link.Attrs().Index,
		Gw:        config.gateway,
	}); err != nil {
		return fmt.Errorf("set default route via %s: %w", config.gateway, err)
	}
	if err := writeResolverConfig(config.dns); err != nil {
		return fmt.Errorf("write DNS configuration: %w", err)
	}
	return nil
}

func writeResolverConfig(servers []net.IP) error {
	return writeResolverConfigAt("/etc/resolv.conf", servers)
}

func writeResolverConfigAt(resolverPath string, servers []net.IP) error {
	var content strings.Builder
	for _, server := range servers {
		if server.To4() == nil {
			return fmt.Errorf("DNS server %q is not IPv4", server)
		}
		fmt.Fprintf(&content, "nameserver %s\n", server)
	}
	if content.Len() == 0 {
		return errors.New("no DNS servers configured")
	}

	existing, err := os.ReadFile(resolverPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read existing resolver file: %w", err)
	}
	var desired strings.Builder
	if err == nil {
		for _, line := range strings.Split(string(existing), "\n") {
			fields := strings.Fields(line)
			if len(fields) > 0 && fields[0] == "nameserver" {
				continue
			}
			if line != "" {
				desired.WriteString(line)
				desired.WriteByte('\n')
			}
		}
	}
	desired.WriteString(content.String())
	if err == nil && string(existing) == desired.String() {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(resolverPath), 0o755); err != nil {
		return fmt.Errorf("create resolver directory: %w", err)
	}

	temp, err := os.CreateTemp(filepath.Dir(resolverPath), ".resolv.conf-*")
	if err != nil {
		return fmt.Errorf("create resolver temp file: %w", err)
	}
	tempPath := temp.Name()
	if _, err := temp.WriteString(desired.String()); err != nil {
		return errors.Join(fmt.Errorf("write resolver temp file: %w", err), temp.Close(), os.Remove(tempPath))
	}
	if err := temp.Chmod(0o644); err != nil {
		return errors.Join(fmt.Errorf("set resolver file permissions: %w", err), temp.Close(), os.Remove(tempPath))
	}
	if err := temp.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync resolver temp file: %w", err), temp.Close(), os.Remove(tempPath))
	}
	if err := temp.Close(); err != nil {
		return errors.Join(fmt.Errorf("close resolver temp file: %w", err), os.Remove(tempPath))
	}
	if err := os.Rename(tempPath, resolverPath); err != nil {
		return errors.Join(fmt.Errorf("replace resolver file: %w", err), os.Remove(tempPath))
	}
	return nil
}

func validGuestInterfaceName(name string) bool {
	if name == "" || len(name) > 15 {
		return false
	}
	for _, char := range name {
		if !(char >= 'a' && char <= 'z') &&
			!(char >= 'A' && char <= 'Z') &&
			!(char >= '0' && char <= '9') &&
			char != '_' && char != '-' && char != '.' {
			return false
		}
	}
	return true
}

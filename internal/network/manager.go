package network

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/sirupsen/logrus"
)

// Manager manages VM networking: IPAM, TAP devices, bridge, firewall, NAT.
type Manager struct {
	mu       sync.Mutex
	cfg      Config
	ipam     *IPAM
	gateway  net.IP
	tapMgr   tapOperations
	bridge   bridgeOperations
	firewall firewallOperations
	logger   *logrus.Entry

	networks map[string]*Network // vmID -> Network
	ready    bool
}

type tapOperations interface {
	Create(name string, mac net.HardwareAddr, mtu int) error
	CreateForNetwork(name string, mac net.HardwareAddr, mtu int, ip net.IP) error
	Delete(name string) error
	Reconcile() error
}

type bridgeOperations interface {
	Ensure(ip net.IP, subnet *net.IPNet) error
	AddPort(tapName string) error
	Delete() error
	deleteIfCreated() error
}

type firewallOperations interface {
	Install() error
	Remove() error
}

// NewManager creates a network Manager from the given config.
func NewManager(cfg Config, logger *logrus.Entry) (*Manager, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate network config: %w", err)
	}
	if logger == nil {
		logger = logrus.NewEntry(logrus.New())
	}
	logger = logger.WithField("component", "network")
	if cfg.MTU == 0 {
		cfg.MTU = 1500
	}
	if cfg.Enabled && len(cfg.DNS) == 0 {
		var err error
		cfg.DNS, err = hostDNSServers()
		if err != nil {
			return nil, fmt.Errorf("resolve guest DNS servers: %w", err)
		}
	}
	for i, server := range cfg.DNS {
		if server.To4() == nil {
			return nil, fmt.Errorf("DNS server %q is not an IPv4 address", server)
		}
		cfg.DNS[i] = append(net.IP(nil), server.To4()...)
	}

	m := &Manager{
		cfg:      cfg,
		logger:   logger,
		networks: make(map[string]*Network),
	}

	if cfg.Enabled {
		ipam, err := NewIPAM(cfg.CIDR)
		if err != nil {
			return nil, fmt.Errorf("create IPAM: %w", err)
		}
		gateway, err := networkGateway(cfg.Gateway, ipam.Network())
		if err != nil {
			return nil, err
		}
		if err := ipam.Reserve(gateway.String()); err != nil {
			return nil, fmt.Errorf("reserve gateway address: %w", err)
		}
		m.ipam = ipam
		m.gateway = gateway
		m.tapMgr = NewTapManager(logger)
		m.bridge = NewBridgeManager(cfg.Bridge, logger)
		firewall := NewFirewall(cfg.Bridge, ipam.Network().String(), cfg.NATInterface, logger)
		firewall.SetDNSServers(cfg.DNS)
		m.firewall = firewall
	}

	return m, nil
}

// Enable returns whether networking is enabled.
func (m *Manager) Enable() bool { return m.cfg.Enabled }

// Initialize sets up shared networking resources (bridge, firewall, NAT).
// Call once before managing VM networks.
func (m *Manager) Initialize() error {
	if !m.cfg.Enabled {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	return m.initializeLocked()
}

func (m *Manager) initializeLocked() error {
	if m.ready {
		return nil
	}

	if err := m.tapMgr.Reconcile(); err != nil {
		return fmt.Errorf("recover stale TAPs: %w", err)
	}
	if err := m.bridge.Ensure(m.gateway, m.ipam.Network()); err != nil {
		if cleanupErr := m.bridge.deleteIfCreated(); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("remove partially initialized bridge: %w", cleanupErr))
		}
		return fmt.Errorf("ensure bridge: %w", err)
	}

	if m.cfg.Firewall {
		if err := m.firewall.Install(); err != nil {
			if cleanupErr := m.firewall.Remove(); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("remove firewall after installation failure: %w", cleanupErr))
			}
			if cleanupErr := m.bridge.deleteIfCreated(); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("remove bridge after firewall installation failure: %w", cleanupErr))
			}
			return fmt.Errorf("install firewall: %w", err)
		}
	}

	m.ready = true
	m.logger.Info("network manager initialized",
		"bridge", m.cfg.Bridge, "cidr", m.cfg.CIDR, "gateway", m.gateway)
	return nil
}

// Shutdown releases shared networking resources.
func (m *Manager) Shutdown() error {
	if !m.cfg.Enabled {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.ready {
		return nil
	}
	if len(m.networks) > 0 {
		return fmt.Errorf("cannot shut down network manager while %d VM networks are allocated", len(m.networks))
	}

	var errs []error
	if m.cfg.Firewall {
		if err := m.firewall.Remove(); err != nil {
			errs = append(errs, err)
		}
	}
	if err := m.bridge.deleteIfCreated(); err != nil {
		errs = append(errs, err)
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	m.ready = false
	m.logger.Info("network manager shut down")
	return nil
}

// Allocate creates network resources for a VM: allocates IP, creates TAP,
// attaches it to the bridge, and enables bridge port isolation.
func (m *Manager) Allocate(vmID string) (*Network, error) {
	if !m.cfg.Enabled {
		return nil, nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.networks[vmID]; exists {
		return nil, fmt.Errorf("network already allocated for VM %q", vmID)
	}

	if !m.ready {
		if err := m.initializeLocked(); err != nil {
			return nil, fmt.Errorf("initialize network: %w", err)
		}
	}

	ipStr, err := m.ipam.Allocate()
	if err != nil {
		return nil, fmt.Errorf("allocate IP for VM %q: %w", vmID, err)
	}
	ip := net.ParseIP(ipStr)

	var ipNet *net.IPNet
	tapName := tapNameForVM(vmID)
	mac := MAC(vmID)
	if err := m.tapMgr.CreateForNetwork(tapName, mac, m.cfg.MTU, ip); err != nil {
		m.ipam.Release(ipStr)
		return nil, fmt.Errorf("create TAP for VM %q: %w", vmID, err)
	}

	if err := m.bridge.AddPort(tapName); err != nil {
		cleanupErr := m.tapMgr.Delete(tapName)
		m.ipam.Release(ipStr)
		return nil, fmt.Errorf("attach TAP to bridge for VM %q: %w", vmID, errors.Join(err, cleanupErr))
	}
	ipNet = m.ipam.Network()

	n := &Network{
		VMID:      vmID,
		TapName:   tapName,
		MAC:       mac,
		IP:        ip,
		Gateway:   append(net.IP(nil), m.gateway...),
		DNS:       cloneIPs(m.cfg.DNS),
		IfaceID:   "eth0",
		Interface: "eth0",
		MTU:       m.cfg.MTU,
		Subnet:    ipNet,
	}
	m.networks[vmID] = n

	m.logger.Info("network allocated for VM",
		"vm_id", vmID, "tap", tapName, "ip", ip, "gateway", m.gateway, "mac", mac)
	return n, nil
}

func cloneIPs(addresses []net.IP) []net.IP {
	cloned := make([]net.IP, len(addresses))
	for i, address := range addresses {
		cloned[i] = append(net.IP(nil), address...)
	}
	return cloned
}

func hostDNSServers() ([]net.IP, error) {
	var errs []error
	for _, path := range []string{"/etc/resolv.conf", "/run/systemd/resolve/resolv.conf"} {
		file, err := os.Open(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("open %s: %w", path, err))
			continue
		}
		servers, parseErr := parseResolverConfig(file)
		closeErr := file.Close()
		if err := errors.Join(parseErr, closeErr); err != nil {
			errs = append(errs, fmt.Errorf("read %s: %w", path, err))
			continue
		}
		if len(servers) > 0 {
			return servers, nil
		}
		errs = append(errs, fmt.Errorf("%s contains no usable IPv4 nameservers", path))
	}
	return nil, errors.Join(errs...)
}

func parseResolverConfig(reader io.Reader) ([]net.IP, error) {
	var servers []net.IP
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || fields[0] != "nameserver" {
			continue
		}
		ip := net.ParseIP(fields[1]).To4()
		if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
			continue
		}
		servers = append(servers, append(net.IP(nil), ip...))
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(servers) == 0 {
		return nil, fmt.Errorf("no usable IPv4 nameservers found")
	}
	return servers, nil
}

func networkGateway(configured net.IP, subnet *net.IPNet) (net.IP, error) {
	var gateway net.IP
	if configured != nil {
		gateway = configured.To4()
		if gateway == nil {
			return nil, fmt.Errorf("gateway %q is not an IPv4 address", configured)
		}
		gateway = append(net.IP(nil), gateway...)
	} else {
		gateway = append(net.IP(nil), subnet.IP.To4()...)
		gateway[len(gateway)-1]++
	}
	if !subnet.Contains(gateway) {
		return nil, fmt.Errorf("gateway %s is outside network %s", gateway, subnet)
	}
	return gateway, nil
}

// Release frees network resources for a VM.
func (m *Manager) Release(vmID string) error {
	if !m.cfg.Enabled {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	n, exists := m.networks[vmID]
	if !exists {
		return nil
	}

	var errs []error
	if err := m.tapMgr.Delete(n.TapName); err != nil {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("release VM %q network: %w", vmID, err)
	}

	delete(m.networks, vmID)
	m.ipam.Release(n.IP.String())

	m.logger.Info("network released for VM",
		"vm_id", vmID, "tap", n.TapName, "ip", n.IP)
	return nil
}

// Get returns the network for a VM, or nil if none allocated.
func (m *Manager) Get(vmID string) *Network {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.networks[vmID]
}

// bridgeName returns the configured bridge name.
func (c Config) bridgeName() string {
	return c.Bridge
}

// tapNameForVM derives a TAP device name from a VM ID.
func tapNameForVM(vmID string) string {
	name := "tap-" + vmID
	if validInterfaceCharacters(vmID) {
		if len(name) <= 15 {
			return name
		}
		hash := sha256.Sum256([]byte(vmID))
		return "tap-" + vmID[:5] + hex.EncodeToString(hash[:3])
	}
	hash := sha256.Sum256([]byte(vmID))
	return "tap-" + hex.EncodeToString(hash[:5])
}

func validInterfaceCharacters(name string) bool {
	if name == "" {
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

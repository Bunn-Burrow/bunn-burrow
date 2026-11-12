package network

import (
	"fmt"
	"net"
	"sync"

	"github.com/sirupsen/logrus"
)

// Manager manages VM networking: IPAM, TAP devices, bridge, firewall, NAT.
type Manager struct {
	mu       sync.Mutex
	cfg      Config
	ipam     *IPAM
	tapMgr   *TapManager
	bridge   *BridgeManager
	firewall *Firewall
	logger   *logrus.Entry

	networks map[string]*Network // vmID -> Network
	ready    bool
}

// NewManager creates a network Manager from the given config.
func NewManager(cfg Config, logger *logrus.Entry) (*Manager, error) {
	if logger == nil {
		logger = logrus.NewEntry(logrus.New())
	}
	logger = logger.WithField("component", "network")

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
		m.ipam = ipam
		m.tapMgr = NewTapManager(logger)
		m.bridge = NewBridgeManager(cfg.Bridge, logger)
		m.firewall = NewFirewall(cfg.Bridge, cfg.CIDR, cfg.NATInterface, logger)
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

	if m.ready {
		return nil
	}

	gw := m.cfg.Gateway
	if gw == nil {
		// Use the first usable IP in the CIDR as the gateway.
		ipNet := m.ipam.Network()
		ip := make(net.IP, len(ipNet.IP))
		copy(ip, ipNet.IP)
		ip[len(ip)-1]++
		gw = ip
	}

	if err := m.bridge.Ensure(gw); err != nil {
		return fmt.Errorf("ensure bridge: %w", err)
	}

	if m.cfg.Firewall {
		if err := m.firewall.Install(); err != nil {
			return fmt.Errorf("install firewall: %w", err)
		}
	}

	m.ready = true
	m.logger.Info("network manager initialized",
		"bridge", m.cfg.Bridge, "cidr", m.cfg.CIDR, "gateway", gw)
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

	var errs []error
	if m.cfg.Firewall {
		if err := m.firewall.Remove(); err != nil {
			errs = append(errs, err)
		}
	}
	_ = m.bridge.Delete()

	m.ready = false
	if len(errs) > 0 {
		return fmt.Errorf("shutdown: %v", errs)
	}
	m.logger.Info("network manager shut down")
	return nil
}

// Allocate creates network resources for a VM: allocates IP, creates TAP,
// attaches to bridge, and installs per-VM firewall state.
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
		if err := m.Initialize(); err != nil {
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
	tapDevice, _, err := m.tapMgr.Create(tapName, mac, m.cfg.MTU)
	if err != nil {
		m.ipam.Release(ipStr)
		return nil, fmt.Errorf("create TAP for VM %q: %w", vmID, err)
	}

	if err := m.bridge.AddPort(tapName); err != nil {
		_ = m.tapMgr.Delete(tapName)
		m.ipam.Release(ipStr)
		return nil, fmt.Errorf("attach TAP to bridge for VM %q: %w", vmID, err)
	}

	gw := m.cfg.Gateway
	if gw == nil {
		ipNet = m.ipam.Network()
		gw = make(net.IP, len(ipNet.IP))
		copy(gw, ipNet.IP)
		gw[len(gw)-1]++
	}

	if ipNet == nil {
		ipNet = m.ipam.Network()
	}

	n := &Network{
		VMID:      vmID,
		TapName:   tapName,
		TapDevice: tapDevice,
		MAC:       mac,
		IP:        ip,
		Gateway:   gw,
		IfaceID:   "eth0",
		Subnet:    ipNet,
	}
	m.networks[vmID] = n

	m.logger.Info("network allocated for VM",
		"vm_id", vmID, "tap", tapName, "ip", ip, "gateway", gw, "mac", mac)
	return n, nil
}

// Release frees network resources for a VM.
func (m *Manager) Release(vmID string) error {
	if !m.cfg.Enabled {
		return nil
	}

	m.mu.Lock()
	n, exists := m.networks[vmID]
	if !exists {
		m.mu.Unlock()
		return nil
	}
	delete(m.networks, vmID)
	m.mu.Unlock()

	var errs []error
	if err := m.tapMgr.Delete(n.TapName); err != nil {
		errs = append(errs, err)
	}
	m.ipam.Release(n.IP.String())

	m.logger.Info("network released for VM",
		"vm_id", vmID, "tap", n.TapName, "ip", n.IP)
	if len(errs) > 0 {
		return fmt.Errorf("release VM %q network: %v", vmID, errs)
	}
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
	if len(vmID) > 11 {
		return "tap-" + vmID[:11]
	}
	return "tap-" + vmID
}

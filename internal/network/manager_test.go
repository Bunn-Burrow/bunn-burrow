package network

import (
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

type fakeTapOperations struct {
	created []string
	deleted []string
}

func (o *fakeTapOperations) Create(name string, _ net.HardwareAddr, _ int) error {
	o.created = append(o.created, name)
	return nil
}
func (o *fakeTapOperations) CreateForNetwork(name string, mac net.HardwareAddr, mtu int, _ net.IP) error {
	return o.Create(name, mac, mtu)
}

func (o *fakeTapOperations) Delete(name string) error {
	o.deleted = append(o.deleted, name)
	return nil
}
func (o *fakeTapOperations) Reconcile() error { return nil }

type fakeBridgeOperations struct {
	addedPorts  []string
	ensureErr   error
	addPortErr  error
	deleteCalls int
}

func (o *fakeBridgeOperations) Ensure(net.IP, *net.IPNet) error { return o.ensureErr }
func (o *fakeBridgeOperations) AddPort(name string) error {
	o.addedPorts = append(o.addedPorts, name)
	return o.addPortErr
}
func (o *fakeBridgeOperations) Delete() error {
	o.deleteCalls++
	return nil
}
func (o *fakeBridgeOperations) deleteIfCreated() error {
	o.deleteCalls++
	return nil
}

type fakeFirewallOperations struct {
	installErr  error
	removeErr   error
	removeCalls int
}

func (o *fakeFirewallOperations) Install() error { return o.installErr }
func (o *fakeFirewallOperations) Remove() error {
	o.removeCalls++
	return o.removeErr
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	require.True(t, cfg.Firewall)
	require.Equal(t, "br-bunn", cfg.Bridge)
	require.Equal(t, "10.11.0.0/16", cfg.CIDR)
	require.Equal(t, 1500, cfg.MTU)
}

func TestManagerAllocateAndRelease(t *testing.T) {
	m, err := NewManager(Config{
		Enabled: true,
		Bridge:  "br-test",
		CIDR:    "10.11.0.0/24",
		DNS:     []net.IP{net.ParseIP("1.1.1.1")},
	}, logrus.NewEntry(logrus.New()))
	require.NoError(t, err)

	taps := &fakeTapOperations{}
	bridges := &fakeBridgeOperations{}
	m.tapMgr = taps
	m.bridge = bridges
	m.ready = true

	network, err := m.Allocate("vm-1")
	require.NoError(t, err)
	require.Equal(t, "10.11.0.2", network.IP.String())
	require.Same(t, network, m.Get("vm-1"))
	require.Equal(t, []string{network.TapName}, taps.created)
	require.Equal(t, []string{network.TapName}, bridges.addedPorts)

	second, err := m.Allocate("vm-2")
	require.NoError(t, err)
	require.Equal(t, "10.11.0.3", second.IP.String())

	require.NoError(t, m.Release("vm-1"))
	require.Nil(t, m.Get("vm-1"))
	require.Equal(t, []string{network.TapName}, taps.deleted)

	reused, err := m.Allocate("vm-3")
	require.NoError(t, err)
	require.Equal(t, network.IP, reused.IP)
	require.NotEqual(t, second.IP, reused.IP)
}

func TestManagerAllocateCleansUpAfterBridgePortFailure(t *testing.T) {
	m, err := NewManager(Config{
		Enabled: true,
		Bridge:  "br-test",
		CIDR:    "10.11.0.0/24",
		DNS:     []net.IP{net.ParseIP("1.1.1.1")},
	}, logrus.NewEntry(logrus.New()))
	require.NoError(t, err)

	taps := &fakeTapOperations{}
	bridges := &fakeBridgeOperations{addPortErr: errors.New("port isolation failed")}
	m.tapMgr = taps
	m.bridge = bridges
	m.ready = true

	_, err = m.Allocate("vm-1")
	require.ErrorContains(t, err, "port isolation failed")
	require.Equal(t, []string{"tap-vm-1"}, taps.created)
	require.Equal(t, []string{"tap-vm-1"}, taps.deleted)
	require.Empty(t, m.networks)

	ip, err := m.ipam.Allocate()
	require.NoError(t, err)
	require.Equal(t, "10.11.0.2", ip)
}

func TestManagerCleansCreatedBridgeAfterFirewallInstallFailure(t *testing.T) {
	m, err := NewManager(Config{
		Enabled:  true,
		Bridge:   "br-test",
		CIDR:     "10.11.0.0/24",
		DNS:      []net.IP{net.ParseIP("1.1.1.1")},
		Firewall: true,
	}, logrus.NewEntry(logrus.New()))
	require.NoError(t, err)

	bridges := &fakeBridgeOperations{}
	firewallErr := errors.New("NAT setup failed")
	firewall := &fakeFirewallOperations{installErr: firewallErr}
	m.tapMgr = &fakeTapOperations{}
	m.bridge = bridges
	m.firewall = firewall

	err = m.Initialize()
	require.ErrorIs(t, err, firewallErr)
	require.Equal(t, 1, bridges.deleteCalls)
	require.Equal(t, 1, firewall.removeCalls)
	require.False(t, m.ready)
}

func TestManagerShutdownRequiresAllNetworksReleased(t *testing.T) {
	m, err := NewManager(Config{
		Enabled:  true,
		Bridge:   "br-test",
		CIDR:     "10.11.0.0/24",
		DNS:      []net.IP{net.ParseIP("1.1.1.1")},
		Firewall: true,
	}, logrus.NewEntry(logrus.New()))
	require.NoError(t, err)

	taps := &fakeTapOperations{}
	bridges := &fakeBridgeOperations{}
	firewall := &fakeFirewallOperations{}
	m.tapMgr = taps
	m.bridge = bridges
	m.firewall = firewall
	m.ready = true
	_, err = m.Allocate("vm-1")
	require.NoError(t, err)

	require.ErrorContains(t, m.Shutdown(), "1 VM networks are allocated")
	require.Zero(t, firewall.removeCalls)
	require.Zero(t, bridges.deleteCalls)

	require.NoError(t, m.Release("vm-1"))
	removeErr := errors.New("firewall cleanup failed")
	firewall.removeErr = removeErr
	require.ErrorIs(t, m.Shutdown(), removeErr)
	require.True(t, m.ready)

	firewall.removeErr = nil
	require.NoError(t, m.Shutdown())
	require.Equal(t, 2, firewall.removeCalls)
	require.Equal(t, 2, bridges.deleteCalls)
}

func TestConfigValidate(t *testing.T) {
	t.Run("disabled is valid", func(t *testing.T) {
		cfg := Config{Enabled: false}
		require.NoError(t, cfg.Validate())
	})

	t.Run("empty bridge when enabled", func(t *testing.T) {
		cfg := Config{Enabled: true, Bridge: "", CIDR: "10.11.0.0/16"}
		require.Error(t, cfg.Validate())
	})

	t.Run("invalid CIDR when enabled", func(t *testing.T) {
		cfg := Config{Enabled: true, Bridge: "br0", CIDR: "not-a-cidr"}
		require.Error(t, cfg.Validate())
	})

	t.Run("valid enabled config", func(t *testing.T) {
		cfg := Config{Enabled: true, Bridge: "br0", CIDR: "10.11.0.0/16"}
		require.NoError(t, cfg.Validate())
	})
}

func TestIPAMAllocateRelease(t *testing.T) {
	ipam, err := NewIPAM("10.11.0.0/24")
	require.NoError(t, err)

	ip1, err := ipam.Allocate()
	require.NoError(t, err)
	if ip1 != "10.11.0.1" {
		t.Fatalf("expected ip1=10.11.0.1, got %q", ip1)
	}

	ip2, err := ipam.Allocate()
	require.NoError(t, err)
	if ip2 != "10.11.0.2" {
		t.Fatalf("expected ip2=10.11.0.2, got %q", ip2)
	}
	if ip1 == ip2 {
		t.Fatalf("ip1 and ip2 should differ")
	}

	// Release ip1 and verify it can be re-allocated.
	ipam.Release(ip1)
	ip3, err := ipam.Allocate()
	require.NoError(t, err)
	if ip3 != "10.11.0.1" {
		t.Fatalf("expected ip3=10.11.0.1, got %q", ip3)
	}
}

func TestIPAMExhaustion(t *testing.T) {
	// /32 has only the network address itself, no usable host addresses.
	ipam, err := NewIPAM("10.11.0.1/32")
	require.NoError(t, err)

	_, err = ipam.Allocate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "exhausted")
}

func TestIPAMReservationSkipsGatewayAndBroadcast(t *testing.T) {
	ipam, err := NewIPAM("10.11.0.0/30")
	require.NoError(t, err)
	require.NoError(t, ipam.Reserve("10.11.0.1"))

	ip, err := ipam.Allocate()
	require.NoError(t, err)
	require.Equal(t, "10.11.0.2", ip)
	_, err = ipam.Allocate()
	require.ErrorContains(t, err, "exhausted")
	require.Error(t, ipam.Reserve("10.11.0.3"))
	require.Error(t, ipam.Reserve("10.12.0.1"))
}

func TestIPAMRejectsIPv6(t *testing.T) {
	_, err := NewIPAM("2001:db8::/64")
	require.ErrorContains(t, err, "not an IPv4 network")
}

func TestManagerReservesConfiguredGateway(t *testing.T) {
	cfg := Config{
		Enabled: true,
		Bridge:  "br-test",
		CIDR:    "10.11.0.0/24",
		Gateway: net.ParseIP("10.11.0.1"),
		DNS:     []net.IP{net.ParseIP("1.1.1.1")},
	}
	m, err := NewManager(cfg, logrus.NewEntry(logrus.New()))
	require.NoError(t, err)
	require.Equal(t, "10.11.0.1", m.gateway.String())

	ip, err := m.ipam.Allocate()
	require.NoError(t, err)
	require.Equal(t, "10.11.0.2", ip)
}

func TestManagerRejectsGatewayOutsideCIDR(t *testing.T) {
	cfg := Config{
		Enabled: true,
		Bridge:  "br-test",
		CIDR:    "10.11.0.0/24",
		Gateway: net.ParseIP("10.12.0.1"),
		DNS:     []net.IP{net.ParseIP("1.1.1.1")},
	}
	_, err := NewManager(cfg, logrus.NewEntry(logrus.New()))
	require.ErrorContains(t, err, "outside network")
}

func TestParseResolverConfig(t *testing.T) {
	servers, err := parseResolverConfig(strings.NewReader(
		"# local stub\nnameserver 127.0.0.53\nnameserver 2001:db8::1\nnameserver 10.255.255.254\n",
	))
	require.NoError(t, err)
	require.Equal(t, []net.IP{net.ParseIP("10.255.255.254").To4()}, servers)
}

func TestMAC(t *testing.T) {
	mac := MAC("vm-123")
	require.NotNil(t, mac)
	// Locally-administered bit set (first byte 0x02).
	require.True(t, mac[0] == 0x02, "mac[0] = 0x%02x", mac[0])
	// Unicast (least significant bit of first byte is 0).
	require.Zero(t, mac[0]&0x01)
	// VM ID influences the MAC.
	mac2 := MAC("vm-456")
	require.NotEqual(t, mac, mac2)
	require.Len(t, MAC("a"), 6)
}

func TestNetworkConfig(t *testing.T) {
	_, ipNet, _ := net.ParseCIDR("10.11.0.0/16")
	n := &Network{
		VMID:    "vm-1",
		TapName: "tap-vm-1",
		MAC:     net.HardwareAddr{0x02, 0x11, 0x22, 0x33, 0x44, 0x55},
		IP:      net.ParseIP("10.11.0.2"),
		Gateway: net.ParseIP("10.11.0.1"),
		IfaceID: "eth0",
		Subnet:  ipNet,
	}
	require.Equal(t, "vm-1", n.VMID)
	require.Equal(t, "tap-vm-1", n.TapName)
	require.Equal(t, "10.11.0.2", n.IP.String())
	require.Equal(t, "10.11.0.1", n.Gateway.String())
	require.Equal(t, "eth0", n.IfaceID)
	require.Equal(t, ipNet.String(), n.Subnet.String())
}

func TestNewManagerDisabled(t *testing.T) {
	cfg := Config{Enabled: false}
	m, err := NewManager(cfg, logrus.NewEntry(logrus.New()))
	require.NoError(t, err)
	require.False(t, m.Enable())
	require.Nil(t, m.ipam)
	require.Nil(t, m.tapMgr)
	require.Nil(t, m.bridge)
	require.Nil(t, m.firewall)
}

func TestTapNameForVM(t *testing.T) {
	name := tapNameForVM("short")
	require.Equal(t, "tap-short", name)

	longID := "a-very-long-vm-id-that-exceeds-limit"
	name = tapNameForVM(longID)
	require.True(t, strings.HasPrefix(name, "tap-a-ver"))
	require.Len(t, name, len("tap-")+11)
	require.NotEqual(t, name, tapNameForVM("a-very-long-vm-id-that-exceeds-limits"))
	require.True(t, validInterfaceName(name))
}

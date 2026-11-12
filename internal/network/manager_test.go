package network

import (
	"net"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	require.True(t, cfg.Firewall)
	require.Equal(t, "br-bunn", cfg.Bridge)
	require.Equal(t, "10.11.0.0/16", cfg.CIDR)
	require.Equal(t, 1500, cfg.MTU)
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
}

func TestNetworkConfig(t *testing.T) {
	_, ipNet, _ := net.ParseCIDR("10.11.0.0/16")
	n := &Network{
		VMID:      "vm-1",
		TapName:   "tap-vm-1",
		TapDevice: "/dev/tap0",
		MAC:       net.HardwareAddr{0x02, 0x11, 0x22, 0x33, 0x44, 0x55},
		IP:        net.ParseIP("10.11.0.2"),
		Gateway:   net.ParseIP("10.11.0.1"),
		IfaceID:   "eth0",
		Subnet:    ipNet,
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
	require.Equal(t, "tap-a-very-long", name)
	require.Len(t, name, len("tap-")+11)
}

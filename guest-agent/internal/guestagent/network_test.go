package guestagent

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseGuestNetwork(t *testing.T) {
	config, err := parseGuestNetwork("10.11.0.2/16,10.11.0.1,10.255.255.254,1.1.1.1")
	require.NoError(t, err)
	require.Equal(t, "eth0", config.iface)
	require.Equal(t, "10.11.0.2/16", config.address.String())
	require.Equal(t, net.ParseIP("10.11.0.1").To4(), config.gateway)
	require.Equal(t, 1500, config.mtu)
	require.Equal(t, []net.IP{
		net.ParseIP("10.255.255.254").To4(),
		net.ParseIP("1.1.1.1").To4(),
	}, config.dns)
}

func TestParseFullGuestNetwork(t *testing.T) {
	config, err := parseGuestNetwork("eth1|02:11:22:33:44:55|10.11.0.2/16|10.11.0.1|1.1.1.1;8.8.8.8|1400")
	require.NoError(t, err)
	require.Equal(t, "eth1", config.iface)
	require.Equal(t, net.HardwareAddr{0x02, 0x11, 0x22, 0x33, 0x44, 0x55}, config.mac)
	require.Equal(t, "10.11.0.2/16", config.address.String())
	require.Equal(t, 1400, config.mtu)
	require.Equal(t, []net.IP{net.ParseIP("1.1.1.1").To4(), net.ParseIP("8.8.8.8").To4()}, config.dns)
}

func TestParseGuestNetworkRejectsInvalidConfiguration(t *testing.T) {
	for _, value := range []string{
		"10.11.0.2/16",
		"2001:db8::2/64,2001:db8::1,2001:db8::53",
		"10.11.0.2/16,10.12.0.1,1.1.1.1",
		"10.11.0.2/16,10.11.0.1,invalid",
		"bad iface||10.11.0.2/16|10.11.0.1|1.1.1.1|1500",
		"eth0|invalid|10.11.0.2/16|10.11.0.1|1.1.1.1|1500",
		"eth0||10.11.0.2/16|10.11.0.1|1.1.1.1|0",
		"eth0||10.11.0.2/16|10.12.0.1|1.1.1.1|1500",
	} {
		t.Run(value, func(t *testing.T) {
			_, err := parseGuestNetwork(value)
			require.Error(t, err)
		})
	}
}

func TestWriteResolverConfigPreservesUnrelatedDirectivesAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resolv.conf")
	original := "# managed by base image\nsearch example.test\noptions ndots:2\nnameserver 127.0.0.53\n"
	require.NoError(t, os.WriteFile(path, []byte(original), 0o600))
	servers := []net.IP{net.ParseIP("1.1.1.1").To4(), net.ParseIP("8.8.8.8").To4()}

	require.NoError(t, writeResolverConfigAt(path, servers))
	expected := "# managed by base image\nsearch example.test\noptions ndots:2\nnameserver 1.1.1.1\nnameserver 8.8.8.8\n"
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, expected, string(data))
	require.NoError(t, writeResolverConfigAt(path, servers))
	data, err = os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, expected, string(data))
}

func TestConfigureNetworkFromCmdlineWithoutConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cmdline")
	require.NoError(t, os.WriteFile(path, []byte("console=ttyS0 root=/dev/vda"), 0o600))
	require.NoError(t, ConfigureNetworkFromCmdline(path))
}

func TestConfigureNetworkFromCmdlineRejectsMalformedConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cmdline")
	require.NoError(t, os.WriteFile(path, []byte("console=ttyS0 bunn_net=invalid"), 0o600))
	require.Error(t, ConfigureNetworkFromCmdline(path))
}

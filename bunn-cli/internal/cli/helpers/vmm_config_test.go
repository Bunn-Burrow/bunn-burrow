package helpers

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"

	"github.com/Bunn-Burrow/bunn-cli/internal/config"
)

func TestResolveNetworkConfig(t *testing.T) {
	loader := config.NewLoader()
	settings := loader.Viper()
	settings.Set(config.KeyNetworkBridge, "br-test")
	settings.Set(config.KeyNetworkCIDR, "10.20.0.0/16")
	settings.Set(config.KeyNetworkGateway, "10.20.0.1")
	settings.Set(config.KeyNetworkDNS, []string{"1.1.1.1", "8.8.8.8"})
	settings.Set(config.KeyNetworkNATInterface, "eth-test")
	settings.Set(config.KeyNetworkFirewall, false)
	settings.Set(config.KeyNetworkMTU, 1400)

	got, err := ResolveNetworkConfig(loader)
	require.NoError(t, err)
	require.True(t, got.Enabled)
	require.Equal(t, "br-test", got.Bridge)
	require.Equal(t, "10.20.0.0/16", got.CIDR)
	require.Equal(t, net.ParseIP("10.20.0.1"), got.Gateway)
	require.Equal(t, []net.IP{net.ParseIP("1.1.1.1").To4(), net.ParseIP("8.8.8.8").To4()}, got.DNS)
	require.Equal(t, "eth-test", got.NATInterface)
	require.False(t, got.Firewall)
	require.Equal(t, 1400, got.MTU)
}

func TestResolveNetworkConfigRejectsInvalidAddresses(t *testing.T) {
	t.Run("gateway", func(t *testing.T) {
		loader := config.NewLoader()
		loader.Viper().Set(config.KeyNetworkGateway, "not-an-ip")

		_, err := ResolveNetworkConfig(loader)
		require.ErrorContains(t, err, `invalid gateway "not-an-ip"`)
	})

	t.Run("DNS", func(t *testing.T) {
		loader := config.NewLoader()
		loader.Viper().Set(config.KeyNetworkDNS, []string{"2001:db8::1"})

		_, err := ResolveNetworkConfig(loader)
		require.ErrorContains(t, err, `invalid IPv4 DNS server "2001:db8::1"`)
	})
}

func TestBuildVMMConfigUsesNetworkDefaults(t *testing.T) {
	loader := config.NewLoader()

	got, err := BuildVMMConfigFromViper(loader, "test-vm")
	require.NoError(t, err)
	require.Nil(t, got.Network)

	loader.Viper().Set(config.KeyNetworkEnabled, true)
	got, err = BuildVMMConfigFromViper(loader, "test-vm")
	require.NoError(t, err)
	require.NotNil(t, got.Network)
	require.True(t, got.Network.Enabled)
	require.Equal(t, "br-bunn", got.Network.Bridge)
	require.Equal(t, "10.11.0.0/16", got.Network.CIDR)
	require.True(t, got.Network.Firewall)
	require.Equal(t, 1500, got.Network.MTU)
}

func TestVMMConfigFlagBindingPreservesPrecedence(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`runtime:
  kernel_path: /file/kernel
  rootfs_path: /file/rootfs
  work_dir: /file/work
`), 0o600))
	t.Setenv("BUNN_BURROW_RUNTIME_ROOTFS_PATH", "/env/rootfs")

	for _, workDirFlag := range []string{"workdir", "vm-workdir"} {
		t.Run(workDirFlag, func(t *testing.T) {
			loader := config.NewLoader()
			flags := pflag.NewFlagSet("vmm", pflag.ContinueOnError)
			flags.String("kernel", "", "")
			flags.String("rootfs", "", "")
			flags.String(workDirFlag, "", "")
			flags.String("bin", "", "")
			flags.Bool("network", false, "")
			flags.String("network-bridge", "", "")
			flags.String("network-cidr", "", "")
			flags.String("network-gateway", "", "")
			flags.String("network-nat-iface", "", "")
			flags.Bool("network-firewall", true, "")
			flags.Int("network-mtu", 1500, "")
			require.NoError(t, flags.Set("kernel", "/flag/kernel"))
			require.NoError(t, flags.Set(workDirFlag, "/flag/work"))

			BindVMMConfigFlags(loader, flags, workDirFlag)
			_, err := loader.Load(configPath)
			require.NoError(t, err)

			got, err := BuildVMMConfigFromViper(loader, "test-vm")
			require.NoError(t, err)
			require.Equal(t, "/flag/kernel", got.KernelImage)
			require.Equal(t, "/env/rootfs", got.RootFS)
			require.Equal(t, "/flag/work", got.WorkDir)
		})
	}
}

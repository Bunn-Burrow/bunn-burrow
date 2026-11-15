package helpers

import (
	"fmt"
	"net"
	"time"

	"github.com/spf13/pflag"

	"github.com/Bunn-Burrow/bunn-burrow/vmm"
	"github.com/Bunn-Burrow/bunn-cli/internal/config"
)

func LoadVMMConfig(loader *config.Loader, flags *pflag.FlagSet, workDirFlag, vmID string) (vmm.Config, error) {
	BindVMMConfigFlags(loader, flags, workDirFlag)
	if _, err := loader.Load(""); err != nil {
		return vmm.Config{}, err
	}
	return BuildVMMConfigFromViper(loader, vmID)
}

func BindVMMConfigFlags(loader *config.Loader, flags *pflag.FlagSet, workDirFlag string) {
	bindings := []struct {
		key      string
		flagName string
	}{
		{config.KeyRuntimeKernelPath, "kernel"},
		{config.KeyRuntimeRootFSPath, "rootfs"},
		{config.KeyRuntimeWorkDir, workDirFlag},
		{config.KeyRuntimeFirecrackerBin, "bin"},
		{config.KeyNetworkEnabled, "network"},
		{config.KeyNetworkBridge, "network-bridge"},
		{config.KeyNetworkCIDR, "network-cidr"},
		{config.KeyNetworkGateway, "network-gateway"},
		{config.KeyNetworkNATInterface, "network-nat-iface"},
		{config.KeyNetworkFirewall, "network-firewall"},
		{config.KeyNetworkMTU, "network-mtu"},
	}
	for _, binding := range bindings {
		loader.BindPFlag(binding.key, flags.Lookup(binding.flagName))
	}
}

func BuildVMMConfigFromViper(loader *config.Loader, vmID string) (vmm.Config, error) {
	settings := loader.Viper()
	cfg := vmm.DefaultConfig()
	cfg.ID = vmID
	cfg.KernelImage = settings.GetString(config.KeyRuntimeKernelPath)
	cfg.RootFS = settings.GetString(config.KeyRuntimeRootFSPath)
	cfg.WorkDir = settings.GetString(config.KeyRuntimeWorkDir)
	cfg.BinPath = settings.GetString(config.KeyRuntimeFirecrackerBin)
	cfg.StartTimeout = settings.GetDuration(config.KeyRuntimeStartTimeout)
	cfg.StopTimeout = settings.GetDuration(config.KeyRuntimeStopTimeout)
	cfg.StopGraceTimeout = 5 * time.Second

	if settings.GetBool(config.KeyNetworkEnabled) {
		network, err := ResolveNetworkConfig(loader)
		if err != nil {
			return vmm.Config{}, err
		}
		cfg.Network = network
	}

	return cfg, nil
}

func ResolveNetworkConfig(loader *config.Loader) (*vmm.NetworkConfig, error) {
	settings := loader.Viper()
	gatewayString := settings.GetString(config.KeyNetworkGateway)
	var gateway net.IP
	if gatewayString != "" {
		gateway = net.ParseIP(gatewayString)
		if gateway == nil {
			return nil, fmt.Errorf("invalid gateway %q", gatewayString)
		}
	}

	dns := make([]net.IP, 0)
	for _, value := range settings.GetStringSlice(config.KeyNetworkDNS) {
		server := net.ParseIP(value).To4()
		if server == nil {
			return nil, fmt.Errorf("invalid IPv4 DNS server %q", value)
		}
		dns = append(dns, append(net.IP(nil), server...))
	}

	return &vmm.NetworkConfig{
		Enabled:      true,
		Bridge:       settings.GetString(config.KeyNetworkBridge),
		CIDR:         settings.GetString(config.KeyNetworkCIDR),
		Gateway:      gateway,
		DNS:          dns,
		NATInterface: settings.GetString(config.KeyNetworkNATInterface),
		Firewall:     settings.GetBool(config.KeyNetworkFirewall),
		MTU:          settings.GetInt(config.KeyNetworkMTU),
	}, nil
}

package vmm

import (
	"net"
	"testing"

	"github.com/omkar/sandbox/internal/network"
	"github.com/stretchr/testify/require"
)

func TestNetworkBootArg(t *testing.T) {
	_, subnet, err := net.ParseCIDR("10.11.0.0/16")
	require.NoError(t, err)
	vm := &VM{network: &network.Network{
		IP:        net.ParseIP("10.11.0.2").To4(),
		Gateway:   net.ParseIP("10.11.0.1").To4(),
		DNS:       []net.IP{net.ParseIP("10.255.255.254").To4()},
		MAC:       net.HardwareAddr{0x02, 0x11, 0x22, 0x33, 0x44, 0x55},
		Interface: "eth0",
		MTU:       1500,
		Subnet:    subnet,
	}}

	arg, err := vm.networkBootArg()
	require.NoError(t, err)
	require.Equal(t, "bunn_net=eth0|02:11:22:33:44:55|10.11.0.2/16|10.11.0.1|10.255.255.254|1500", arg)
}

func TestNetworkBootArgRequiresDNS(t *testing.T) {
	_, subnet, err := net.ParseCIDR("10.11.0.0/16")
	require.NoError(t, err)
	vm := &VM{network: &network.Network{
		IP:        net.ParseIP("10.11.0.2").To4(),
		Gateway:   net.ParseIP("10.11.0.1").To4(),
		MAC:       net.HardwareAddr{0x02, 0x11, 0x22, 0x33, 0x44, 0x55},
		Interface: "eth0",
		MTU:       1500,
		Subnet:    subnet,
	}}

	_, err = vm.networkBootArg()
	require.ErrorContains(t, err, "no DNS servers")
}

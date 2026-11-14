package network

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateHelperRequestAcceptsScopedOperations(t *testing.T) {
	tests := []helperRequest{
		{
			Version:   helperProtocol,
			Operation: "bridge.ensure",
			Bridge:    "br-bunn",
			Gateway:   "10.11.0.1",
			CIDR:      "10.11.0.0/16",
		},
		{
			Version:        helperProtocol,
			Operation:      "tap.create",
			Tap:            "tap-vm-1",
			MAC:            "02:11:22:33:44:55",
			MTU:            1500,
			IP:             "10.11.0.2",
			OwnerPID:       1234,
			OwnerStartTime: "5678",
		},
		{
			Version:      helperProtocol,
			Operation:    "firewall.install",
			Bridge:       "br-bunn",
			CIDR:         "10.11.0.0/16",
			NATInterface: "wlan0",
			DNS:          []string{"1.1.1.1", "8.8.8.8"},
		},
	}

	for _, request := range tests {
		require.NoError(t, validateHelperRequest(request))
	}

}

func TestValidateHelperRequestRejectsUnsafeArguments(t *testing.T) {
	tests := []struct {
		name    string
		request helperRequest
		message string
	}{
		{
			name: "unsupported operation",
			request: helperRequest{
				Version:   helperProtocol,
				Operation: "exec",
			},
			message: "unsupported privileged network operation",
		},
		{
			name: "non-Bunn bridge",
			request: helperRequest{
				Version:   helperProtocol,
				Operation: "bridge.delete",
				Bridge:    "docker0",
			},
			message: "invalid Bunn bridge",
		},
		{
			name: "invalid TAP name",
			request: helperRequest{
				Version:   helperProtocol,
				Operation: "tap.delete",
				Tap:       "tap;reboot",
			},
			message: "invalid Bunn TAP",
		},
		{
			name: "invalid interface",
			request: helperRequest{
				Version:      helperProtocol,
				Operation:    "firewall.install",
				Bridge:       "br-bunn",
				CIDR:         "10.11.0.0/16",
				NATInterface: "eth0;reboot",
			},
			message: "invalid NAT interface",
		},
		{
			name: "too many DNS servers",
			request: helperRequest{
				Version:   helperProtocol,
				Operation: "firewall.install",
				Bridge:    "br-bunn",
				CIDR:      "10.11.0.0/16",
				DNS: []string{
					"1.1.1.1", "1.1.1.2", "1.1.1.3", "1.1.1.4",
					"1.1.1.5", "1.1.1.6", "1.1.1.7", "1.1.1.8", "1.1.1.9",
				},
			},
			message: "at most 8 DNS servers",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateHelperRequest(test.request)
			require.ErrorContains(t, err, test.message)
		})
	}
}

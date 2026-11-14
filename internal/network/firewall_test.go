package network

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

type fakeIptablesExitError struct {
	code int
}

func (e fakeIptablesExitError) Error() string { return "iptables rule not found" }
func (e fakeIptablesExitError) ExitCode() int { return e.code }

func TestFirewallInstallUsesIPFamilyAndInstallsNAT(t *testing.T) {
	firewall, commands, iptablesCommands, forwardingPath := newTestFirewall(t)

	require.NoError(t, firewall.Install())
	require.FileExists(t, forwardingPath)
	forwarding, err := os.ReadFile(forwardingPath)
	require.NoError(t, err)
	require.Equal(t, "1\n", string(forwarding))

	for _, args := range *commands {
		for i, arg := range args {
			if arg == "ip" || arg == "bridge" || arg == "inet" {
				if i > 0 && (args[i-1] == "table" || args[i-1] == "chain" || args[i-1] == "set" || args[i-1] == "rule") {
					require.Equal(t, "ip", arg, "unsupported nftables family in command %q", strings.Join(args, " "))
				}
			}
		}
	}

	require.Contains(t, commandStrings(*commands),
		`add rule ip bunn-nat postrouting ip saddr 10.11.0.0/24 oifname eth0 masquerade comment bunn-burrow`)
	require.Contains(t, commandStrings(*iptablesCommands),
		"-t filter -I FORWARD 1 -i br-test -s 10.11.0.0/24 -o eth0 -m comment --comment bunn-burrow -j ACCEPT")
	require.Contains(t, commandStrings(*iptablesCommands),
		"-t filter -I FORWARD 1 -i eth0 -o br-test -d 10.11.0.0/24 -m conntrack --ctstate ESTABLISHED,RELATED -m comment --comment bunn-burrow -j ACCEPT")
	require.Contains(t, commandStrings(*iptablesCommands),
		"-t filter -I FORWARD 1 -o br-test -d 10.11.0.0/24 -m comment --comment bunn-burrow -j DROP")
	require.Contains(t, commandStrings(*iptablesCommands),
		"-t filter -I INPUT 1 -i br-test -s 10.11.0.0/24 -d 1.1.1.1 -p udp --dport 53 -m comment --comment bunn-burrow -j ACCEPT")
	require.NotContains(t, commandStrings(*commands), "docker")
}

func TestHasNATRuleMatchesRenderedRuleWithCounters(t *testing.T) {
	rules := []byte(`table ip bunn-nat {
	chain postrouting {
		ip saddr 10.11.0.0/24 oifname "eth0" counter packets 0 bytes 0 masquerade comment "bunn-burrow"
	}
}`)
	require.True(t, hasNATRule(rules, "10.11.0.0/24", "eth0"))
	require.False(t, hasNATRule(rules, "10.12.0.0/24", "eth0"))
	require.False(t, hasNATRule(rules, "10.11.0.0/24", "eth1"))
}

func TestFirewallInstallIsIdempotent(t *testing.T) {
	firewall, commands, iptablesCommands, _ := newTestFirewall(t)
	firewall.runNFT = nil
	tableExists, chainExists, natRuleExists := false, false, false
	firewall.runNFTOutput = func(args ...string) ([]byte, error) {
		*commands = append(*commands, append([]string(nil), args...))
		switch strings.Join(args, " ") {
		case "add table ip bunn-nat":
			if tableExists {
				return nil, errors.New("File exists")
			}
			tableExists = true
		case "add chain ip bunn-nat postrouting { type nat hook postrouting priority 100; policy accept; }":
			if chainExists {
				return nil, errors.New("File exists")
			}
			chainExists = true
		case "list chain ip bunn-nat postrouting":
			if natRuleExists {
				return []byte(`ip saddr 10.11.0.0/24 oifname "eth0" counter packets 0 bytes 0 masquerade comment "bunn-burrow"`), nil
			}
		case "add rule ip bunn-nat postrouting ip saddr 10.11.0.0/24 oifname eth0 masquerade comment bunn-burrow":
			natRuleExists = true
		}
		return nil, nil
	}
	iptablesRules := make(map[string]bool)
	firewall.runIPTables = func(args ...string) error {
		*iptablesCommands = append(*iptablesCommands, append([]string(nil), args...))
		op := args[2]
		rule := append([]string{args[1], args[3]}, args[4:]...)
		if op == "-I" {
			rule = append([]string{args[1], args[3]}, args[5:]...)
		}
		key := strings.Join(rule, " ")
		switch op {
		case "-C":
			if !iptablesRules[key] {
				return fakeIptablesExitError{code: 1}
			}
		case "-I":
			iptablesRules[key] = true
		case "-D":
			delete(iptablesRules, key)
		}
		return nil
	}

	require.NoError(t, firewall.Install())
	require.NoError(t, firewall.Install())

	require.Equal(t, 1, countCommand(*commands,
		"add rule ip bunn-nat postrouting ip saddr 10.11.0.0/24 oifname eth0 masquerade comment bunn-burrow"))
	require.Equal(t, 5, countCommandsWith(*iptablesCommands, "-I"))
}

func TestFirewallRemoveDeletesNATAndRestoresForwarding(t *testing.T) {
	firewall, commands, iptablesCommands, forwardingPath := newTestFirewall(t)
	require.NoError(t, firewall.Install())
	require.NoError(t, firewall.Remove())

	forwarding, err := os.ReadFile(forwardingPath)
	require.NoError(t, err)
	require.Equal(t, "0\n", string(forwarding))
	require.Contains(t, commandStrings(*commands), "delete table ip bunn-nat")
	require.Contains(t, commandStrings(*iptablesCommands),
		"-t filter -D FORWARD -i br-test -s 10.11.0.0/24 -o eth0 -m comment --comment bunn-burrow -j ACCEPT")
	for _, args := range *commands {
		require.NotContains(t, args, "bridge")
		require.NotContains(t, args, "docker")
	}
}

func TestFirewallInstallCleansUpPartialNATRules(t *testing.T) {
	firewall, commands, _, forwardingPath := newTestFirewall(t)
	failure := errors.New("chain creation failed")
	firewall.runNFT = func(args ...string) error {
		*commands = append(*commands, append([]string(nil), args...))
		if strings.Join(args, " ") == "add chain ip bunn-nat postrouting { type nat hook postrouting priority 100; policy accept; }" {
			return failure
		}
		return nil
	}

	err := firewall.Install()
	require.ErrorIs(t, err, failure)
	require.Contains(t, commandStrings(*commands), "delete table ip bunn-nat")
	forwarding, readErr := os.ReadFile(forwardingPath)
	require.NoError(t, readErr)
	require.Equal(t, "0\n", string(forwarding))
}

func newTestFirewall(t *testing.T) (*Firewall, *[][]string, *[][]string, string) {
	t.Helper()
	forwardingPath := filepath.Join(t.TempDir(), "ip_forward")
	require.NoError(t, os.WriteFile(forwardingPath, []byte("0\n"), 0o600))
	commands := new([][]string)
	iptablesCommands := new([][]string)
	firewall := NewFirewall("br-test", "10.11.0.0/24", "eth0", logrus.NewEntry(logrus.New()))
	firewall.SetDNSServers([]net.IP{net.ParseIP("1.1.1.1")})
	firewall.forwardingPath = forwardingPath
	firewall.runNFT = func(args ...string) error {
		*commands = append(*commands, append([]string(nil), args...))
		return nil
	}
	firewall.runIPTables = func(args ...string) error {
		*iptablesCommands = append(*iptablesCommands, append([]string(nil), args...))
		for _, arg := range args {
			if arg == "-C" || arg == "-D" {
				return fakeIptablesExitError{code: 1}
			}
		}
		return nil
	}
	return firewall, commands, iptablesCommands, forwardingPath
}

func commandStrings(commands [][]string) []string {
	result := make([]string, len(commands))
	for i, args := range commands {
		result[i] = strings.Join(args, " ")
	}
	return result
}

func countCommand(commands [][]string, expected string) int {
	count := 0
	for _, args := range commands {
		if strings.Join(args, " ") == expected {
			count++
		}
	}
	return count
}

func countCommandsWith(commands [][]string, value string) int {
	count := 0
	for _, args := range commands {
		for _, arg := range args {
			if arg == value {
				count++
				break
			}
		}
	}
	return count
}

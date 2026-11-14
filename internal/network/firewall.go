package network

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/sirupsen/logrus"
)

// Firewall manages nftables NAT/masquerade and IPv4 forwarding for VM networking.
type Firewall struct {
	bridge                string
	cidr                  string
	natIface              string
	logger                *logrus.Entry
	changedForwarding     bool
	installedNATInterface string
	runNFT                func(args ...string) error
	runNFTOutput          func(args ...string) ([]byte, error)
	runIPTables           func(args ...string) error
	forwardingPath        string
	dnsServers            []net.IP
	createdNATTable       bool
	createdNATChain       bool
	addedForwardingRules  [][]string
}

const firewallRuleComment = "bunn-burrow"

// NewFirewall creates a Firewall manager.
func NewFirewall(bridge, cidr, natIface string, logger *logrus.Entry) *Firewall {
	if logger == nil {
		logger = logrus.NewEntry(logrus.New())
	}
	return &Firewall{
		bridge:   bridge,
		cidr:     cidr,
		natIface: natIface,
		logger:   logger.WithField("component", "network-firewall"),
	}
}

// SetDNSServers configures narrowly scoped host INPUT exceptions for nameservers
// that resolve to host-local addresses, such as the WSL DNS stub.
func (f *Firewall) SetDNSServers(servers []net.IP) {
	f.dnsServers = cloneIPs(servers)
}

// nft executes an nft command.
func (f *Firewall) nft(args ...string) error {
	_, err := f.nftOutput(args...)
	return err
}

func (f *Firewall) nftOutput(args ...string) ([]byte, error) {
	var out []byte
	var err error
	if f.runNFTOutput != nil {
		out, err = f.runNFTOutput(args...)
	} else if f.runNFT != nil {
		err = f.runNFT(args...)
	} else {
		cmd := exec.Command("nft", args...)
		out, err = cmd.CombinedOutput()
	}
	if err != nil {
		return out, fmt.Errorf("nft %s: %w: %s", strings.Join(args, " "), err, string(out))
	}
	return out, nil
}

// Install sets up the nftables ruleset:
//   - NAT postrouting masquerade for VM subnet outbound traffic.
func (f *Firewall) Install() (installErr error) {
	if f.runNFT == nil && f.runIPTables == nil && os.Geteuid() != 0 {
		response, err := callNetworkHelper(helperRequest{
			Version:      helperProtocol,
			Operation:    "firewall.install",
			Bridge:       f.bridge,
			CIDR:         f.cidr,
			NATInterface: f.natIface,
			DNS:          ipStrings(f.dnsServers),
		})
		if response.ForwardingStatePresent {
			f.changedForwarding = response.ChangedForwarding
			f.installedNATInterface = response.NATInterface
		}
		return err
	}

	defer func() {
		if installErr == nil {
			return
		}
		if cleanupErr := f.cleanupFailedInstall(); cleanupErr != nil {
			installErr = errors.Join(installErr, fmt.Errorf("clean up incomplete firewall installation: %w", cleanupErr))
		}
	}()
	f.createdNATTable = false
	f.createdNATChain = false
	f.addedForwardingRules = nil

	natIface := f.natIface
	if natIface == "" {
		var err error
		natIface, err = defaultRouteInterface()
		if err != nil {
			return fmt.Errorf("determine NAT interface: %w", err)
		}
	}
	if !validInterfaceName(natIface) {
		return fmt.Errorf("invalid NAT interface name %q", natIface)
	}
	f.installedNATInterface = natIface

	if err := f.nft("add", "table", "ip", "bunn-nat"); err != nil {
		if !isNftAlreadyExists(err) {
			return fmt.Errorf("create NAT table: %w", err)
		}
	} else {
		f.createdNATTable = true
	}
	if err := f.nft("add", "chain", "ip", "bunn-nat", "postrouting",
		"{ type nat hook postrouting priority 100; policy accept; }"); err != nil {
		if !isNftAlreadyExists(err) {
			return fmt.Errorf("create NAT postrouting chain: %w", err)
		}
	} else {
		f.createdNATChain = true
	}

	for _, rule := range f.forwardingRules(natIface) {
		added, err := f.ensureForwardingRule(rule)
		if err != nil {
			return fmt.Errorf("install forwarding rule: %w", err)
		}
		if added {
			f.addedForwardingRules = append(f.addedForwardingRules, rule)
		}
	}
	if err := f.enableIPv4Forwarding(); err != nil {
		return fmt.Errorf("enable IPv4 forwarding: %w", err)
	}
	rules, err := f.nftOutput("list", "chain", "ip", "bunn-nat", "postrouting")
	if err != nil {
		return fmt.Errorf("inspect NAT postrouting chain: %w", err)
	}
	if !hasNATRule(rules, f.cidr, natIface) {
		if err := f.nft("add", "rule", "ip", "bunn-nat", "postrouting",
			"ip", "saddr", f.cidr, "oifname", natIface,
			"masquerade", "comment", firewallRuleComment); err != nil {
			return fmt.Errorf("add masquerade rule: %w", err)
		}
	}

	f.logger.Info("nftables rules installed",
		"bridge", f.bridge, "cidr", f.cidr, "nat_iface", natIface)
	f.createdNATTable = false
	f.createdNATChain = false
	f.addedForwardingRules = nil
	return nil
}

func hasNATRule(rules []byte, cidr, natIface string) bool {
	source := "ip saddr " + cidr
	output := "oifname " + strconv.Quote(natIface)
	comment := "comment " + strconv.Quote(firewallRuleComment)
	for _, line := range strings.Split(string(rules), "\n") {
		if strings.Contains(line, source) &&
			strings.Contains(line, output) &&
			strings.Contains(line, comment) &&
			strings.Contains(line, "masquerade") {
			return true
		}
	}
	return false
}

// Remove deletes the nftables ruleset for VM networking.
func (f *Firewall) Remove() error {
	if f.runNFT == nil && f.runIPTables == nil && os.Geteuid() != 0 {
		response, err := callNetworkHelper(helperRequest{
			Version:           helperProtocol,
			Operation:         "firewall.remove",
			Bridge:            f.bridge,
			CIDR:              f.cidr,
			NATInterface:      f.networkInterfaceForRemoval(),
			DNS:               ipStrings(f.dnsServers),
			ChangedForwarding: f.changedForwarding,
		})
		if response.ForwardingStatePresent {
			f.changedForwarding = response.ChangedForwarding
			f.installedNATInterface = response.NATInterface
		}
		return err
	}

	var errs []error
	for _, rule := range f.forwardingRules(f.networkInterfaceForRemoval()) {
		if err := f.removeForwardingRule(rule); err != nil {
			errs = append(errs, err)
		}
	}
	if err := f.deleteTableIfPresent("ip", "bunn-nat"); err != nil {
		errs = append(errs, err)
	}
	if err := f.restoreIPv4Forwarding(); err != nil {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err == nil {
		f.installedNATInterface = ""
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("remove firewall rules: %w", err)
	}
	f.logger.Info("nftables rules removed")
	return nil
}

func (f *Firewall) forwardingRules(natIface string) [][]string {
	var rules [][]string
	if natIface != "" {
		rules = append(rules,
			[]string{
				"-t", "filter", "FORWARD",
				"-o", f.bridge, "-d", f.cidr,
				"-m", "comment", "--comment", firewallRuleComment, "-j", "DROP",
			},
			[]string{
				"-t", "filter", "FORWARD",
				"-i", natIface, "-o", f.bridge, "-d", f.cidr,
				"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED",
				"-m", "comment", "--comment", firewallRuleComment, "-j", "ACCEPT",
			},
			[]string{
				"-t", "filter", "FORWARD",
				"-i", f.bridge, "-s", f.cidr, "-o", natIface,
				"-m", "comment", "--comment", firewallRuleComment, "-j", "ACCEPT",
			},
		)
	}
	for _, server := range f.dnsServers {
		if server.To4() == nil {
			continue
		}
		for _, protocol := range []string{"udp", "tcp"} {
			rules = append(rules, []string{
				"-t", "filter", "INPUT",
				"-i", f.bridge, "-s", f.cidr, "-d", server.To4().String(),
				"-p", protocol, "--dport", "53",
				"-m", "comment", "--comment", firewallRuleComment, "-j", "ACCEPT",
			})
		}
	}
	return rules
}

func (f *Firewall) networkInterfaceForRemoval() string {
	if f.installedNATInterface != "" {
		return f.installedNATInterface
	}
	return f.natIface
}

func (f *Firewall) ensureForwardingRule(rule []string) (bool, error) {
	check := append([]string(nil), rule...)
	check = append([]string{check[0], check[1], "-C"}, check[2:]...)
	if err := f.iptables(check...); err == nil {
		return false, nil
	} else if !isIptablesRuleMissing(err) {
		return false, err
	}

	insert := append([]string(nil), rule...)
	insert = append([]string{insert[0], insert[1], "-I", insert[2], "1"}, insert[3:]...)
	if err := f.iptables(insert...); err != nil {
		return false, err
	}
	return true, nil
}

func (f *Firewall) cleanupFailedInstall() error {
	var errs []error
	for i := len(f.addedForwardingRules) - 1; i >= 0; i-- {
		if err := f.removeForwardingRule(f.addedForwardingRules[i]); err != nil {
			errs = append(errs, err)
		}
	}
	if f.createdNATTable {
		if err := f.deleteTableIfPresent("ip", "bunn-nat"); err != nil {
			errs = append(errs, err)
		}
	} else if f.createdNATChain {
		if err := f.nft("delete", "chain", "ip", "bunn-nat", "postrouting"); err != nil {
			errs = append(errs, err)
		}
	}
	if err := f.restoreIPv4Forwarding(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func isNftAlreadyExists(err error) bool {
	return strings.Contains(err.Error(), "File exists")
}

func (f *Firewall) removeForwardingRule(rule []string) error {
	remove := append([]string(nil), rule...)
	remove = append([]string{remove[0], remove[1], "-D"}, remove[2:]...)
	err := f.iptables(remove...)
	if err != nil && !isIptablesRuleMissing(err) {
		return err
	}
	return nil
}

func (f *Firewall) iptables(args ...string) error {
	var output []byte
	var err error
	if f.runIPTables != nil {
		err = f.runIPTables(args...)
	} else {
		fullArgs := append([]string{"-w"}, args...)
		output, err = exec.Command("iptables", fullArgs...).CombinedOutput()
	}
	if err != nil {
		return fmt.Errorf("iptables %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func isIptablesRuleMissing(err error) bool {
	var exitError interface{ ExitCode() int }
	if !errors.As(err, &exitError) || exitError.ExitCode() != 1 {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "rule not found") ||
		strings.Contains(message, "bad rule") ||
		strings.Contains(message, "does a matching rule exist")
}

func ipStrings(addresses []net.IP) []string {
	result := make([]string, len(addresses))
	for i, address := range addresses {
		result[i] = address.String()
	}
	return result
}

func (f *Firewall) enableIPv4Forwarding() error {
	path := f.ipv4ForwardingPath()
	value, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	switch strings.TrimSpace(string(value)) {
	case "1":
		return nil
	case "0":
		if err := os.WriteFile(path, []byte("1\n"), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		f.changedForwarding = true
		return nil
	default:
		return fmt.Errorf("unexpected value %q in %s", strings.TrimSpace(string(value)), path)
	}
}

func (f *Firewall) restoreIPv4Forwarding() error {
	if !f.changedForwarding {
		return nil
	}
	path := f.ipv4ForwardingPath()
	if err := os.WriteFile(path, []byte("0\n"), 0o644); err != nil {
		return fmt.Errorf("restore %s: %w", path, err)
	}
	f.changedForwarding = false
	return nil
}

func (f *Firewall) ipv4ForwardingPath() string {
	if f.forwardingPath != "" {
		return f.forwardingPath
	}
	return "/proc/sys/net/ipv4/ip_forward"
}

func (f *Firewall) deleteTableIfPresent(family, table string) error {
	err := f.nft("delete", "table", family, table)
	if err == nil || strings.Contains(err.Error(), "No such file or directory") {
		return nil
	}
	return err
}

func validInterfaceName(name string) bool {
	if len(name) == 0 || len(name) > 15 {
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

// defaultRouteInterface returns the host interface for the default route.
func defaultRouteInterface() (string, error) {
	out, err := exec.Command("ip", "route", "get", "1.1.1.1").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ip route get: %w: %s", err, string(out))
	}
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "dev" && i+1 < len(fields) {
			if !validInterfaceName(fields[i+1]) {
				return "", fmt.Errorf("invalid interface name %q in route output", fields[i+1])
			}
			return fields[i+1], nil
		}
	}
	return "", fmt.Errorf("default route output has no device: %s", strings.TrimSpace(string(out)))
}

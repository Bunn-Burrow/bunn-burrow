package network

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/sirupsen/logrus"
)

// Firewall manages nftables rules for VM networking: Layer-2 isolation,
// NAT/masquerade for outbound, and forwarding.
type Firewall struct {
	bridge    string
	cidr      string
	natIface  string
	logger    *logrus.Entry
	tableName string
	chainName string
}

// NewFirewall creates a Firewall manager.
func NewFirewall(bridge, cidr, natIface string, logger *logrus.Entry) *Firewall {
	if logger == nil {
		logger = logrus.NewEntry(logrus.New())
	}
	return &Firewall{
		bridge:    bridge,
		cidr:      cidr,
		natIface:  natIface,
		logger:    logger.WithField("component", "network-firewall"),
		tableName: "bunn",
		chainName: "vm-filter",
	}
}

// nft executes an nft command.
func (f *Firewall) nft(args ...string) error {
	cmd := exec.Command("nft", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("nft %s: %w: %s", strings.Join(args, " "), err, string(out))
	}
	return nil
}

// Install sets up the nftables ruleset:
//   - Bridge-family filter dropping VM-to-VM traffic at Layer 2.
//   - NAT postrouting masquerade for VM subnet outbound traffic.
func (f *Firewall) Install() error {
	// Flush any existing bunn rules (idempotent).
	_ = f.nft("delete", "table", "bridge", "bunn")
	_ = f.nft("delete", "table", "ip", "bunn-nat")

	// Create bridge table and chain for Layer-2 isolation.
	if err := f.nft("add", "table", "bridge", f.tableName); err != nil {
		return fmt.Errorf("create bridge table: %w", err)
	}
	if err := f.nft("add", "chain", "bridge", f.tableName, f.chainName,
		"{ type filter hook forward priority 0; policy accept; }"); err != nil {
		return fmt.Errorf("create bridge chain: %w", err)
	}

	// Drop traffic between VM TAPs (Layer-2 isolation).
	if err := f.nft("add", "rule", "bridge", f.tableName, f.chainName,
		"iifname", "@"+f.bridge, "oifname", "@"+f.bridge, "drop"); err != nil {
		return fmt.Errorf("add VM-to-VM drop rule: %w", err)
	}

	// Create IP NAT table for masquerade.
	if err := f.nft("add", "table", "ip", "bunn-nat"); err != nil {
		return fmt.Errorf("create NAT table: %w", err)
	}
	if err := f.nft("add", "chain", "ip", "bunn-nat", "postrouting",
		"{ type nat hook postrouting priority 100; policy accept; }"); err != nil {
		return fmt.Errorf("create NAT postrouting chain: %w", err)
	}

	natIface := f.natIface
	if natIface == "" {
		natIface = defaultRouteInterface()
	}

	if err := f.nft("add", "rule", "ip", "bunn-nat", "postrouting",
		"ip", "saddr", f.cidr, "oifname", natIface, "masquerade"); err != nil {
		return fmt.Errorf("add masquerade rule: %w", err)
	}

	f.logger.Info("nftables rules installed",
		"bridge", f.bridge, "cidr", f.cidr, "nat_iface", natIface)
	return nil
}

// Remove deletes the nftables ruleset for VM networking.
func (f *Firewall) Remove() error {
	var errs []string
	if err := f.nft("delete", "table", "bridge", f.tableName); err != nil {
		errs = append(errs, err.Error())
	}
	if err := f.nft("delete", "table", "ip", "bunn-nat"); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return fmt.Errorf("remove firewall rules: %s", strings.Join(errs, "; "))
	}
	f.logger.Info("nftables rules removed")
	return nil
}

// defaultRouteInterface returns the host interface for the default route.
func defaultRouteInterface() string {
	out, err := exec.Command("ip", "route", "get", "1.1.1.1").CombinedOutput()
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "dev" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

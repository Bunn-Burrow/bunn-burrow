package network

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
)

const (
	networkHelperPath   = "/usr/local/libexec/bunn-burrow/bunn-network-helper"
	helperProtocol      = 1
	maxHelperRequest    = 64 << 10
	networkLockPath     = "/run/lock/bunn-burrow-network.lock"
	networkStateDir     = "/run/bunn-burrow"
	forwardingStateFile = "/run/bunn-burrow/ip_forward.original"
)

type helperRequest struct {
	Version           int      `json:"version"`
	Operation         string   `json:"operation"`
	Bridge            string   `json:"bridge,omitempty"`
	Tap               string   `json:"tap,omitempty"`
	Gateway           string   `json:"gateway,omitempty"`
	CIDR              string   `json:"cidr,omitempty"`
	IP                string   `json:"ip,omitempty"`
	MAC               string   `json:"mac,omitempty"`
	MTU               int      `json:"mtu,omitempty"`
	OwnerPID          int      `json:"owner_pid,omitempty"`
	OwnerStartTime    string   `json:"owner_start_time,omitempty"`
	ChangedForwarding bool     `json:"changed_forwarding,omitempty"`
	NATInterface      string   `json:"nat_interface,omitempty"`
	DNS               []string `json:"dns,omitempty"`
}

type helperResponse struct {
	Error                  string `json:"error,omitempty"`
	BridgeCreated          bool   `json:"bridge_created,omitempty"`
	ChangedForwarding      bool   `json:"changed_forwarding,omitempty"`
	ForwardingStatePresent bool   `json:"forwarding_state_present,omitempty"`
	NATInterface           string `json:"nat_interface,omitempty"`
}

func callNetworkHelper(request helperRequest) (helperResponse, error) {
	if err := validateHelperRequest(request); err != nil {
		return helperResponse{}, err
	}
	if err := verifyTrustedNetworkHelper(networkHelperPath); err != nil {
		return helperResponse{}, err
	}
	sudoPath, err := exec.LookPath("sudo")
	if err != nil {
		return helperResponse{}, fmt.Errorf("networking requires sudo, but sudo was not found in PATH: %w", err)
	}

	payload, err := json.Marshal(request)
	if err != nil {
		return helperResponse{}, fmt.Errorf("encode privileged network request: %w", err)
	}
	cmd := exec.CommandContext(
		context.Background(),
		sudoPath,
		"-p", "Bunn Burrow needs administrator permission to configure VM networking: ",
		"--", networkHelperPath, "serve",
	)
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)
	runErr := cmd.Run()

	var response helperResponse
	if decodeErr := json.Unmarshal(stdout.Bytes(), &response); decodeErr != nil {
		if runErr != nil {
			return helperResponse{}, fmt.Errorf("run privileged network helper: %w: %s", runErr, strings.TrimSpace(stderr.String()))
		}
		return helperResponse{}, fmt.Errorf("decode privileged network helper response: %w", decodeErr)
	}
	if response.Error != "" {
		return response, errors.New(response.Error)
	}
	if runErr != nil {
		return response, fmt.Errorf("run privileged network helper: %w: %s", runErr, strings.TrimSpace(stderr.String()))
	}
	return response, nil
}

func verifyTrustedNetworkHelper(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect privileged network helper %q: %w", path, err)
		}
		return fmt.Errorf(
			"privileged network helper is not installed at %s; build it with `make build-cli` and install it with `make install-network-helper`",
			path,
		)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("privileged network helper %q must be a non-writable regular file", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 {
		return fmt.Errorf("privileged network helper %q must be owned by root", path)
	}
	for dir := filepath.Dir(path); dir != "/" && dir != "."; dir = filepath.Dir(dir) {
		dirInfo, err := os.Lstat(dir)
		if err != nil {
			return fmt.Errorf("inspect privileged network helper directory %q: %w", dir, err)
		}
		dirStat, ok := dirInfo.Sys().(*syscall.Stat_t)
		if !ok || dirStat.Uid != 0 || dirInfo.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("privileged network helper directory %q must be root-owned and non-writable", dir)
		}
	}
	return nil
}

// RunNetworkHelper serves one validated request. It is called only by the
// separately installed helper binary, never by the normal CLI command path.
func RunNetworkHelper(input io.Reader, output io.Writer) error {
	if os.Geteuid() != 0 {
		return errors.New("privileged network helper must run as root")
	}
	payload, err := io.ReadAll(io.LimitReader(input, maxHelperRequest+1))
	if err != nil {
		return writeHelperError(output, fmt.Errorf("read network request: %w", err))
	}
	if len(payload) > maxHelperRequest {
		return writeHelperError(output, fmt.Errorf("network helper request exceeds %d bytes", maxHelperRequest))
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var request helperRequest
	if err := decoder.Decode(&request); err != nil {
		return writeHelperError(output, fmt.Errorf("decode network request: %w", err))
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple requests are not allowed")
		}
		return writeHelperError(output, fmt.Errorf("decode network request: %w", err))
	}

	if err := validateHelperRequest(request); err != nil {
		return writeHelperError(output, err)
	}
	lock, err := lockHostNetworking()
	if err != nil {
		return writeHelperError(output, err)
	}
	response, err := executeHelperRequest(request)
	unlockErr := unlockHostNetworking(lock)
	err = errors.Join(err, unlockErr)
	if err != nil {
		response.Error = err.Error()
		if encodeErr := json.NewEncoder(output).Encode(response); encodeErr != nil {
			return errors.Join(err, fmt.Errorf("write helper response: %w", encodeErr))
		}
		return err
	}
	if err := json.NewEncoder(output).Encode(response); err != nil {
		return fmt.Errorf("write helper response: %w", err)
	}
	return nil
}

func writeHelperError(output io.Writer, cause error) error {
	response := helperResponse{Error: cause.Error()}
	if err := json.NewEncoder(output).Encode(response); err != nil {
		return errors.Join(cause, fmt.Errorf("write helper response: %w", err))
	}
	return cause
}

func executeHelperRequest(request helperRequest) (helperResponse, error) {
	logger := helperLogger()
	switch request.Operation {
	case "bridge.ensure":
		_, subnet, _ := net.ParseCIDR(request.CIDR)
		bridge := NewBridgeManager(request.Bridge, logger)
		err := bridge.Ensure(net.ParseIP(request.Gateway), subnet)
		return helperResponse{BridgeCreated: bridge.created}, err
	case "bridge.add-port":
		tap, err := netlink.LinkByName(request.Tap)
		if err != nil {
			return helperResponse{}, fmt.Errorf("lookup TAP %q: %w", request.Tap, err)
		}
		if _, ok := tap.(*netlink.Tuntap); !ok || !strings.HasPrefix(tap.Attrs().Alias, "bunn-burrow:") {
			return helperResponse{}, fmt.Errorf("refusing to attach TAP %q not owned by Bunn Burrow", request.Tap)
		}
		return helperResponse{}, NewBridgeManager(request.Bridge, logger).AddPort(request.Tap)
	case "bridge.delete":
		return helperResponse{}, NewBridgeManager(request.Bridge, logger).Delete()
	case "tap.create":
		mac, _ := net.ParseMAC(request.MAC)
		return helperResponse{}, NewTapManager(logger).create(
			request.Tap,
			mac,
			request.MTU,
			networkTapAlias(request.OwnerPID, request.OwnerStartTime, request.IP),
		)
	case "tap.delete":
		return helperResponse{}, NewTapManager(logger).Delete(request.Tap)
	case "tap.reconcile":
		return helperResponse{}, NewTapManager(logger).Reconcile()
	case "firewall.install":
		if err := rememberIPv4ForwardingState(); err != nil {
			return helperResponse{}, err
		}
		firewall := NewFirewall(request.Bridge, request.CIDR, request.NATInterface, logger)
		firewall.SetDNSServers(parseIPs(request.DNS))
		err := firewall.Install()
		if err != nil {
			if cleanupErr := clearForwardingStateIfDisabled(); cleanupErr != nil {
				err = errors.Join(err, cleanupErr)
			}
		}
		return helperResponse{
			ChangedForwarding:      firewall.changedForwarding,
			ForwardingStatePresent: true,
			NATInterface:           firewall.networkInterfaceForRemoval(),
		}, err
	case "firewall.remove":
		active, err := hasActiveBunnTaps(request.Bridge)
		if err != nil {
			return helperResponse{}, err
		}
		if active {
			return helperResponse{
				ChangedForwarding:      request.ChangedForwarding,
				ForwardingStatePresent: true,
				NATInterface:           request.NATInterface,
			}, nil
		}
		firewall := NewFirewall(request.Bridge, request.CIDR, request.NATInterface, logger)
		firewall.SetDNSServers(parseIPs(request.DNS))
		firewall.changedForwarding, err = forwardingStateNeedsRestore()
		if err != nil {
			return helperResponse{}, err
		}
		err = firewall.Remove()
		if err == nil {
			if cleanupErr := removeForwardingState(); cleanupErr != nil {
				err = cleanupErr
			}
		}
		return helperResponse{
			ChangedForwarding:      firewall.changedForwarding,
			ForwardingStatePresent: true,
			NATInterface:           firewall.networkInterfaceForRemoval(),
		}, err
	default:
		return helperResponse{}, fmt.Errorf("unsupported privileged network operation %q", request.Operation)
	}
}

func lockHostNetworking() (*os.File, error) {
	lock, err := os.OpenFile(networkLockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open network helper lock: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return nil, errors.Join(fmt.Errorf("lock host networking: %w", err), lock.Close())
	}
	return lock, nil
}

func unlockHostNetworking(lock *os.File) error {
	return errors.Join(
		syscall.Flock(int(lock.Fd()), syscall.LOCK_UN),
		lock.Close(),
	)
}

func hasActiveBunnTaps(bridgeName string) (bool, error) {
	bridge, err := netlink.LinkByName(bridgeName)
	if err != nil {
		if _, ok := err.(netlink.LinkNotFoundError); ok {
			return false, nil
		}
		return false, fmt.Errorf("lookup Bunn bridge %q: %w", bridgeName, err)
	}
	links, err := netlink.LinkList()
	if err != nil {
		return false, fmt.Errorf("list links on Bunn bridge %q: %w", bridgeName, err)
	}
	for _, link := range links {
		if link.Attrs().MasterIndex == bridge.Attrs().Index &&
			strings.HasPrefix(link.Attrs().Alias, "bunn-burrow:") {
			return true, nil
		}
	}
	return false, nil
}

func rememberIPv4ForwardingState() error {
	value, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	if err != nil {
		return fmt.Errorf("read IPv4 forwarding state: %w", err)
	}
	if strings.TrimSpace(string(value)) != "0" {
		return nil
	}
	if err := os.MkdirAll(networkStateDir, 0o700); err != nil {
		return fmt.Errorf("create network helper state directory: %w", err)
	}
	file, err := os.OpenFile(forwardingStateFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return fmt.Errorf("record original IPv4 forwarding state: %w", err)
	}
	_, writeErr := file.WriteString("0\n")
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return errors.Join(fmt.Errorf("write original IPv4 forwarding state: %w", err), os.Remove(forwardingStateFile))
	}
	return nil
}

func forwardingStateNeedsRestore() (bool, error) {
	value, err := os.ReadFile(forwardingStateFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("read original forwarding state: %w", err)
	}
	if strings.TrimSpace(string(value)) != "0" {
		return false, fmt.Errorf("unexpected original forwarding state %q", strings.TrimSpace(string(value)))
	}
	return true, nil
}

func clearForwardingStateIfDisabled() error {
	value, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	if err != nil {
		return fmt.Errorf("read IPv4 forwarding state after failed install: %w", err)
	}
	if strings.TrimSpace(string(value)) == "0" {
		return removeForwardingState()
	}
	return nil
}

func removeForwardingState() error {
	if err := os.Remove(forwardingStateFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove original forwarding state: %w", err)
	}
	if err := os.Remove(networkStateDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove network helper state directory: %w", err)
	}
	return nil
}

func parseIPs(values []string) []net.IP {
	ips := make([]net.IP, 0, len(values))
	for _, value := range values {
		ips = append(ips, net.ParseIP(value).To4())
	}
	return ips
}

func validateHelperRequest(request helperRequest) error {
	if request.Version != helperProtocol {
		return fmt.Errorf("unsupported network helper protocol version %d", request.Version)
	}
	switch request.Operation {
	case "bridge.ensure":
		if !validBunnBridgeName(request.Bridge) {
			return fmt.Errorf("invalid Bunn bridge name %q", request.Bridge)
		}
		_, subnet, err := net.ParseCIDR(request.CIDR)
		if err != nil || subnet.IP.To4() == nil {
			return fmt.Errorf("invalid IPv4 bridge CIDR %q", request.CIDR)
		}
		gateway := net.ParseIP(request.Gateway).To4()
		if gateway == nil || !subnet.Contains(gateway) {
			return fmt.Errorf("invalid IPv4 gateway %q for %s", request.Gateway, subnet)
		}
	case "bridge.add-port":
		if !validBunnBridgeName(request.Bridge) || !validBunnTapName(request.Tap) {
			return errors.New("invalid Bunn bridge or TAP name")
		}
	case "bridge.delete":
		if !validBunnBridgeName(request.Bridge) {
			return fmt.Errorf("invalid Bunn bridge name %q", request.Bridge)
		}
	case "tap.create":
		if !validBunnTapName(request.Tap) {
			return fmt.Errorf("invalid Bunn TAP name %q", request.Tap)
		}
		mac, err := net.ParseMAC(request.MAC)
		if err != nil || len(mac) != 6 {
			return fmt.Errorf("invalid TAP MAC address %q", request.MAC)
		}
		if request.MTU < 576 || request.MTU > 65535 {
			return fmt.Errorf("invalid TAP MTU %d", request.MTU)
		}
		if request.OwnerPID <= 0 || request.OwnerStartTime == "" {
			return errors.New("TAP owner process identity is required")
		}
		if _, err := strconv.ParseUint(request.OwnerStartTime, 10, 64); err != nil {
			return fmt.Errorf("invalid TAP owner process start time %q", request.OwnerStartTime)
		}
		if request.IP != "" && net.ParseIP(request.IP).To4() == nil {
			return fmt.Errorf("invalid TAP lease IPv4 address %q", request.IP)
		}
	case "tap.delete":
		if !validBunnTapName(request.Tap) {
			return fmt.Errorf("invalid Bunn TAP name %q", request.Tap)
		}
	case "tap.reconcile":
	case "firewall.install", "firewall.remove":
		if !validBunnBridgeName(request.Bridge) {
			return fmt.Errorf("invalid Bunn bridge name %q", request.Bridge)
		}
		_, subnet, err := net.ParseCIDR(request.CIDR)
		if err != nil || subnet.IP.To4() == nil {
			return fmt.Errorf("invalid IPv4 firewall CIDR %q", request.CIDR)
		}
		if request.NATInterface != "" && !validInterfaceName(request.NATInterface) {
			return fmt.Errorf("invalid NAT interface name %q", request.NATInterface)
		}
		for _, server := range request.DNS {
			if net.ParseIP(server).To4() == nil {
				return fmt.Errorf("invalid IPv4 DNS server %q", server)
			}
		}
		if len(request.DNS) > 8 {
			return fmt.Errorf("at most 8 DNS servers may be configured")
		}
	case "":
		return errors.New("privileged network operation is required")
	default:
		return fmt.Errorf("unsupported privileged network operation %q", request.Operation)
	}
	return nil
}

func validBunnBridgeName(name string) bool {
	return validInterfaceName(name) && (name == "br-bunn" || strings.HasPrefix(name, "br-bunn-"))
}

func validBunnTapName(name string) bool {
	return validInterfaceName(name) && strings.HasPrefix(name, "tap-")
}

func helperLogger() *logrus.Entry {
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	return logrus.NewEntry(logger)
}

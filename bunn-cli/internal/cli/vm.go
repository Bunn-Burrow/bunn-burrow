package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/Bunn-Burrow/bunn-cli/internal/cli/helpers"
	"github.com/Bunn-Burrow/bunn-cli/internal/config"
	"github.com/Bunn-Burrow/bunn-burrow/vmm"
)

// vmTracker tracks VMs created within this CLI process.
// VM lifecycle is session-scoped: there is no persistent daemon, so VMs
// created by one `bunn` invocation cannot be managed by another.
type vmTracker struct {
	mu  sync.Mutex
	vms map[string]*vmm.VM
}

func newVMTracker() *vmTracker {
	return &vmTracker{vms: make(map[string]*vmm.VM)}
}

func (t *vmTracker) add(vm *vmm.VM) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.vms[vm.ID()] = vm
}

func (t *vmTracker) get(id string) (*vmm.VM, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	vm, ok := t.vms[id]
	return vm, ok
}

func (t *vmTracker) remove(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.vms, id)
}

func (t *vmTracker) list() []*vmm.VM {
	t.mu.Lock()
	defer t.mu.Unlock()
	vms := make([]*vmm.VM, 0, len(t.vms))
	for _, vm := range t.vms {
		vms = append(vms, vm)
	}
	return vms
}

// globalTracker is the in-process VM registry for this CLI invocation.
var globalTracker = newVMTracker()

// newVMCommand creates the `vm` subcommand group.
// Session-scoped: only VMs created by this CLI invocation can be managed.
func newVMCommand(loader *config.Loader) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vm",
		Short: "Manage VMs (session-scoped)",
		Long: `Create, inspect, stop, and delete VMs within the current CLI session.

Because there is no persistent daemon, VM lifecycle commands only apply to VMs
created by this CLI invocation. Once the CLI exits, VM state is lost.
VMs created by a previous ` + "`bunn`" + ` invocation cannot be stopped or deleted
by a new one.`,
	}

	cmd.AddCommand(newVMRunCommand(loader))
	cmd.AddCommand(newVMStopCommand())
	cmd.AddCommand(newVMDeleteCommand())
	cmd.AddCommand(newVMListCommand())

	return cmd
}

// stopVM stops a VM and returns any error. It checks the VM state first so
// that Stop is not called on a VM that was never successfully started.
func stopVM(vm *vmm.VM, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	state := vm.State()
	if state == vmm.StateCreated || state == vmm.StateFailed || state == vmm.StateStopped || state == vmm.StateDestroyed || state == vmm.StateDestroying {
		// Nothing to stop; VM never started or is already terminal.
		return nil
	}

	if err := vm.Stop(ctx); err != nil {
		return fmt.Errorf("stop VM %q: %w", vm.ID(), err)
	}
	return nil
}

func stopAndDestroyVM(vm *vmm.VM, timeout time.Duration) error {
	stopErr := stopVM(vm, timeout)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	destroyErr := vm.Destroy(ctx)

	return errors.Join(stopErr, destroyErr)
}

// newVMRunCommand creates the `vm run` subcommand.
// `bunn vm run` is a foreground session: it starts a VM, runs until interrupted,
// and reliably cleans up the Firecracker process on exit.
func newVMRunCommand(loader *config.Loader) *cobra.Command {
	var (
		vmID     string
		jsonOut  bool
		kernel   string
		rootfs   string
		workdir  string
		vcpu     int64
		mem      int64
		cid      uint32
		port     uint32
		bin      string
		template string

		networkEnabled  bool
		networkBridge   string
		networkCIDR     string
		networkGateway  string
		networkNATIface string
		networkFirewall bool
		networkMTU      int
		trackedVMID     string
	)

	cmd := &cobra.Command{
		Use:   "run [vm-id]",
		Short: "Start a VM in the foreground (session-scoped)",
		Long: `Start a Firecracker VM and run it in the foreground until interrupted
(Ctrl+C) or terminated (SIGTERM). The VM is stopped automatically when this
command exits.

Kernel, rootfs, and workdir can be set via flags or the BUNN_BURROW_RUNTIME_*
environment variables. Configuration is loaded from ~/.config/bunn-burrow/config.yaml.

LIMITATION: Because there is no persistent daemon, this command is
session-scoped. Once ` + "`bunn vm run`" + ` exits, the VM is stopped and its state
is lost. It cannot be managed by a later ` + "`bunn`" + ` invocation.`,
		Example: `  bunn vm run my-vm --kernel /path/to/vmlinux --rootfs /path/to/rootfs.ext4
  bunn vm run --template my-template`,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := helpers.NewCommandWriter(cmd)

			if len(args) > 0 {
				vmID = args[0]
			}

			vmmCfg, err := helpers.LoadVMMConfig(loader, cmd.Flags(), "workdir", vmID)
			if err != nil {
				return err
			}

			// Apply per-VM overrides that aren't in the config struct.
			if vcpu != 0 {
				vmmCfg.VCPU = vcpu
			}
			if mem != 0 {
				vmmCfg.MemSizeMiB = mem
			}
			if cid != 0 {
				vmmCfg.VsockCID = cid
			}
			if port != 0 {
				vmmCfg.VsockPort = port
			}
			if template != "" {
				vmmCfg.TemplateID = template
			}

			logger := logrus.New()
			logger.SetOutput(os.Stderr)
			logger.Level = logrus.InfoLevel

			vm, err := vmm.New(vmmCfg, logger.WithField("component", "vmm"))
			if err != nil {
				return fmt.Errorf("create VM: %w", err)
			}

			// Track the VM so it can be cleaned up on any exit path.
			globalTracker.add(vm)
			trackedVMID = vm.ID()

			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			// Start the VM. If startup fails, the PersistentPostRunE cleanup
			// checks the VM state before attempting to stop, so Stop is never
			// called on an unsuccessfully started VM.
			if err := vm.Start(ctx); err != nil {
				return fmt.Errorf("start VM: %w", err)
			}

			if jsonOut {
				type runResult struct {
					VMID   string `json:"vm_id"`
					State  string `json:"state"`
					PID    int    `json:"pid"`
					Status string `json:"status"`
				}
				return w.JSON(runResult{
					VMID:   vm.ID(),
					State:  string(vm.State()),
					PID:    vm.PID(),
					Status: "running",
				})
			}

			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", w.Styles().Success.Render(fmt.Sprintf("VM %s running (pid=%d, state=%s)", vm.ID(), vm.PID(), vm.State())))
			fmt.Fprintf(cmd.OutOrStdout(), "Press Ctrl+C to stop the VM.\n")

			// Wait for context cancellation (interrupt/term).
			<-ctx.Done()
			return nil
		},
	}

	// PersistentPostRunE guarantees cleanup runs even if RunE returns an error
	// (e.g., startup failure). It stops and destroys the VM, then removes it
	// from the in-process tracker.
	cmd.PersistentPostRunE = func(cmd *cobra.Command, args []string) error {
		id := trackedVMID
		if id == "" && len(args) > 0 {
			id = args[0]
		}
		if id == "" {
			return nil
		}
		vm, ok := globalTracker.get(id)
		if !ok {
			return nil
		}
		defer globalTracker.remove(id)
		if err := stopAndDestroyVM(vm, 30*time.Second); err != nil {
			return fmt.Errorf("cleanup VM %q: %w", id, err)
		}
		return nil
	}

	cmd.Flags().StringVar(&kernel, "kernel", "", "Kernel image path")
	cmd.Flags().StringVar(&rootfs, "rootfs", "", "Root filesystem image path")
	cmd.Flags().StringVar(&workdir, "workdir", "", "VM working directory")
	cmd.Flags().Int64Var(&vcpu, "vcpu", 0, "vCPU count (1 or even, max 32)")
	cmd.Flags().Int64Var(&mem, "mem", 0, "Memory in MiB")
	cmd.Flags().Uint32Var(&cid, "cid", 0, "Guest vsock CID")
	cmd.Flags().Uint32Var(&port, "port", 0, "Guest vsock port")
	cmd.Flags().StringVar(&bin, "bin", "", "Firecracker binary path")
	cmd.Flags().StringVar(&template, "template", "", "Template ID (uses template rootfs instead of --rootfs)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output in JSON format")
	cmd.Flags().BoolVar(&networkEnabled, "network", false, "Enable VM networking (TAP + bridge + NAT)")
	cmd.Flags().StringVar(&networkBridge, "network-bridge", "", "Bridge name for VM TAPs")
	cmd.Flags().StringVar(&networkCIDR, "network-cidr", "", "CIDR pool for VM IP allocation")
	cmd.Flags().StringVar(&networkGateway, "network-gateway", "", "Gateway IP for VM subnet")
	cmd.Flags().StringVar(&networkNATIface, "network-nat-iface", "", "Host interface for outbound masquerade")
	cmd.Flags().BoolVar(&networkFirewall, "network-firewall", true, "Enable nftables firewall for VM isolation")
	cmd.Flags().IntVar(&networkMTU, "network-mtu", 1500, "MTU for TAP devices")
	return cmd
}

// newVMStopCommand creates the `vm stop` subcommand.
// Session-scoped: only stops VMs created by this CLI invocation.
func newVMStopCommand() *cobra.Command {
	var jsonOut bool

	cmd := &cobra.Command{
		Use:   "stop <vm-id>",
		Short: "Stop a VM (session-scoped)",
		Long: `Stop a VM created by this CLI invocation.

Because there is no persistent daemon, this command can only stop VMs that were
started by the current ` + "`bunn`" + ` invocation. VMs created by a previous
invocation cannot be stopped here.

Sends Ctrl+Alt+Del for graceful shutdown, then escalates to SIGTERM and
SIGKILL on timeout.`,
		Example: `  bunn vm stop my-vm`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			w := helpers.NewCommandWriter(cmd)

			vmID := args[0]
			vm, ok := globalTracker.get(vmID)
			if !ok {
				return fmt.Errorf("VM %q not found in this CLI session", vmID)
			}

			if err := stopVM(vm, 60*time.Second); err != nil {
				return err
			}

			globalTracker.remove(vmID)

			if jsonOut {
				type stopResult struct {
					VMID   string `json:"vm_id"`
					Status string `json:"status"`
				}
				return w.JSON(stopResult{VMID: vmID, Status: "stopped"})
			}

			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", w.Styles().Success.Render(fmt.Sprintf("VM %s stopped", vmID)))
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output in JSON format")
	return cmd
}

// newVMDeleteCommand creates the `vm delete` subcommand.
// Session-scoped: only deletes VMs created by this CLI invocation.
func newVMDeleteCommand() *cobra.Command {
	var jsonOut bool

	cmd := &cobra.Command{
		Use:   "delete <vm-id>",
		Short: "Delete a VM (session-scoped)",
		Long: `Stop (if running) and delete a VM created by this CLI invocation.

Because there is no persistent daemon, this command can only delete VMs that
were started by the current ` + "`bunn`" + ` invocation.

Releases NBD mappings, writable layers, and template references.`,
		Example: `  bunn vm delete my-vm`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			w := helpers.NewCommandWriter(cmd)

			vmID := args[0]
			vm, ok := globalTracker.get(vmID)
			if !ok {
				return fmt.Errorf("VM %q not found in this CLI session", vmID)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			if err := stopVM(vm, 60*time.Second); err != nil {
				return err
			}

			if err := vm.Destroy(ctx); err != nil {
				return fmt.Errorf("delete VM: %w", err)
			}

			globalTracker.remove(vmID)

			if jsonOut {
				type deleteResult struct {
					VMID   string `json:"vm_id"`
					Status string `json:"status"`
				}
				return w.JSON(deleteResult{VMID: vmID, Status: "deleted"})
			}

			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", w.Styles().Success.Render(fmt.Sprintf("VM %s deleted", vmID)))
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output in JSON format")
	return cmd
}

// newVMListCommand creates the `vm list` subcommand.
// Session-scoped: only lists VMs created by this CLI invocation.
func newVMListCommand() *cobra.Command {
	var jsonOut bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List VMs in this CLI session",
		Long: `List VMs created by this CLI invocation.

Because there is no persistent daemon, only VMs started in this session are
shown. VMs created by a previous ` + "`bunn`" + ` invocation are not listed.`,
		Example: `  bunn vm list`,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := helpers.NewCommandWriter(cmd)

			vms := globalTracker.list()

			if jsonOut {
				type vmEntry struct {
					VMID  string `json:"vm_id"`
					State string `json:"state"`
					PID   int    `json:"pid"`
					Dir   string `json:"dir"`
				}
				entries := make([]vmEntry, 0, len(vms))
				for _, vm := range vms {
					entries = append(entries, vmEntry{
						VMID:  vm.ID(),
						State: string(vm.State()),
						PID:   vm.PID(),
						Dir:   vm.Dir(),
					})
				}
				return w.JSON(entries)
			}

			if len(vms) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No VMs found.")
				return nil
			}

			header := []string{"VM ID", "State", "PID", "Dir"}
			rows := make([][]string, 0, len(vms))
			for _, vm := range vms {
				rows = append(rows, []string{vm.ID(), string(vm.State()), fmt.Sprintf("%d", vm.PID()), vm.Dir()})
			}
			return w.Table(header, rows)
		},
	}

	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output in JSON format")
	return cmd
}

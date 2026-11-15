package cli

import (
	"os"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/Bunn-Burrow/bunn-cli/internal/cli/helpers"
	"github.com/Bunn-Burrow/bunn-cli/internal/config"
)

// newRuntimeCommand creates the `runtime` subcommand group.
func newRuntimeCommand(loader *config.Loader) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "runtime",
		Short: "Inspect runtime environment",
		Long: `Inspect the Bunn Burrow runtime environment and available capabilities.

Because there is no persistent daemon, runtime inspection is limited to
host-side capabilities and configuration.`,
	}

	cmd.AddCommand(newRuntimeListCommand())
	cmd.AddCommand(newRuntimeInspectCommand(loader))

	return cmd
}

// newRuntimeListCommand creates the `runtime list` subcommand.
func newRuntimeListCommand() *cobra.Command {
	var jsonOut bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List runtime capabilities",
		Long: `List the runtime capabilities available on this host.

This reports host-side capabilities (Firecracker binary, KVM, qemu-img,
qemu-nbd) rather than VM state, since there is no persistent daemon.`,
		Example: `  bunn runtime list
  bunn runtime list --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := helpers.NewCommandWriter(cmd)

			rows := [][]string{
				{"firecracker", checkBinary("firecracker"), binaryPath("firecracker")},
				{"kvm", checkKVM(), "/dev/kvm"},
				{"qemu-img", checkBinary("qemu-img"), binaryPath("qemu-img")},
				{"qemu-nbd", checkBinary("qemu-nbd"), binaryPath("qemu-nbd")},
			}

			if jsonOut {
				type cap struct {
					Name   string `json:"name"`
					Status string `json:"status"`
					Detail string `json:"detail"`
				}
				caps := make([]cap, 0, len(rows))
				for _, r := range rows {
					caps = append(caps, cap{Name: r[0], Status: r[1], Detail: r[2]})
				}
				return w.JSON(caps)
			}

			header := []string{"Capability", "Status", "Detail"}
			return w.Table(header, rows)
		},
	}

	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output in JSON format")
	return cmd
}

// newRuntimeInspectCommand creates the `runtime inspect` subcommand.
func newRuntimeInspectCommand(loader *config.Loader) *cobra.Command {
	var path string

	cmd := &cobra.Command{
		Use:   "inspect",
		Short: "Inspect runtime configuration",
		Long: `Inspect the resolved runtime configuration from config file,
environment variables, and defaults.`,
		Example: `  bunn runtime inspect`,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := helpers.NewCommandWriter(cmd)

			cfg, err := loader.Load(path)
			if err != nil {
				return err
			}

			pairs := [][2]string{
				{"firecracker_bin", cfg.Runtime.FirecrackerBin},
				{"kernel_path", cfg.Runtime.KernelPath},
				{"rootfs_path", cfg.Runtime.RootFSPath},
				{"work_dir", cfg.Runtime.WorkDir},
				{"start_timeout", cfg.Runtime.StartTimeout.String()},
				{"stop_timeout", cfg.Runtime.StopTimeout.String()},
				{"storage_root_dir", cfg.Storage.RootDir},
				{"storage_cache_dir", cfg.Storage.CacheDir},
				{"log_level", cfg.Logging.Level},
				{"log_format", cfg.Logging.Format},
				{"output_format", cfg.Output.Format},
				{"output_color", cfg.Output.Color},
			}
			return w.KeyValue(pairs)
		},
	}

	cmd.Flags().StringVar(&path, "path", "", "Configuration file path (overrides default)")
	return cmd
}

func checkBinary(name string) string {
	_, err := exec.LookPath(name)
	if err != nil {
		return "not found"
	}
	return "available"
}

func binaryPath(name string) string {
	path, err := exec.LookPath(name)
	if err != nil {
		return "not found in PATH"
	}
	return path
}

func checkKVM() string {
	_, err := os.Stat("/dev/kvm")
	if err != nil {
		return "not available"
	}
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return "no read/write access"
	}
	f.Close()
	return "available"
}

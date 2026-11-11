// Package cli contains the Cobra command definitions for the bunn CLI.
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/Bunn-Burrow/bunn-cli/internal/buildinfo"
	"github.com/Bunn-Burrow/bunn-cli/internal/config"
	"github.com/Bunn-Burrow/bunn-cli/internal/output"
)

// NewRootCommand creates the root cobra command for the bunn CLI.
func NewRootCommand() *cobra.Command {
	loader := config.NewLoader()

	var (
		format string
		color  string
	)

	rootCmd := &cobra.Command{
		Use:   "bunn",
		Short: "bunn - Firecracker microVM sandbox CLI",
		Long: `bunn is the developer-facing command-line interface for the
Bunn Burrow Firecracker microVM sandbox runtime.

It operates the local runtime directly - it is not a Control Plane client.
Use it to manage VMs, templates, snapshots, and inspect runtime health.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       buildinfo.Version,
	}

	rootCmd.PersistentFlags().StringVar(&format, "output", "table", "Output format: table, json, text")
	rootCmd.PersistentFlags().StringVar(&color, "color", "auto", "Color output: auto, always, never")

	// Bind persistent flags to Viper so CLI flags take precedence over
	// environment variables, config file values, and defaults.
	loader.BindPFlag(config.KeyOutputFormat, rootCmd.PersistentFlags().Lookup("output"))
	loader.BindPFlag(config.KeyOutputColor, rootCmd.PersistentFlags().Lookup("color"))

	rootCmd.AddCommand(newVersionCommand())
	rootCmd.AddCommand(newDoctorCommand())
	rootCmd.AddCommand(newConfigCommand(loader))
	rootCmd.AddCommand(newVMCommand(loader))
	rootCmd.AddCommand(newRuntimeCommand(loader))

	rootCmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		// Load config (defaults + config file + env vars + bound flags).
		if _, err := loader.Load(""); err != nil {
			return err
		}

		// Read validated values from Viper (flag > env > config file > default).
		format = loader.Viper().GetString(config.KeyOutputFormat)
		color = loader.Viper().GetString(config.KeyOutputColor)

		f := output.Format(format)
		if !f.Valid() {
			return fmt.Errorf("invalid output format %q: must be table, json, or text", format)
		}
		if color != "auto" && color != "always" && color != "never" {
			return fmt.Errorf("invalid color value %q: must be auto, always, or never", color)
		}
		return nil
	}

	return rootCmd
}

// Execute runs the root command.
func Execute() int {
	cmd := NewRootCommand()
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

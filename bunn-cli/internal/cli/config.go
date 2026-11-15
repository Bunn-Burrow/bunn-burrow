package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Bunn-Burrow/bunn-cli/internal/config"
)

// newConfigCommand creates the `config` subcommand group.
func newConfigCommand(loader *config.Loader) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage CLI configuration",
		Long: `Initialize, view, and resolve the bunn CLI configuration file.

Configuration is stored at ~/.config/bunn-burrow/config.yaml by default.
Precedence: CLI flags > environment variables (BUNN_BURROW_*) > config file > defaults.`,
	}

	cmd.AddCommand(newConfigInitCommand())
	cmd.AddCommand(newConfigViewCommand(loader))
	cmd.AddCommand(newConfigPathCommand())

	return cmd
}

// newConfigInitCommand creates the `config init` subcommand.
func newConfigInitCommand() *cobra.Command {
	var (
		path    string
		force   bool
		jsonOut bool
	)

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize a default configuration file",
		Long: `Create a default configuration file at the default path (~/.config/bunn-burrow/config.yaml)
or at a custom path. Refuses to overwrite an existing file unless --force is set.`,
		Example: `  bunn config init
  bunn config init --path ~/my-config.yaml --force`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := config.Defaults()
			p := config.ConfigPath(path)

			if err := config.Save(cfg, p, force); err != nil {
				return err
			}

			if jsonOut {
				type initResult struct {
					Path   string `json:"path"`
					Status string `json:"status"`
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(initResult{Path: p, Status: "created"})
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Configuration written to %s\n", p)
			return nil
		},
	}

	cmd.Flags().StringVar(&path, "path", "", "Configuration file path (overrides default)")
	cmd.Flags().BoolVar(&force, "force", false, "Overwrite existing configuration file")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output in JSON format")
	return cmd
}

// newConfigViewCommand creates the `config view` subcommand.
func newConfigViewCommand(loader *config.Loader) *cobra.Command {
	var (
		path    string
		jsonOut bool
		redact  bool
	)

	cmd := &cobra.Command{
		Use:   "view",
		Short: "Display the effective configuration",
		Long: `Load and display the effective configuration, showing values from
the config file, environment variables, and defaults.

With --redact, sensitive fields are masked (presence indicated, value hidden).`,
		Example: `  bunn config view
  bunn config view --path ~/my-config.yaml
  bunn config view --redact`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loader.Load(path)
			if err != nil {
				return err
			}

			if jsonOut {
				if redact {
					cfg = config.Redacted(cfg)
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(cfg)
			}

			if redact {
				cfg = config.Redacted(cfg)
			}
			fmt.Fprint(cmd.OutOrStdout(), config.String(cfg))
			return nil
		},
	}

	cmd.Flags().StringVar(&path, "path", "", "Configuration file path (overrides default)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output in JSON format")
	cmd.Flags().BoolVar(&redact, "redact", false, "Redact sensitive fields")
	return cmd
}

// newConfigPathCommand creates the `config path` subcommand.
func newConfigPathCommand() *cobra.Command {
	var path string

	cmd := &cobra.Command{
		Use:   "path",
		Short: "Print the configuration file path",
		Long: `Print the resolved configuration file path.

If --path is given, that path is resolved (including ~ expansion). Otherwise
the default path is printed.`,
		Example: `  bunn config path
  bunn config path --path ~/my-config.yaml`,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), config.ConfigPath(path))
			return nil
		},
	}

	cmd.Flags().StringVar(&path, "path", "", "Configuration file path to resolve")
	return cmd
}

package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Bunn-Burrow/bunn-cli/internal/buildinfo"
	"github.com/Bunn-Burrow/bunn-cli/internal/output"
)

// newVersionCommand creates the `version` subcommand.
func newVersionCommand() *cobra.Command {
	var jsonFlag bool

	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the CLI version",
		Long: `Print the bunn CLI version, Git commit, build date, and Go version.

With --json, output is machine-readable JSON suitable for scripting.`,
		Example: `  bunn version
  bunn version --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := output.NewWriter(output.FormatTable, "never")

			if jsonFlag {
				type versionInfo struct {
					CLI     string `json:"cli"`
					Version string `json:"version"`
					Commit  string `json:"commit,omitempty"`
					Date    string `json:"date,omitempty"`
					Go      string `json:"go"`
				}
				info := versionInfo{
					CLI:     "bunn",
					Version: buildinfo.Version,
					Commit:  buildinfo.Commit,
					Date:    buildinfo.Date,
					Go:      buildinfo.GoVersion(),
				}
				return w.JSON(info)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "bunn version %s\n", buildinfo.Version)
			if buildinfo.Commit != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "  commit: %s\n", buildinfo.Commit)
			}
			if buildinfo.Date != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "  built: %s\n", buildinfo.Date)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "  go: %s\n", buildinfo.GoVersion())
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Output in JSON format")
	return cmd
}

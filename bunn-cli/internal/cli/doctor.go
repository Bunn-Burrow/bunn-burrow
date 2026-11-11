package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Bunn-Burrow/bunn-cli/internal/doctor"
	"github.com/Bunn-Burrow/bunn-cli/internal/output"
)

// newDoctorCommand creates the `doctor` subcommand.
func newDoctorCommand() *cobra.Command {
	var jsonFlag bool

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose runtime health and prerequisites",
		Long: `Inspect Firecracker binary availability, KVM device, qemu-img/qemu-nbd,
kernel and rootfs configuration, storage directories, and host capabilities.

Returns a non-zero exit code if any check fails.`,
		Example: `  bunn doctor
  bunn doctor --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := output.NewWriter(output.FormatTable, "never")

			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()

			runner := doctor.NewRunner()
			result := runner.Run(ctx)

			if jsonFlag {
				return w.JSON(result)
			}

			header := []string{"Check", "Status", "Detail"}
			rows := make([][]string, 0, len(result.Checks))
			for _, c := range result.Checks {
				rows = append(rows, []string{c.Name, output.FormatDoctorStatus(w, c.Status), c.Detail})
			}
			if err := w.Table(header, rows); err != nil {
				return err
			}

			fmt.Fprintln(cmd.OutOrStdout())
			if result.AllPassed {
				fmt.Fprintf(w.ErrWriter(), "%s\n", w.Styles().Success.Render("All checks passed."))
				return nil
			}
			fmt.Fprintf(w.ErrWriter(), "%s\n", w.Styles().Error.Render("One or more checks failed. See above."))
			return fmt.Errorf("doctor: runtime not fully ready")
		},
	}

	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Output in JSON format")
	return cmd
}

var _ = os.Open
var _ = exec.LookPath
var _ = runtime.GOOS
var _ = strings.TrimSpace

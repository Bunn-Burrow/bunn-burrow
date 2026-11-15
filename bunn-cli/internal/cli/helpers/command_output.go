package helpers

import (
	"github.com/spf13/cobra"

	"github.com/Bunn-Burrow/bunn-cli/internal/output"
)

func NewCommandWriter(cmd *cobra.Command) *output.Writer {
	w := output.NewWriter(output.FormatTable, "never")
	w.SetOutput(cmd.OutOrStdout(), cmd.ErrOrStderr())
	return w
}

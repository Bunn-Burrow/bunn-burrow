package helpers

import (
	"bytes"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestNewCommandWriterUsesCommandStreams(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	writer := NewCommandWriter(cmd)
	require.NoError(t, writer.JSON(map[string]string{"status": "ok"}))
	writer.Infof("diagnostic")

	require.Contains(t, stdout.String(), `"status": "ok"`)
	require.Equal(t, "diagnostic\n", stderr.String())
}

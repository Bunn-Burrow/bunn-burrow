package cli

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRuntimeListJSONIncludesCapabilityDetails(t *testing.T) {
	cmd := newRuntimeListCommand()
	out := new(bytes.Buffer)
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs([]string{"--json"})

	require.NoError(t, cmd.Execute())

	var capabilities []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Detail string `json:"detail"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &capabilities))
	require.NotEmpty(t, capabilities)
	for _, capability := range capabilities {
		require.NotEmpty(t, capability.Name)
		require.NotEmpty(t, capability.Status)
		require.NotEmpty(t, capability.Detail)
	}
}

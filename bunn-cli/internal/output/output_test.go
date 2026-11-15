package output

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestJSONOutput verifies that JSON output produces valid JSON.
func TestJSONOutput(t *testing.T) {
	var buf bytes.Buffer
	w := &Writer{
		out:    &buf,
		err:    &buf,
		format: FormatJSON,
		color:  false,
		style:  NewStyles(false),
	}

	type testStruct struct {
		Name  string `json:"name"`
		Value int    `json:"value"`
	}

	err := w.JSON(testStruct{Name: "test", Value: 42})
	require.NoError(t, err)

	output := buf.String()
	require.Contains(t, output, `"name": "test"`)
	require.Contains(t, output, `"value": 42`)

	// Verify no ANSI escape sequences in JSON output
	require.NotContains(t, output, "\x1b")
}

// TestTableOutput verifies that Table output renders aligned columns.
func TestTableOutput(t *testing.T) {
	var buf bytes.Buffer
	w := &Writer{
		out:    &buf,
		err:    &buf,
		format: FormatTable,
		color:  false,
		style:  NewStyles(false),
	}

	header := []string{"Name", "Status"}
	rows := [][]string{
		{"vm1", "running"},
		{"vm2", "stopped"},
	}

	err := w.Table(header, rows)
	require.NoError(t, err)

	output := buf.String()
	require.Contains(t, output, "vm1")
	require.Contains(t, output, "running")
	require.Contains(t, output, "vm2")
	require.Contains(t, output, "stopped")
}

// TestTableOutputEmpty verifies that empty table shows "No items found."
func TestTableOutputEmpty(t *testing.T) {
	var buf bytes.Buffer
	w := &Writer{
		out:    &buf,
		err:    &buf,
		format: FormatTable,
		color:  false,
		style:  NewStyles(false),
	}

	err := w.Table([]string{"Name", "Status"}, [][]string{})
	require.NoError(t, err)

	require.Contains(t, buf.String(), "No items found.")
}

// TestTextOutput verifies that Text output writes plain text.
func TestTextOutput(t *testing.T) {
	var buf bytes.Buffer
	w := &Writer{
		out:    &buf,
		err:    &buf,
		format: FormatText,
		color:  false,
		style:  NewStyles(false),
	}

	err := w.Text("hello %s", "world")
	require.NoError(t, err)

	require.Equal(t, "hello world\n", buf.String())
}

// TestKeyValueOutput verifies KeyValue output format.
func TestKeyValueOutput(t *testing.T) {
	var buf bytes.Buffer
	w := &Writer{
		out:    &buf,
		err:    &buf,
		format: FormatTable,
		color:  false,
		style:  NewStyles(false),
	}

	pairs := [][2]string{
		{"name", "test-vm"},
		{"state", "running"},
	}

	err := w.KeyValue(pairs)
	require.NoError(t, err)

	output := buf.String()
	require.Contains(t, output, "name")
	require.Contains(t, output, "test-vm")
	require.Contains(t, output, "state")
	require.Contains(t, output, "running")
}

// TestWriterColorAutoNoTTY verifies color is disabled in non-TTY.
func TestWriterColorAutoNoTTY(t *testing.T) {
	w := NewWriter(FormatTable, "auto")
	require.False(t, w.color)
}

// TestWriterColorAlways verifies color is enabled when forced.
func TestWriterColorAlways(t *testing.T) {
	w := NewWriter(FormatTable, "always")
	require.True(t, w.color)
}

// TestWriterColorNever verifies color is disabled when disabled.
func TestWriterColorNever(t *testing.T) {
	w := NewWriter(FormatTable, "never")
	require.False(t, w.color)
}

// TestFormatValid verifies Format.Valid().
func TestFormatValid(t *testing.T) {
	require.True(t, FormatTable.Valid())
	require.True(t, FormatJSON.Valid())
	require.True(t, FormatText.Valid())
	require.False(t, Format("invalid").Valid())
}

// TestStringsContainNoANSI verifies that no output contains ANSI escapes.
func TestStringsContainNoANSI(t *testing.T) {
	var buf bytes.Buffer
	w := &Writer{
		out:    &buf,
		err:    &buf,
		format: FormatTable,
		color:  true,
		style:  NewStyles(true),
	}

	err := w.Text("hello %s", "world")
	require.NoError(t, err)

	// When color is true but output is not a TTY (we construct directly),
	// the styles still have ANSI codes but they won't be applied to plain
	// Text output since Text doesn't use styles.
	require.NotContains(t, buf.String(), "\x1b")
}

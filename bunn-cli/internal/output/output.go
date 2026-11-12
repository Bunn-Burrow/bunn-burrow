// Package output provides formatted terminal output for the bunn CLI.
package output

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/charmbracelet/lipgloss"
)

// Format selects the output rendering.
type Format string

const (
	FormatTable Format = "table"
	FormatJSON  Format = "json"
	FormatText  Format = "text"
)

// Valid reports whether the format is supported.
func (f Format) Valid() bool {
	switch f {
	case FormatTable, FormatJSON, FormatText:
		return true
	}
	return false
}

// String returns the canonical format name.
func (f Format) String() string { return string(f) }

// Writer writes formatted output to stdout and reserves stderr for diagnostics.
type Writer struct {
	out    io.Writer
	err    io.Writer
	format Format
	color  bool
	style  *Styles
}

// Styles holds centralized Lipgloss styles for consistent terminal output.
type Styles struct {
	Heading lipgloss.Style
	Success lipgloss.Style
	Warning lipgloss.Style
	Error   lipgloss.Style
	Info    lipgloss.Style
	Header  lipgloss.Style
	Cell    lipgloss.Style
	Value   lipgloss.Style
	Key     lipgloss.Style
	Muted   lipgloss.Style
	Bold    lipgloss.Style
	Code    lipgloss.Style
}

// NewWriter creates a Writer for the given format.
func NewWriter(format Format, color string) *Writer {
	useColor := false
	if color == "auto" {
		useColor = isTerminal(os.Stdout) && os.Getenv("NO_COLOR") == ""
	} else if color == "always" {
		useColor = true
	}

	style := NewStyles(useColor)

	return &Writer{
		out:    os.Stdout,
		err:    os.Stderr,
		format: format,
		color:  useColor,
		style:  style,
	}
}

// SetOutput configures custom destination writers for standard and error output.
func (w *Writer) SetOutput(out, err io.Writer) {
	if out != nil {
		w.out = out
	}
	if err != nil {
		w.err = err
	}
}

// NewStyles creates a style set.
func NewStyles(useColor bool) *Styles {
	if !useColor {
		return &Styles{}
	}

	return &Styles{
		Heading: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("12")),
		Success: lipgloss.NewStyle().
			Foreground(lipgloss.Color("10")),
		Warning: lipgloss.NewStyle().
			Foreground(lipgloss.Color("11")),
		Error: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("9")),
		Info: lipgloss.NewStyle().
			Foreground(lipgloss.Color("14")),
		Header: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("12")),
		Cell: lipgloss.NewStyle().
			Foreground(lipgloss.Color("15")),
		Value: lipgloss.NewStyle().
			Foreground(lipgloss.Color("10")),
		Key: lipgloss.NewStyle().
			Foreground(lipgloss.Color("12")),
		Muted: lipgloss.NewStyle().
			Foreground(lipgloss.Color("8")),
		Bold: lipgloss.NewStyle().
			Bold(true),
		Code: lipgloss.NewStyle().
			Foreground(lipgloss.Color("11")).
			Background(lipgloss.Color("235")),
	}
}

// ErrWriter returns the writer reserved for diagnostics (stderr by default).
func (w *Writer) ErrWriter() io.Writer {
	return w.err
}

// Styles returns the current style set.
func (w *Writer) Styles() *Styles {
	return w.style
}

// Fatalf writes a diagnostic to stderr and returns an error.
func (w *Writer) Fatalf(format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if w.color {
		msg = w.style.Error.Render(msg)
	}
	fmt.Fprintln(w.err, msg)
	return fmt.Errorf("%s", msg)
}

// Errorf writes an error diagnostic to stderr.
func (w *Writer) Errorf(format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if w.color {
		msg = w.style.Error.Render("error: " + msg)
	} else {
		msg = "error: " + msg
	}
	fmt.Fprintln(w.err, msg)
	return fmt.Errorf("%s", msg)
}

// Warningf writes a warning diagnostic to stderr.
func (w *Writer) Warningf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if w.color {
		msg = w.style.Warning.Render("warning: " + msg)
	} else {
		msg = "warning: " + msg
	}
	fmt.Fprintln(w.err, msg)
}

// Infof writes an informational message to stderr.
func (w *Writer) Infof(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if w.color {
		msg = w.style.Info.Render(msg)
	}
	fmt.Fprintln(w.err, msg)
}

// JSON marshals v and writes it to stdout with a trailing newline.
func (w *Writer) JSON(v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal output: %w", err)
	}
	_, err = fmt.Fprintln(w.out, string(data))
	return err
}

// Text writes a plain text line to stdout.
func (w *Writer) Text(format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	_, err := fmt.Fprintln(w.out, msg)
	return err
}

// Table renders rows of string columns with aligned columns.
func (w *Writer) Table(header []string, rows [][]string) error {
	if len(rows) == 0 {
		fmt.Fprintln(w.out, "No items found.")
		return nil
	}
	wt := tabwriter.NewWriter(w.out, 0, 0, 2, ' ', 0)
	if len(header) > 0 {
		for i, h := range header {
			if i > 0 {
				fmt.Fprint(wt, "\t")
			}
			fmt.Fprint(wt, h)
		}
		fmt.Fprintln(wt)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i > 0 {
				fmt.Fprint(wt, "\t")
			}
			fmt.Fprint(wt, cell)
		}
		fmt.Fprintln(wt)
	}
	return wt.Flush()
}

// KeyValue renders key-value pairs.
func (w *Writer) KeyValue(pairs [][2]string) error {
	maxKey := 0
	for _, p := range pairs {
		if len(p[0]) > maxKey {
			maxKey = len(p[0])
		}
	}
	for _, p := range pairs {
		key := p[0]
		if w.color {
			key = w.style.Key.Render(key)
		}
		fmt.Fprintf(w.out, "%s%s %s\n", key, strings.Repeat(" ", maxKey-len(p[0])+1), p[1])
	}
	return nil
}

// Successf writes a success message to stdout.
func (w *Writer) Successf(format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if w.color {
		msg = w.style.Success.Render(msg)
	}
	_, err := fmt.Fprintln(w.out, msg)
	return err
}

// isTerminal reports whether the given file is a terminal.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// FormatDoctorStatus returns a styled status string for doctor checks.
func FormatDoctorStatus(w *Writer, status string) string {
	switch status {
	case "pass":
		if w.color {
			return w.style.Success.Render("PASS")
		}
		return "PASS"
	case "warn":
		if w.color {
			return w.style.Warning.Render("WARN")
		}
		return "WARN"
	case "fail":
		if w.color {
			return w.style.Error.Render("FAIL")
		}
		return "FAIL"
	case "skip":
		if w.color {
			return w.style.Muted.Render("SKIP")
		}
		return "SKIP"
	default:
		return status
	}
}

// Package output provides output formatting utilities for CLI commands.
package output

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// Format represents the output format type.
type Format string

const (
	// FormatTable outputs data in a formatted table.
	FormatTable Format = "table"
	// FormatJSON outputs data as JSON.
	FormatJSON Format = "json"
	// FormatYAML outputs data as YAML.
	FormatYAML Format = "yaml"
)

// ParseFormat parses a string into a Format, returning an error if invalid.
func ParseFormat(s string) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "table", "":
		return FormatTable, nil
	case "json":
		return FormatJSON, nil
	case "yaml", "yml":
		return FormatYAML, nil
	default:
		return "", fmt.Errorf("invalid output format: %q (valid: table, json, yaml)", s)
	}
}

// String returns the string representation of the format.
func (f Format) String() string {
	return string(f)
}

// Printer handles formatted output to a writer.
type Printer struct {
	out    io.Writer
	format Format
	color  bool
}

// NewPrinter creates a new Printer with the given options.
func NewPrinter(out io.Writer, format Format, color bool) *Printer {
	return &Printer{
		out:    out,
		format: format,
		color:  color,
	}
}

// DefaultPrinter creates a Printer that writes to stdout with table format.
func DefaultPrinter() *Printer {
	return NewPrinter(os.Stdout, FormatTable, true)
}

// Format returns the printer's output format.
func (p *Printer) Format() Format {
	return p.format
}

// Writer returns the printer's output writer.
func (p *Printer) Writer() io.Writer {
	return p.out
}

// ColorEnabled returns whether color output is enabled.
func (p *Printer) ColorEnabled() bool {
	return p.color
}

// Print outputs data in the configured format.
// For table format, data should implement TableRenderer.
// For JSON/YAML, data will be marshaled directly.
func (p *Printer) Print(data any) error {
	switch p.format {
	case FormatTable:
		if renderer, ok := data.(TableRenderer); ok {
			return PrintTable(p.out, renderer)
		}
		// Fallback to JSON if data doesn't implement TableRenderer
		return PrintJSON(p.out, data)
	case FormatJSON:
		return PrintJSON(p.out, data)
	case FormatYAML:
		return PrintYAML(p.out, data)
	default:
		return fmt.Errorf("unknown format: %s", p.format)
	}
}

// Println prints a message followed by a newline.
func (p *Printer) Println(args ...any) {
	_, _ = fmt.Fprintln(p.out, args...)
}

// Printf prints a formatted message.
func (p *Printer) Printf(format string, args ...any) {
	_, _ = fmt.Fprintf(p.out, format, args...)
}

// Success prints a success message.
func (p *Printer) Success(msg string) {
	if p.color {
		_, _ = fmt.Fprintf(p.out, "\033[32m%s\033[0m\n", msg)
	} else {
		_, _ = fmt.Fprintln(p.out, msg)
	}
}

// Error prints an error message.
func (p *Printer) Error(msg string) {
	if p.color {
		_, _ = fmt.Fprintf(p.out, "\033[31m%s\033[0m\n", msg)
	} else {
		_, _ = fmt.Fprintln(p.out, msg)
	}
}

// Warning prints a warning message.
func (p *Printer) Warning(msg string) {
	if p.color {
		_, _ = fmt.Fprintf(p.out, "\033[33m%s\033[0m\n", msg)
	} else {
		_, _ = fmt.Fprintln(p.out, msg)
	}
}

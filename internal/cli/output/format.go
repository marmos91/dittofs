// Package output provides output formatting utilities for CLI commands.
package output

import (
	"fmt"
	"io"
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

// Printer writes human-readable status messages to a writer, colourised when
// enabled. Structured output does not go through it: commands emit JSON/YAML
// with PrintJSON/PrintYAML directly, so the only thing a Printer formats is a
// one-line success message.
type Printer struct {
	out   io.Writer
	color bool
}

// NewPrinter creates a new Printer with the given options.
func NewPrinter(out io.Writer, color bool) *Printer {
	return &Printer{out: out, color: color}
}

// Success prints a success message.
func (p *Printer) Success(msg string) {
	if p.color {
		_, _ = fmt.Fprintf(p.out, "\033[32m%s\033[0m\n", msg)
	} else {
		_, _ = fmt.Fprintln(p.out, msg)
	}
}

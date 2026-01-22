package output

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseFormat(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    Format
		wantErr bool
	}{
		{name: "table", input: "table", want: FormatTable},
		{name: "empty defaults to table", input: "", want: FormatTable},
		{name: "json", input: "json", want: FormatJSON},
		{name: "JSON uppercase", input: "JSON", want: FormatJSON},
		{name: "yaml", input: "yaml", want: FormatYAML},
		{name: "yml alias", input: "yml", want: FormatYAML},
		{name: "whitespace trimmed", input: "  table  ", want: FormatTable},
		{name: "invalid format", input: "xml", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseFormat(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestFormatString(t *testing.T) {
	assert.Equal(t, "table", FormatTable.String())
	assert.Equal(t, "json", FormatJSON.String())
	assert.Equal(t, "yaml", FormatYAML.String())
}

func TestPrinter(t *testing.T) {
	var buf bytes.Buffer
	printer := NewPrinter(&buf, FormatTable, true)

	assert.Equal(t, FormatTable, printer.Format())
	assert.True(t, printer.ColorEnabled())

	printer.Println("test message")
	assert.Contains(t, buf.String(), "test message")
}

func TestPrinterSuccess(t *testing.T) {
	var buf bytes.Buffer
	printer := NewPrinter(&buf, FormatTable, false)

	printer.Success("success message")
	assert.Contains(t, buf.String(), "success message")
}

func TestPrinterError(t *testing.T) {
	var buf bytes.Buffer
	printer := NewPrinter(&buf, FormatTable, false)

	printer.Error("error message")
	assert.Contains(t, buf.String(), "error message")
}

func TestPrinterWarning(t *testing.T) {
	var buf bytes.Buffer
	printer := NewPrinter(&buf, FormatTable, false)

	printer.Warning("warning message")
	assert.Contains(t, buf.String(), "warning message")
}

func TestDefaultPrinter(t *testing.T) {
	printer := DefaultPrinter()
	assert.NotNil(t, printer)
	assert.Equal(t, FormatTable, printer.Format())
	assert.True(t, printer.ColorEnabled())
}

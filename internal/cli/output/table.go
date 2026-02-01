package output

import (
	"io"

	"github.com/olekukonko/tablewriter"
)

// TableRenderer is implemented by types that can render themselves as a table.
type TableRenderer interface {
	// Headers returns the column headers for the table.
	Headers() []string
	// Rows returns the data rows for the table.
	Rows() [][]string
}

// PrintTable writes data as a formatted table to the writer.
func PrintTable(w io.Writer, data TableRenderer) error {
	table := tablewriter.NewWriter(w)
	table.SetHeader(data.Headers())

	// Configure table style for clean output
	table.SetAutoWrapText(false)
	table.SetAutoFormatHeaders(true)
	table.SetHeaderAlignment(tablewriter.ALIGN_LEFT)
	table.SetAlignment(tablewriter.ALIGN_LEFT)
	table.SetCenterSeparator("")
	table.SetColumnSeparator("")
	table.SetRowSeparator("")
	table.SetHeaderLine(false)
	table.SetBorder(false)
	table.SetTablePadding("  ")
	table.SetNoWhiteSpace(true)

	for _, row := range data.Rows() {
		table.Append(row)
	}

	table.Render()
	return nil
}

// TableData is a simple implementation of TableRenderer for ad-hoc tables.
type TableData struct {
	headers []string
	rows    [][]string
}

// NewTableData creates a new TableData with the given headers.
func NewTableData(headers ...string) *TableData {
	return &TableData{
		headers: headers,
		rows:    make([][]string, 0),
	}
}

// AddRow adds a row to the table.
func (t *TableData) AddRow(row ...string) {
	t.rows = append(t.rows, row)
}

// Headers implements TableRenderer.
func (t *TableData) Headers() []string {
	return t.headers
}

// Rows implements TableRenderer.
func (t *TableData) Rows() [][]string {
	return t.rows
}

// SimpleTable prints a simple key-value table.
func SimpleTable(w io.Writer, pairs [][2]string) error {
	table := tablewriter.NewWriter(w)

	table.SetAutoWrapText(false)
	table.SetAutoFormatHeaders(false)
	table.SetHeaderAlignment(tablewriter.ALIGN_LEFT)
	table.SetAlignment(tablewriter.ALIGN_LEFT)
	table.SetCenterSeparator("")
	table.SetColumnSeparator(":")
	table.SetRowSeparator("")
	table.SetHeaderLine(false)
	table.SetBorder(false)
	table.SetTablePadding("  ")
	table.SetNoWhiteSpace(true)

	for _, pair := range pairs {
		table.Append([]string{pair[0], pair[1]})
	}

	table.Render()
	return nil
}

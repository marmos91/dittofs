package output

import (
	"errors"
	"io"

	"github.com/olekukonko/tablewriter"
	"github.com/olekukonko/tablewriter/tw"
)

// TableRenderer is implemented by types that can render themselves as a table.
type TableRenderer interface {
	// Headers returns the column headers for the table.
	Headers() []string
	// Rows returns the data rows for the table.
	Rows() [][]string
}

// borderless returns the options for the CLI's plain column layout: no borders,
// no separators and no header rule, every cell left-aligned and never wrapped,
// with two trailing spaces per cell standing in for the gap between columns.
func borderless() []tablewriter.Option {
	return []tablewriter.Option{
		tablewriter.WithRendition(tw.Rendition{
			Borders: tw.BorderNone,
			Settings: tw.Settings{
				Separators: tw.Separators{
					ShowHeader:     tw.Off,
					ShowFooter:     tw.Off,
					BetweenRows:    tw.Off,
					BetweenColumns: tw.Off,
				},
				Lines: tw.Lines{
					ShowTop:        tw.Off,
					ShowBottom:     tw.Off,
					ShowHeaderLine: tw.Off,
					ShowFooterLine: tw.Off,
				},
			},
		}),
		tablewriter.WithPadding(tw.Padding{Left: tw.Empty, Right: "  ", Overwrite: true}),
		tablewriter.WithHeaderAlignment(tw.AlignLeft),
		tablewriter.WithAlignment(tw.Alignment{tw.AlignLeft}),
		tablewriter.WithHeaderAutoWrap(tw.WrapNone),
		tablewriter.WithRowAutoWrap(tw.WrapNone),
	}
}

// PrintTable writes data as a formatted table to the writer.
//
// A nil renderer is a bug in the calling command, but it must not take the
// process down: reaching Headers() through a nil interface panics, so a `show`
// command that forgot its renderer crashes the CLI with a stack trace instead of
// printing anything. Report it as an error and let the command surface it like
// any other failure.
func PrintTable(w io.Writer, data TableRenderer) error {
	if data == nil {
		return errors.New("no table renderer for this resource; retry with -o json")
	}
	opts := append(borderless(), tablewriter.WithHeaderAutoFormat(tw.On))
	table := tablewriter.NewTable(w, opts...)
	table.Header(data.Headers())

	for _, row := range data.Rows() {
		if err := table.Append(row); err != nil {
			return err
		}
	}

	return table.Render()
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
	table := tablewriter.NewTable(w, borderless()...)

	for _, pair := range pairs {
		if err := table.Append([]string{pair[0], pair[1]}); err != nil {
			return err
		}
	}

	return table.Render()
}

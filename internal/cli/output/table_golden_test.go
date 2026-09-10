package output

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The CLI's table layout is a promise to anyone reading or scripting dfsctl
// output, and it is produced entirely by tablewriter configuration rather than
// by code here — so a library upgrade can silently restyle every command. These
// golden strings pin the layout byte for byte, trailing spaces included.

func TestPrintTableGoldenLayout(t *testing.T) {
	table := NewTableData("name", "share path", "enabled")
	table.AddRow("/alice", "/srv/exports/alice", "yes")
	table.AddRow("/archive-long-name", "/x", "-")
	table.AddRow("", "empty first cell", "yes")

	var buf bytes.Buffer
	require.NoError(t, PrintTable(&buf, table))

	const want = "NAME                SHARE PATH          ENABLED  \n" +
		"/alice              /srv/exports/alice  yes      \n" +
		"/archive-long-name  /x                  -        \n" +
		"                    empty first cell    yes      \n"
	assert.Equal(t, want, buf.String())
}

// Long cells must run past the terminal width rather than wrap: a wrapped cell
// breaks column alignment and anything grepping the output line by line.
func TestPrintTableGoldenNoWrap(t *testing.T) {
	table := NewTableData("K", "V")
	table.AddRow("desc", "a value far longer than any default wrap width, which must stay on one line")

	var buf bytes.Buffer
	require.NoError(t, PrintTable(&buf, table))

	const want = "K     V                                                                            \n" +
		"desc  a value far longer than any default wrap width, which must stay on one line  \n"
	assert.Equal(t, want, buf.String())
}

func TestSimpleTableGoldenLayout(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, SimpleTable(&buf, [][2]string{
		{"Name", "/alice"},
		{"Backing Store", "s3://bucket/prefix"},
		{"Empty", ""},
	}))

	const want = "Name           /alice              \n" +
		"Backing Store  s3://bucket/prefix  \n" +
		"Empty                              \n"
	assert.Equal(t, want, buf.String())
}

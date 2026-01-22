package output

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTableData(t *testing.T) {
	table := NewTableData("Name", "Age", "City")

	assert.Equal(t, []string{"Name", "Age", "City"}, table.Headers())
	assert.Empty(t, table.Rows())

	table.AddRow("Alice", "30", "NYC")
	table.AddRow("Bob", "25", "LA")

	rows := table.Rows()
	require.Len(t, rows, 2)
	assert.Equal(t, []string{"Alice", "30", "NYC"}, rows[0])
	assert.Equal(t, []string{"Bob", "25", "LA"}, rows[1])
}

func TestPrintTable(t *testing.T) {
	table := NewTableData("Name", "Value")
	table.AddRow("key1", "value1")
	table.AddRow("key2", "value2")

	var buf bytes.Buffer
	err := PrintTable(&buf, table)
	require.NoError(t, err)

	output := buf.String()
	assert.Contains(t, output, "NAME")
	assert.Contains(t, output, "VALUE")
	assert.Contains(t, output, "key1")
	assert.Contains(t, output, "value1")
	assert.Contains(t, output, "key2")
	assert.Contains(t, output, "value2")
}

func TestSimpleTable(t *testing.T) {
	pairs := [][2]string{
		{"Key1", "Value1"},
		{"Key2", "Value2"},
	}

	var buf bytes.Buffer
	err := SimpleTable(&buf, pairs)
	require.NoError(t, err)

	output := buf.String()
	assert.Contains(t, output, "Key1")
	assert.Contains(t, output, "Value1")
	assert.Contains(t, output, "Key2")
	assert.Contains(t, output, "Value2")
}

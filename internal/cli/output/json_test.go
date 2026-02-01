package output

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testStruct struct {
	Name  string `json:"name"`
	Value int    `json:"value"`
}

func TestPrintJSON(t *testing.T) {
	data := testStruct{Name: "test", Value: 42}

	var buf bytes.Buffer
	err := PrintJSON(&buf, data)
	require.NoError(t, err)

	output := buf.String()
	assert.Contains(t, output, `"name": "test"`)
	assert.Contains(t, output, `"value": 42`)
}

func TestPrintJSONCompact(t *testing.T) {
	data := testStruct{Name: "test", Value: 42}

	var buf bytes.Buffer
	err := PrintJSONCompact(&buf, data)
	require.NoError(t, err)

	output := buf.String()
	// Compact JSON should not have extra indentation
	assert.Contains(t, output, `"name":"test"`)
	assert.Contains(t, output, `"value":42`)
}

func TestPrintJSONArray(t *testing.T) {
	data := []testStruct{
		{Name: "a", Value: 1},
		{Name: "b", Value: 2},
	}

	var buf bytes.Buffer
	err := PrintJSON(&buf, data)
	require.NoError(t, err)

	output := buf.String()
	assert.Contains(t, output, `"name": "a"`)
	assert.Contains(t, output, `"name": "b"`)
}

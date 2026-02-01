package output

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrintYAML(t *testing.T) {
	data := struct {
		Name  string `yaml:"name"`
		Value int    `yaml:"value"`
	}{
		Name:  "test",
		Value: 42,
	}

	var buf bytes.Buffer
	err := PrintYAML(&buf, data)
	require.NoError(t, err)

	output := buf.String()
	assert.Contains(t, output, "name: test")
	assert.Contains(t, output, "value: 42")
}

func TestPrintYAMLArray(t *testing.T) {
	data := []struct {
		Name string `yaml:"name"`
	}{
		{Name: "a"},
		{Name: "b"},
	}

	var buf bytes.Buffer
	err := PrintYAML(&buf, data)
	require.NoError(t, err)

	output := buf.String()
	assert.Contains(t, output, "- name: a")
	assert.Contains(t, output, "- name: b")
}

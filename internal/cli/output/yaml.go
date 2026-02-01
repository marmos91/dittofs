package output

import (
	"io"

	"gopkg.in/yaml.v3"
)

// PrintYAML writes data as YAML to the writer.
func PrintYAML(w io.Writer, data any) error {
	encoder := yaml.NewEncoder(w)
	encoder.SetIndent(2)
	defer func() { _ = encoder.Close() }()
	return encoder.Encode(data)
}

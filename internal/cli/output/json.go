package output

import (
	"encoding/json"
	"io"
)

// PrintJSON writes data as formatted JSON to the writer.
func PrintJSON(w io.Writer, data any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(data)
}

// PrintJSONCompact writes data as compact JSON to the writer.
func PrintJSONCompact(w io.Writer, data any) error {
	encoder := json.NewEncoder(w)
	return encoder.Encode(data)
}

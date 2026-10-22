package known

import (
	"bytes"
	"os"

	"gopkg.in/yaml.v3"
)

// Prune removes from the known file at path each entry keep refuses, and
// returns them. The rest of the file stays as it was, comments included: the
// comment above a removed entry, which heads the entries after it, moves to
// the next entry kept, unless that one has its own. A file with nothing to
// remove isn't written.
func Prune(path string, keep func(Entry) bool) ([]Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	entries, err := Parse(path, data)
	if err != nil || len(entries) == 0 {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	list := doc.Content[0]
	var kept []*yaml.Node
	var removed []Entry
	comment := ""
	for i, n := range list.Content {
		if keep(entries[i]) {
			if n.HeadComment == "" {
				n.HeadComment = comment
			}
			comment = ""
			kept = append(kept, n)
			continue
		}
		if n.HeadComment != "" {
			comment = n.HeadComment
		}
		removed = append(removed, entries[i])
	}
	if len(removed) == 0 {
		return nil, nil
	}
	list.Content = kept
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return removed, os.WriteFile(path, out.Bytes(), 0o644)
}

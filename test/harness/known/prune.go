package known

import (
	"bytes"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/marmos91/dittofs/test/harness/report"
)

// Result is what a test did in a cell of a past run, passed, failed, skipped
// or other, before it was graded, and where the cell ran: its option, matrix
// values, tier and OS, as an entry's where names them.
type Result struct {
	Name, Outcome string
	At            map[string]string
}

// Prune removes from the known file at path each entry the results show
// stale, and returns them. The rest of the file stays as it was, comments
// included: the comment above a removed entry, which heads the entries after
// it, moves to the next entry kept, unless that one has its own. A file with
// nothing to remove isn't written.
func Prune(path string, results []Result) ([]Entry, error) {
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
		if !entries[i].stale(results) {
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

// stale reports whether the results show the entry no longer holds, judged
// on those that matched its test in cells it applies to: a known failure
// one of them passed and none failed, or an expected skip one of them ran,
// passing or failing, and none skipped. Any other result shows nothing, so
// an entry stays unless its test ran where it applies and did otherwise.
func (e Entry) stale(results []Result) bool {
	// decision: an intermittent entry is never removed: its test passes on
	// some runs by definition, so no window of runs shows it stale. Remove
	// it by hand once its cause is fixed.
	if e.Intermittent {
		return false
	}
	holds := map[string]string{"fail": report.Failed, "skip": report.Skipped}[e.Status]
	shown := false
	for _, r := range results {
		if !e.applies(r.At) || !Match(e.Test, r.Name) {
			continue
		}
		switch r.Outcome {
		case holds:
			return false
		case report.Passed, report.Failed:
			shown = true // it ran: for a known failure, that it passed
		}
	}
	return shown
}

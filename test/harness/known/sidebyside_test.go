package known

import (
	"fmt"
	"os"
	"slices"
	"testing"
)

// sideBySide are the Markdown lists the scripts grade with, each with the
// known file dt reads in its place and the values of the matrix axis the
// list was imported with, if any.
var sideBySide = []struct {
	md, known, axis string
	values          []string
}{
	{"../../posix/KNOWN_FAILURES.md", "../../posix/known.yaml", "nfs", []string{"3"}},
	{"../../posix/KNOWN_FAILURES_V4.md", "../../posix/known.yaml", "nfs", []string{"4", "4.1"}},
	{"../../smb-conformance/KNOWN_FAILURES.md", "../../smb-conformance/known.yaml", "", nil},
	{"../../smb-conformance/smbtorture/KNOWN_FAILURES.md", "../../smb-conformance/smbtorture/known.yaml", "", nil},
	{"../../smb-conformance/smbtorture/KNOWN_FAILURES_KERBEROS.md", "../../smb-conformance/smbtorture/known-kerberos.yaml", "", nil},
}

// Until the scripts go, a change to a Markdown list or its known file is
// made to both: each row of a table of tests has an entry that applies where
// the list does, and each known failure that applies there has a row. An
// expected skip needs no row.
func TestMarkdownListsMatchKnownFiles(t *testing.T) {
	for _, c := range sideBySide {
		var l List
		if err := l.Load(c.md); err != nil {
			t.Fatal(err)
		}
		var rows []string
		for _, r := range l.Rows() {
			if r.NamesTest {
				rows = append(rows, r.Name)
			}
		}
		data, err := os.ReadFile(c.known)
		if err != nil {
			t.Fatal(err)
		}
		entries, err := Parse(c.known, data)
		if err != nil {
			t.Fatal(err)
		}
		values := c.values
		if c.axis == "" {
			values = []string{""} // every entry applies where the list does
		}
		for _, v := range values {
			at := ""
			if c.axis != "" {
				at = fmt.Sprintf(" where %s is %s", c.axis, v)
			}
			var listed, fails []string
			for _, e := range entries {
				if e.Where[c.axis] == nil || slices.Contains(e.Where[c.axis], v) {
					listed = append(listed, e.Test)
					if e.Status == "fail" {
						fails = append(fails, e.Test)
					}
				}
			}
			for _, name := range rows {
				if !slices.Contains(listed, name) {
					t.Errorf("%s: row %s has no entry%s in %s", c.md, name, at, c.known)
				}
			}
			for _, name := range fails {
				if !slices.Contains(rows, name) {
					t.Errorf("%s: known failure %s%s has no row in %s", c.known, name, at, c.md)
				}
			}
		}
	}
}

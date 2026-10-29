package known

import (
	"bytes"
	"errors"
	"io/fs"
	"maps"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// upstream are the smbtorture rows outside an appendix whose tests fail
// against Samba too: the lists' policy gave them an issue, but they won't be
// fixed.
var upstream = []string{"smb2.charset.Testing", "smb2.session.reauth5", "smb2.reauth5"}

var issueRef = regexp.MustCompile(`#[0-9]+`)

// Import adds the rows of the Markdown list at md to the known file at file,
// as known failures that apply where says, and writes the file. An entry
// that differs from one the file holds only in the values where gives its
// one key merges into it, as the lists per version do. The file is written
// only if every entry is valid. It returns the names of the rows left out,
// those outside a table of tests, such as a changelog's or a legend's.
func Import(md, file string, where map[string][]string) ([]string, error) {
	text, err := os.ReadFile(md)
	if err != nil {
		return nil, err
	}
	var l List
	l.parse(string(text))
	data, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	entries, err := Parse(file, data)
	if err != nil {
		return nil, err
	}
	var left []string
	for _, r := range l.Rows() {
		if !r.NamesTest {
			left = append(left, r.Name)
			continue
		}
		entries = merge(entries, entryOf(r, where))
	}
	out, err := marshal(entries)
	if err != nil {
		return left, err
	}
	if _, err := Parse(file, out); err != nil {
		return left, err
	}
	return left, os.WriteFile(file, out, 0o644)
}

// entryOf maps a row of a table of tests to an entry: its category says
// whether it will be fixed, and a row in a list's "Permanently
// Unimplementable" appendix never will. Any other category, smbtorture's
// free-text ones above its appendix included, is a bug, planned, with the
// row's issue.
//
// decision: the issue is read from the Issue column, the row's fifth field,
// only. WPTS's format for a row above its appendix puts it in the sixth,
// after a status column; no row uses that format, so such a row is refused
// for want of an issue rather than read.
func entryOf(r Row, where map[string][]string) Entry {
	e := Entry{Test: r.Name, Status: "fail", Fix: "wontfix", Reason: r.Reason, Issue: issueRef.FindString(r.Issue), Where: where}
	switch {
	case strings.HasPrefix(r.Heading, "Permanently Unimplementable"):
		e.Category = "permanent"
	case slices.Contains(upstream, r.Name), r.Category == "suite":
		e.Category = "proto"
	case slices.Contains([]string{"feature", "proto", "env"}, r.Category):
		e.Category = r.Category
	default:
		e.Category, e.Fix = "bug", "planned"
	}
	return e
}

// merge appends e to entries, or adds the values of e's where, which has
// one key at most, to an entry that is the same but for those values.
func merge(entries []Entry, e Entry) []Entry {
	bare := func(e Entry) Entry {
		e.Where, e.Line = nil, 0
		return e
	}
	for i := range entries {
		old := &entries[i]
		if !reflect.DeepEqual(bare(*old), bare(e)) || !slices.Equal(slices.Sorted(maps.Keys(old.Where)), slices.Sorted(maps.Keys(e.Where))) {
			continue
		}
		for k, values := range e.Where {
			old.Where[k] = append(old.Where[k], values...)
		}
		return entries
	}
	return append(entries, e)
}

// marshal writes entries as a known file, each where on one line.
func marshal(entries []Entry) ([]byte, error) {
	var list yaml.Node
	if err := list.Encode(entries); err != nil {
		return nil, err
	}
	for _, e := range list.Content {
		for i := 0; i+1 < len(e.Content); i += 2 {
			if e.Content[i].Value == "where" {
				e.Content[i+1].Style = yaml.FlowStyle
			}
		}
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&list); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// Package known reads the Markdown known-failure lists and matches test names
// against them, as test/common/known-failures.sh does: kf_load is Load,
// kf_is_known and kf_reason are Lookup, and kf_match_pattern is Match. Both
// run on the cases in test/common/testdata.
package known

import (
	"os"
	"slices"
	"strings"
)

// space is what the shell trims around a field and before a fence marker in
// the C locale. Under a UTF-8 locale it also trims some non-ASCII spaces,
// which ones depending on the system's C library; Load trims only these.
const space = " \t\n\v\f\r"

// List holds the rows of one or more tables: each name, which may be a
// pattern, with its reason.
type List struct {
	names   []string          // in the order they first appear
	reasons map[string]string // by name
}

// Load adds the rows of the table in the file at path. A path that isn't a
// regular file adds nothing, so every failure grades as new. A file that
// can't be read is an error; the shell reports it and loads nothing.
func (l *List) Load(path string) error {
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	l.parse(string(data))
	return nil
}

// parse adds a table's rows. A row is a line starting with "|", split at
// every "|", even an escaped one: the second field is the name and the
// fourth the reason, each trimmed. Lines inside a fence are skipped, and so
// are separator rows, the header (named "Test Name"), and a row with no
// name. A row with no reason gets "unknown". A later row with the same name
// replaces the reason.
func (l *List) parse(text string) {
	// The shell's read drops NUL bytes, and a last line with no newline.
	lines := strings.Split(strings.ReplaceAll(text, "\x00", ""), "\n")
	lines = lines[:len(lines)-1]
	fenced := false
	for _, line := range lines {
		if isFence(line) {
			fenced = !fenced
			continue
		}
		if fenced || !strings.HasPrefix(line, "|") || isSeparator(line) {
			continue
		}
		fields := strings.Split(line, "|")
		name, reason := field(fields, 1), field(fields, 3)
		if name == "" || name == "Test Name" {
			continue
		}
		if reason == "" {
			reason = "unknown"
		}
		l.add(name, reason)
	}
}

// isFence reports whether line opens or closes a fence: it starts with ```
// or ~~~ after any indentation. Either marker closes a fence the other
// opened, and anything may follow it.
func isFence(line string) bool {
	bare := strings.TrimLeft(line, space)
	return strings.HasPrefix(bare, "```") || strings.HasPrefix(bare, "~~~")
}

// isSeparator reports whether row is a separator row: a "|" and then a "-",
// spaces allowed between. An alignment row such as "|:---|" isn't one.
func isSeparator(row string) bool {
	return strings.HasPrefix(strings.TrimLeft(row[1:], space), "-")
}

// field returns fields[i] trimmed, or "" when the row has fewer fields.
func field(fields []string, i int) string {
	if i >= len(fields) {
		return ""
	}
	return strings.Trim(fields[i], space)
}

func (l *List) add(name, reason string) {
	if l.reasons == nil {
		l.reasons = map[string]string{}
	}
	if _, ok := l.reasons[name]; !ok {
		l.names = append(l.names, name)
	}
	l.reasons[name] = reason
}

// Names returns the rows' names, each once, in the order they first appear.
func (l *List) Names() []string {
	return slices.Clone(l.names)
}

// Lookup returns the reason of the row name matches, and whether one does.
// The row named name itself comes first, then the first row, in the order
// the names first appear, whose pattern matches. The shell tries the
// patterns in its hash order instead, so when two patterns with different
// reasons match one name, the reason it reports may differ; whether the name
// is known never does.
func (l *List) Lookup(name string) (string, bool) {
	if reason, ok := l.reasons[name]; ok {
		return reason, true
	}
	for _, pattern := range l.names {
		if Match(pattern, name) {
			return l.reasons[pattern], true
		}
	}
	return "", false
}

// Match reports whether name matches pattern, a row's name. A pattern
// matches itself, and matches as a shell pattern (see glob). A pattern
// ending in ".*" also matches every name starting with what comes before
// the ".*", so "smb2.lock.*" matches "smb2.lock" and "smb2.locking" too. A
// pattern with no "*" that doesn't end in ".t" also matches with ".t" after
// it and every name under it: "open/etxtbsy" matches "open/etxtbsy.t" and
// "open/etxtbsy/x".
func Match(pattern, name string) bool {
	if pattern == name || glob(pattern, name) {
		return true
	}
	if prefix, ok := strings.CutSuffix(pattern, ".*"); ok && strings.HasPrefix(name, prefix) {
		return true
	}
	if strings.HasSuffix(pattern, ".t") || strings.Contains(pattern, "*") {
		return false
	}
	return glob(pattern+".t", name) || glob(pattern+"/*", name)
}

// Package known reads the Markdown known-failure lists and matches test names
// against them, as test/common/known-failures.sh does: kf_load is Load and
// kf_match_pattern is Match. Both run on the cases in test/common/testdata.
package known

import (
	"os"
	"path"
	"slices"
	"strings"
)

// space is what the shell trims around a field and before a fence marker in
// the C locale. Under a UTF-8 locale it also trims some non-ASCII spaces,
// which ones depending on the system's C library; Load trims only these.
const space = " \t\n\v\f\r"

// Row is a row of a list: a test name, which may be a pattern, and why the
// test fails.
type Row struct {
	Name, Reason string
}

// List holds the rows of one or more tables, one per name, in the order the
// names first appear.
type List struct {
	rows  []Row
	index map[string]int // by name
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
	if i, ok := l.index[name]; ok {
		l.rows[i].Reason = reason
		return
	}
	if l.index == nil {
		l.index = map[string]int{}
	}
	l.index[name] = len(l.rows)
	l.rows = append(l.rows, Row{name, reason})
}

// Rows returns the rows, one per name, in the order the names first appear.
func (l *List) Rows() []Row {
	return slices.Clone(l.rows)
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

// glob reports whether name matches pattern as the shell matches a case
// pattern, for the patterns the lists use: "*" matches any string, "/"
// included, and "?" any one character. path.Match's "*" stops at "/", so
// glob hides "/" from it in both, and reads the shell's "[!" as "[^".
//
// decision: bracket expressions, character classes and backslash escapes
// aren't supported, because no list uses them. They get path.Match's
// reading, which agrees with the shell's on plain sets and escapes but not
// on classes, a "]" or "-" at the edge of a set, an unclosed "[" or a
// trailing backslash. Revisit when a list needs one of those: match it as
// the shell does, or refuse the pattern.
func glob(pattern, name string) bool {
	ok, err := path.Match(shellPattern.Replace(pattern), strings.ReplaceAll(name, "/", "\x00"))
	return err == nil && ok
}

// shellPattern turns a shell pattern into path.Match's, with "/" hidden.
var shellPattern = strings.NewReplacer("/", "\x00", "[!", "[^")

package known

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// testdata holds the cases test/common/known-failures_test.sh runs too.
const testdata = "../../common/testdata"

// rows returns the list's rows as the .want files hold them: "name<TAB>reason",
// sorted bytewise.
func rows(l *List) []string {
	var out []string
	for _, name := range l.Names() {
		reason, _ := l.Lookup(name)
		out = append(out, name+"\t"+reason)
	}
	slices.Sort(out)
	return out
}

// differ says how got differs from want, or returns "" when it doesn't.
func differ(got, want []string) string {
	var b strings.Builder
	for _, r := range want {
		if !slices.Contains(got, r) {
			fmt.Fprintf(&b, "\n  missing %q", r)
		}
	}
	for _, r := range got {
		if !slices.Contains(want, r) {
			fmt.Fprintf(&b, "\n  extra   %q", r)
		}
	}
	return b.String()
}

// lines returns the file's lines, without their newlines.
func lines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func write(t *testing.T, path, text string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Each table in the test data loads exactly the rows its .want file lists.
func TestTables(t *testing.T) {
	tables, _ := filepath.Glob(filepath.Join(testdata, "*.md"))
	if len(tables) == 0 {
		t.Fatal("no tables in the test data")
	}
	for _, table := range tables {
		var l List
		if err := l.Load(table); err != nil {
			t.Fatal(err)
		}
		want := lines(t, strings.TrimSuffix(table, ".md")+".want")
		if d := differ(rows(&l), want); d != "" {
			t.Errorf("%s:%s", filepath.Base(table), d)
		}
	}
}

// Each case in match.txt holds, through Match and through a list holding
// only the pattern.
func TestMatch(t *testing.T) {
	cases := 0
	for i, line := range lines(t, filepath.Join(testdata, "match.txt")) {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 3 || (f[2] != "yes" && f[2] != "no") {
			t.Fatalf("match.txt:%d: %q isn't PATTERN<TAB>NAME<TAB>yes|no", i+1, line)
		}
		pattern, name, want := f[0], f[1], f[2] == "yes"
		var l List
		l.add(pattern, "a reason")
		_, known := l.Lookup(name)
		if got := Match(pattern, name); got != want || known != want {
			t.Errorf("%q against %q: Match %v, Lookup %v, want %v", pattern, name, got, known, want)
		}
		cases++
	}
	if cases == 0 {
		t.Fatal("no cases in match.txt")
	}
}

// lineEnds are tables the test data can't hold, since an editor or git may
// rewrite their line ends or treat them as binary.
var lineEnds = []struct {
	name, table string
	want        []string
}{
	{"a last line with no newline is dropped", "| a | x | ra |\n| b | x | rb |", []string{"a\tra"}},
	{"carriage returns are trimmed, after a fence marker too",
		"| a | x | ra |\r\n```\r\n| b | x | rb |\r\n```\r\n| c | x |\r\n", []string{"a\tra", "c\tunknown"}},
	{"NUL bytes are dropped", "| a\x00b | x | r\x00a |\n", []string{"ab\tra"}},
}

func TestLineEnds(t *testing.T) {
	for _, c := range lineEnds {
		var l List
		if err := l.Load(write(t, filepath.Join(t.TempDir(), "list.md"), c.table)); err != nil {
			t.Fatal(err)
		}
		if d := differ(rows(&l), c.want); d != "" {
			t.Errorf("%s:%s", c.name, d)
		}
	}
}

// A second table adds its rows, and a row it shares with the first replaces
// the reason; names keep the order they first appear in. A path that isn't
// a regular file adds nothing.
func TestLoadMerges(t *testing.T) {
	dir := t.TempDir()
	var l List
	for _, path := range []string{
		write(t, filepath.Join(dir, "base.md"), "| b | x | base b |\n| a | x | base a |\n"),
		write(t, filepath.Join(dir, "overlay.md"), "| c | x | overlay c |\n| a | x | overlay a |\n"),
		filepath.Join(dir, "missing.md"),
		dir,
		"",
	} {
		if err := l.Load(path); err != nil {
			t.Fatalf("%q: %v", path, err)
		}
	}
	if got, want := l.Names(), []string{"b", "a", "c"}; !slices.Equal(got, want) {
		t.Errorf("names %q, want %q", got, want)
	}
	if got, _ := l.Lookup("a"); got != "overlay a" {
		t.Errorf("a's reason is %q, want the overlay's", got)
	}
}

// A file that can't be read is an error, not an empty list.
func TestLoadUnreadable(t *testing.T) {
	path := write(t, filepath.Join(t.TempDir(), "list.md"), "| a | x | r |\n")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(path); err == nil {
		t.Skip("this user reads files without permissions")
	}
	var l List
	if err := l.Load(path); err == nil {
		t.Error("no error")
	}
}

// A name's own row comes first; otherwise the first row, in the order the
// names first appear, whose pattern matches.
func TestLookup(t *testing.T) {
	var l List
	l.add("smb2.lock.*", "the family")
	l.add("smb2.lock.lock1", "its own")
	l.add("smb2.*", "wider")
	for name, want := range map[string]string{
		"smb2.lock.lock1": "its own",
		"smb2.lock.lock2": "the family",
		"smb2.read.eof":   "wider",
		"smb3.x":          "",
	} {
		if got, known := l.Lookup(name); got != want || known != (want != "") {
			t.Errorf("%s: %q, %v; want %q", name, got, known, want)
		}
	}
}

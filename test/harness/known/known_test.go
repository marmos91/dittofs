package known

import (
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
	for _, r := range l.Rows() {
		out = append(out, r.Name+"\t"+r.Reason)
	}
	slices.Sort(out)
	return out
}

// lines returns the file's lines, without their newlines; none for an empty
// file.
func lines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		return nil
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
		got, want := rows(&l), lines(t, strings.TrimSuffix(table, ".md")+".want")
		if !slices.Equal(got, want) {
			t.Errorf("%s: got %q, want %q", filepath.Base(table), got, want)
		}
	}
}

// Each case in match.txt holds.
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
		if got := Match(f[0], f[1]); got != (f[2] == "yes") {
			t.Errorf("%q against %q: %v, want %s", f[0], f[1], got, f[2])
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
		if got := rows(&l); !slices.Equal(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
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
	want := []Row{{"b", "base b"}, {"a", "overlay a"}, {"c", "overlay c"}}
	if got := l.Rows(); !slices.Equal(got, want) {
		t.Errorf("rows %q, want %q", got, want)
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

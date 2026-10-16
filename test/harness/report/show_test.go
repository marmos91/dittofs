package report

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Read gives back the run Write wrote, every field and every test in its
// cell, so the report.md Write generates is what summary.json shows.
func TestReadGivesBackTheRun(t *testing.T) {
	dir := t.TempDir()
	if err := full.Write(dir); err != nil {
		t.Fatal(err)
	}
	got, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, full) {
		t.Fatalf("got\n%+v\nwant\n%+v", got, full)
	}
	var md bytes.Buffer
	if err := got.Show(&md, "md", false); err != nil {
		t.Fatal(err)
	}
	if file, err := os.ReadFile(filepath.Join(dir, "report.md")); err != nil || !bytes.Equal(file, md.Bytes()) {
		t.Errorf("report.md isn't what summary.json shows: %v\n%s", err, file)
	}
}

// When one cell's name begins another's, a test goes back to the cell with
// the longer name that begins its suite: unit's own tests to unit, and
// unit/nonrace's to it, whatever their order.
func TestReadNestedCells(t *testing.T) {
	pkg := []string{"example.com/m/a"}
	r := Run{Name: "r", Cells: []Cell{
		{Name: "unit/nonrace", Result: "passed", Tests: []Test{{Name: "TestN", Suite: pkg, Status: Passed}}},
		{Name: "unit", Result: "passed", Tests: []Test{{Name: "TestU", Suite: pkg, Status: Passed}}},
	}}
	dir := t.TempDir()
	if err := r.Write(dir); err != nil {
		t.Fatal(err)
	}
	got, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"TestN", "TestU"} {
		if c := got.Cells[i]; len(c.Tests) != 1 || c.Tests[0].Name != want || !reflect.DeepEqual(c.Tests[0].Suite, pkg) {
			t.Errorf("%s: tests %+v, want %s in %v", c.Name, c.Tests, want, pkg)
		}
	}
}

// Read refuses a summary.json it can't read, and one with a test in no cell.
func TestReadRefuses(t *testing.T) {
	for name, data := range map[string]string{
		"not JSON":  "{",
		"no cell":   `{"results":{"tests":[{"name":"x","suite":["gone"],"status":"passed","duration":0}],"extra":{"cells":[{"suite":"s"}]}}}`,
		"no suites": `{"results":{"tests":[{"name":"x","status":"passed","duration":0}],"extra":{"cells":[{"suite":"s"}]}}}`,
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "summary.json"), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Read(dir); err == nil {
			t.Errorf("%s: read", name)
		}
	}
	if _, err := Read(t.TempDir()); err == nil {
		t.Error("no summary.json: read")
	}
}

// A new failure is a failure graded new, or not graded yet; an unexpected
// pass, a known failure and every other result aren't.
func TestNewFailure(t *testing.T) {
	for _, c := range []struct {
		test Test
		want bool
	}{
		{Test{Status: Failed}, true},
		{Test{Status: Failed, Grade: New}, true},
		{Test{Status: Failed, Grade: "unexpected_pass"}, false},
		{Test{Status: Skipped, Grade: "known"}, false},
		{Test{Status: Other}, false},
		{Test{Status: Passed, Flaky: true}, false},
	} {
		if got := c.test.NewFailure(); got != c.want {
			t.Errorf("%+v: %v, want %v", c.test, got, c.want)
		}
	}
}

// dt report shows a run as text, md or json: each cell, then each new
// failure under its cell with its own suite and its message's first line;
// json is summary.json itself. With failures, only the new failures.
//
// In summary.json, each test's suite is its cell's name, split at /, then its
// own suite; the counts follow the statuses, flaky passes counted too; a
// grade and its reason go in the test's extra; durations are milliseconds;
// and the run's own fields go in extra, each cell's suite apart from the rest
// of its name, with its JUnit file.
func TestShow(t *testing.T) {
	for _, format := range []string{"text", "md", "json"} {
		for failures, suffix := range map[bool]string{false: "", true: "-failures"} {
			var b bytes.Buffer
			if err := full.Show(&b, format, failures); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join("report", "full"+suffix+"."+format)
			if format == "json" && !failures {
				file = "full.summary.json"
			}
			golden(t, file, b.Bytes())
		}
	}
	if err := full.Show(&bytes.Buffer{}, "html", false); err == nil {
		t.Error("an unknown format: no error")
	}
}

// A run with no new failures shows none, and with failures says so.
func TestShowNoNewFailures(t *testing.T) {
	r := Run{Name: "r", Cells: []Cell{{Name: "lint-go", Result: "passed", Tests: []Test{{Name: "go vet ./...", Status: Passed}}}}}
	for format, want := range map[string]string{"text": "run r: exit 0\nno new failures\n", "md": "No new failures.\n"} {
		var b bytes.Buffer
		if err := r.Show(&b, format, true); err != nil || b.String() != want {
			t.Errorf("%s: %q, %v; want %q", format, b.String(), err, want)
		}
		b.Reset()
		if err := r.Show(&b, format, false); err != nil || strings.Contains(strings.ToLower(b.String()), "new failure") {
			t.Errorf("%s: %q, %v", format, b.String(), err)
		}
	}
}

// A lint job's summary reads as it did when dt run wrote it: the same title
// and table, a | in a note escaped. A run with new failures adds a table of
// them after it, as code, where a | in a command can't break the table
// either.
func TestMarkdownAsTheLintJobsHadIt(t *testing.T) {
	passed := Run{Cells: []Cell{{Name: "shellcheck", Result: "passed", Took: time.Second, Tests: []Test{{Name: "git ls-files", Status: Passed}}}}}
	failed := Run{Cells: []Cell{{Name: "lint-go", Result: "failed", Note: "`a | b` exited 1", Took: 2 * time.Second, Tests: []Test{
		{Name: "a | b", Status: Failed, Message: "`a | b` exited 1"},
		{Name: "go vet ./...", Status: Passed},
	}}}}
	// What dt run's own job summary was, for each.
	before := map[*Run]string{
		&passed: "### dt run shellcheck\n\n| Cell | Result | Time | Note |\n|---|---|---|---|\n| `shellcheck` | passed | 1s |  |\n",
		&failed: "### dt run lint-go\n\n| Cell | Result | Time | Note |\n|---|---|---|---|\n| `lint-go` | failed | 2s | `a \\| b` exited 1 |\n",
	}
	added := map[*Run]string{
		&passed: "",
		&failed: "\n#### New failures\n\n| Cell | Test | Message |\n|---|---|---|\n| `lint-go` | `a \\| b` | `` `a \\| b` exited 1 `` |\n",
	}
	for r, want := range before {
		if got := r.markdown(false); got != want+added[r] {
			t.Errorf("got\n%s\nwant\n%s", got, want+added[r])
		}
	}
}

// What a tool prints can't break the job summary's tables or turn into
// markup: a note's line ends become spaces, names and messages are code,
// fenced past any backquotes in them, a message shows its first line that
// isn't blank, and an empty message an empty cell.
func TestMarkdownEscapes(t *testing.T) {
	r := Run{Cells: []Cell{{Name: "unit", Result: "failed", Took: time.Second, Note: "go list:\r\nno Go files\nin x | y", Tests: []Test{
		{Name: "TestNil", Status: Failed, Message: "\n  \n    a_test.go:5: got <nil>, want *err*\n    more"},
		{Name: "it's `quoted`", Status: Failed, Message: "a ``double`` run"},
		{Name: "TestSilent", Status: Failed},
	}}}}
	want := "### dt run unit\n\n| Cell | Result | Time | Note |\n|---|---|---|---|\n" +
		"| `unit` | failed | 1s | go list: no Go files in x \\| y |\n\n" +
		"#### New failures\n\n| Cell | Test | Message |\n|---|---|---|\n" +
		"| `unit` | `TestNil` | `a_test.go:5: got <nil>, want *err*` |\n" +
		"| `unit` | `` it's `quoted` `` | ``` a ``double`` run ``` |\n" +
		"| `unit` | `TestSilent` |  |\n"
	if got := r.markdown(false); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

// However many tests fail, the job summary stays well under 1 MiB: it lists
// the first 200 new failures, each message cut to 200 characters, and says
// how many more there are.
func TestMarkdownCap(t *testing.T) {
	c := Cell{Name: "pjdfstest/badger-s3/4.1", Result: "failed"}
	long := strings.Repeat("not ok 7 - chmod returned EPERM, want 0 ", 50)
	for i := range 8000 {
		c.Tests = append(c.Tests, Test{Name: fmt.Sprintf("chmod/%04d.t", i), Status: Failed, Message: long})
	}
	md := Run{Cells: []Cell{c}}.markdown(false)
	if len(md) > 100<<10 {
		t.Errorf("%d bytes", len(md))
	}
	if rows := strings.Count(md, "| `pjdfstest/badger-s3/4.1` | `chmod/"); rows != 200 {
		t.Errorf("%d rows", rows)
	}
	if want := "| `" + long[:200] + " ...` |\n"; !strings.Contains(md, want) {
		t.Errorf("no message cut to 200 characters:\n%s", md[:2000])
	}
	if !strings.HasSuffix(md, "\nand 7800 more new failures, all in summary.json\n") {
		t.Errorf("ends %q", md[len(md)-200:])
	}
}

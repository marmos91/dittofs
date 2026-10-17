package report

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite the golden files from what the tests get")

// golden compares got with testdata/name, or writes it there with -update.
// A checkout may have turned the file's line ends into CRLF.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n"))) {
		t.Errorf("%s differs; got\n%s", path, got)
	}
}

// full is a run with every field dt writes: a test of each status, a skip
// graded ungraded, a known failure with its tool's status, reason and issue,
// an unexpected pass, an expected skip, a flaky pass, tests below their cell
// (a Go test's package), a cell of an option and matrix values, and cells
// without tests, a log or a note.
var full = Run{
	Name:   "20261015T120000Z-abcdef12-a2",
	Start:  time.UnixMilli(1791460800000),
	Stop:   time.UnixMilli(1791460912000),
	Commit: "abcdef12", Branch: "develop", Dirty: true, OS: "linux", In: "container", Tier: "merge", Exit: 1,
	Cells: []Cell{
		{Name: "lint-repo", Result: "failed", Note: "`false` exited 1", Took: 3 * time.Second, Log: "logs/lint-repo.log", Tests: []Test{
			{Name: "test/conformance/check-docs.sh", Status: Passed, Duration: 1500 * time.Millisecond},
			{Name: "false", Status: Failed, Duration: 2 * time.Millisecond, Message: "`false` exited 1"},
			{Name: "test/new-package-tests.sh {base}", Status: Skipped, Message: "no change to check", Grade: Ungraded},
		}},
		{Name: "unit/windows", Result: "failed", Note: "1 passed, 0 failed, 1 skipped; example.com/m/c: failed outside any test", Took: 61 * time.Second, Log: "logs/unit-windows.log", Tests: []Test{
			{Name: "TestA/sub", Suite: []string{"example.com/m/a"}, Status: Passed, Duration: 40 * time.Millisecond},
			{Name: "TestB", Suite: []string{"example.com/m/a"}, Status: Skipped, Message: "a_test.go:9: needs root", Grade: Ungraded},
			{Name: "TestKnown", Suite: []string{"example.com/m/a"}, Status: Skipped, RawStatus: Failed, Duration: 5 * time.Millisecond, Message: "a_test.go:12: got 1, want 2", Grade: Known,
				Reason: "the clock has a one-second step", Issue: "#12"},
			{Name: "TestFixed", Suite: []string{"example.com/m/a"}, Status: Failed, RawStatus: Passed, Duration: 8 * time.Millisecond, Grade: UnexpectedPass, Reason: "the cache is never flushed"},
			{Name: "TestSoak", Suite: []string{"example.com/m/a"}, Status: Skipped, Message: "a_test.go:30: skipped with -short", Grade: ExpectedSkip, Reason: "a 24-seed soak"},
			{Name: "TestFlaky", Suite: []string{"example.com/m/a"}, Status: Passed, Duration: 70 * time.Millisecond, Flaky: true},
			{Name: "TestCrash", Suite: []string{"example.com/m/c"}, Status: Other, Message: "panic: boom"},
			{Name: "example.com/m/c", Status: Failed, Duration: 300 * time.Millisecond, Message: "failed outside any test"},
		}},
		{Name: "pynfs/badger-s3/1", Result: "not run", Note: "dt doesn't set up fixtures yet"},
		{Name: "smbtorture/memory", Result: "skipped", Note: "docker: not found"},
	},
}

// summary.json is CTRF 0.1.0 by CTRF's own schema: its required fields, no
// field of dt's own outside an extra, and suite as an array. Without any
// cells too. And the check isn't vacuous: it refuses a test with a field of
// dt's own outside its extra.
func TestJSONMatchesTheSchema(t *testing.T) {
	s := loadSchema(t)
	for name, r := range map[string]Run{"full": full, "empty": {Name: "20261015T120000Z-abcdef12", In: "host", Tier: "pr", Exit: 2}} {
		data, err := r.JSON()
		if err != nil {
			t.Fatal(err)
		}
		if errs := s.check(s.root, decode(t, data), "$"); len(errs) > 0 {
			t.Errorf("%s:\n%s", name, strings.Join(errs, "\n"))
		}
	}
	data, err := full.JSON()
	if err != nil {
		t.Fatal(err)
	}
	doc := decode(t, data).(map[string]any)
	doc["results"].(map[string]any)["tests"].([]any)[0].(map[string]any)["grade"] = "new"
	if errs := s.check(s.root, doc, "$"); len(errs) == 0 {
		t.Error("the check passes a test with the field grade outside its extra")
	}
}

// A reason goes in a test's extra even without a grade.
func TestJSONKeepsAReasonAlone(t *testing.T) {
	data, err := Run{Cells: []Cell{{Name: "s", Tests: []Test{{Name: "t", Status: Skipped, Reason: "why"}}}}}.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"reason": "why"`)) {
		t.Errorf("no reason:\n%s", data)
	}
}

// summary.json is readable by anyone, as the logs are, so a run as root
// leaves one its user can read; the JUnit files too (TestWriteJUnit).
func TestWriteIsReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes have no read bits for others here")
	}
	dir := t.TempDir()
	if err := (Run{Name: "r"}).Write(dir); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(dir, "summary.json")); err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("mode %v, %v", info.Mode(), err)
	}
}

// A reader of summary.json, or of a JUnit file, never sees part of one: while
// they're rewritten over and over, every read gives a whole document, and no
// temporary file is left, beside summary.json and report.md or beside the
// JUnit files.
func TestWriteReplacesTheFileWhole(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a file open for reading can't be replaced here, so the reads would stop the writes")
	}
	dir := t.TempDir()
	big := Run{Name: "big", Cells: []Cell{{Name: "s"}}}
	for i := range 5000 {
		big.Cells[0].Tests = append(big.Cells[0].Tests, Test{Name: "test " + strconv.Itoa(i), Status: Passed})
	}
	small := Run{Name: "small", Cells: []Cell{{Name: "s", Tests: big.Cells[0].Tests[:1]}}}
	if err := small.Write(dir); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 40 {
			r := small
			if i%2 == 0 {
				r = big
			}
			if err := r.Write(dir); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for reads := 0; ; reads++ {
		data, err := os.ReadFile(filepath.Join(dir, "summary.json"))
		if err != nil || !json.Valid(data) {
			<-done
			t.Fatalf("read %d: %d bytes, not a whole document: %v", reads, len(data), err)
		}
		x, err := os.ReadFile(filepath.Join(dir, "junit", "s.xml"))
		if err == nil {
			err = oneDocument(x)
		}
		if err != nil {
			<-done
			t.Fatalf("read %d: %d bytes of junit/s.xml, not a whole document: %v", reads, len(x), err)
		}
		select {
		case <-done:
			left, _ := os.ReadDir(dir)
			junit, _ := os.ReadDir(filepath.Join(dir, "junit"))
			if len(left) != 3 || len(junit) != 1 {
				t.Errorf("left behind: %v, junit/%v", left, junit)
			}
			return
		default:
		}
	}
}

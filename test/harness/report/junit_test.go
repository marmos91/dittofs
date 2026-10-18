package report

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// A test's CTRF status, which its grade decides, gives its JUnit one: a
// pass; a failure; skipped with the entry's reason for a known failure, and
// with the tool's for an ungraded skip; an error for an unexpected skip; a
// failure with its grade as its message for an unexpected pass, which the
// tool said nothing about. JUnit reads the grade only for that message, so
// grades that share a status share a row.
func TestJUnitGrades(t *testing.T) {
	pjdfstest, smbtorture, unit := "pjdfstest/badger-s3/4.1", "smbtorture/memory", "unit"
	for _, c := range []struct {
		file, cell, result string
		test               Test
	}{
		{"passed", pjdfstest, "passed", Test{Name: "chmod/00.t", Status: Passed, Duration: 1500 * time.Millisecond}},
		{"failed", "lint-repo", "failed", Test{Name: "false", Status: Failed, Duration: 2 * time.Millisecond, Message: "`false` exited 1", Grade: New}},
		{"known", pjdfstest, "passed", Test{Name: "unlink/14.t", Status: Skipped, RawStatus: Failed, Duration: 860 * time.Millisecond, Message: "not ok 3", Grade: Known,
			Reason: "NFSv4 silly rename: unlinking an open file renames it, so its link count stays 1"}},
		{"unexpected_pass", smbtorture, "failed", Test{Name: "smb2.notify.rec", Status: Failed, RawStatus: Passed, Duration: 40 * time.Millisecond, Grade: UnexpectedPass,
			Reason: "a buffered backlog arrives per operation", Issue: "#12"}},
		{"unexpected_skip", smbtorture, "not run", Test{Name: "smb2.lock.rw", Status: Other, Message: "skip: no lock support", Grade: "unexpected_skip"}},
		{"ungraded", unit, "passed", Test{Name: "TestB", Suite: []string{"example.com/m/a"}, Status: Skipped, Message: "a_test.go:9: needs root", Grade: Ungraded}},
	} {
		cell := Cell{Name: c.cell, Result: c.result, Took: 912 * time.Second, Tests: []Test{c.test}}
		got, err := cell.junit()
		if err != nil {
			t.Fatal(err)
		}
		golden(t, filepath.Join("junit", c.file+".xml"), got)
	}
}

// A cell that didn't run, or failed, with no testcase that says so, gets one
// named after it, with its note, as an error or a failure, so a reader of
// JUnit alone sees it red: a cell with no tests, and one whose tests passed
// before its limit stopped it. A cell skipped for its needs, which passes,
// gets none, and its properties say why.
func TestJUnitCellResult(t *testing.T) {
	for _, c := range []struct {
		file string
		cell Cell
	}{
		{"no-tests", Cell{Name: "pynfs/badger-s3/1", Result: "not run", Note: "dt doesn't set up fixtures yet"}},
		{"failed-without-a-failed-test", Cell{Name: "pjdfstest/memory/3", Result: "failed", Took: 60 * time.Second,
			Note:  "chmod/00.t: stopped at the cell's limit; 2 tests not started: stopped at the cell's limit",
			Tests: []Test{{Name: "chmod/00.t", Status: Passed, Duration: time.Second}}}},
		{"skipped-cell", Cell{Name: "smbtorture/memory", Result: "skipped", Note: "docker: not found"}},
	} {
		got, err := c.cell.junit()
		if err != nil {
			t.Fatal(err)
		}
		golden(t, filepath.Join("junit", c.file+".xml"), got)
	}
}

// What a tool prints can hold anything: markup, quotes, line ends, tabs, a
// terminal's escapes, NUL. The file stays well-formed, the text comes back
// as written, in attributes and bodies, and a character XML can't hold comes
// back as U+FFFD. The counts agree with the testcases.
func TestJUnitIsWellFormed(t *testing.T) {
	odd := "a <b> & \"c\" 'd' ]]> e\nf\r\n\tg"
	cell := Cell{Name: "s/<&>", Result: "failed", Note: odd, Tests: []Test{
		{Name: odd, Suite: []string{odd}, Status: Failed, Message: odd},
		{Name: "esc", Status: Other, Message: "\x1b[31mred\x1b[0m\x00"},
		{Name: "skip", Status: Skipped, Reason: odd, Message: odd},
		{Name: "pass", Status: Passed},
	}}
	data, err := cell.junit()
	if err != nil {
		t.Fatal(err)
	}
	var doc junitDoc
	if err := xml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	s := doc.Suites[0]
	if s.Name != cell.Name || s.Properties.List[1].Value != odd || s.Cases[0].Name != odd || s.Cases[0].Classname != cell.Name+"/"+odd ||
		s.Cases[0].Failure.Message != odd || s.Cases[0].Failure.Body != odd || s.Cases[2].Skipped.Message != odd || s.Cases[2].Skipped.Body != odd {
		t.Errorf("text changed: %+v", s)
	}
	if got, want := s.Cases[1].Error.Message, "�[31mred�[0m�"; got != want || s.Cases[1].Error.Body != want {
		t.Errorf("escapes: got %q and %q, want %q", got, s.Cases[1].Error.Body, want)
	}
	if s.Tests != 4 || s.Failures != 1 || s.Errors != 1 || s.Skipped != 1 {
		t.Errorf("counts %+v", s)
	}
}

// Write writes summary.json last, so it never names a JUnit file that isn't
// there: when a JUnit file can't be written, no summary.json is.
func TestWriteSummaryLast(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "junit"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := full.Write(dir); err == nil {
		t.Fatal("wrote with junit a file")
	}
	if _, err := os.Stat(filepath.Join(dir, "summary.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("summary.json written before the JUnit files: %v", err)
	}
}

// Write writes a JUnit file for every cell, the one its entry in summary.json
// names, readable by anyone, as every file Write writes is, and leaves no
// temporary file.
func TestWriteJUnit(t *testing.T) {
	dir := t.TempDir()
	if err := full.Write(dir); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Results struct {
			Extra struct{ Cells []struct{ JUnit string } }
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "summary.json"))
	if err == nil {
		err = json.Unmarshal(data, &doc)
	}
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range full.Cells {
		path := doc.Results.Extra.Cells[i].JUnit
		got, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		want, _ := c.junit()
		if !bytes.Equal(got, want) {
			t.Errorf("%s: %s isn't the cell's JUnit", c.Name, path)
		}
		if info, _ := os.Stat(filepath.Join(dir, path)); runtime.GOOS != "windows" && info.Mode().Perm() != 0o644 {
			t.Errorf("%s: mode %v", path, info.Mode())
		}
	}
	if left, _ := os.ReadDir(filepath.Join(dir, "junit")); len(left) != len(full.Cells) {
		t.Errorf("junit holds %d files for %d cells: %v", len(left), len(full.Cells), left)
	}
}

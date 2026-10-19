package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/harness/report"
)

// script is a scripts' cell: the runner's exit status, and the grader's
// grades, "" when it wrote none.
type script struct{ status, grades string }

// TestCompare compares cells as a workflow run's artifacts hold them: each dt
// cell in a run of its own, as dt writes it, and each scripts' cell under the
// path its job uploads.
func TestCompare(t *testing.T) {
	pass := report.Test{Name: "p", Status: report.Passed}
	known := report.Test{Name: "k", Status: report.Skipped, RawStatus: report.Failed, Grade: report.Known}
	newFail := report.Test{Name: "n", Status: report.Failed, Grade: report.New}
	unexpectedPass := report.Test{Name: "u", Status: report.Failed, RawStatus: report.Passed, Grade: report.UnexpectedPass}
	skipped := report.Test{Name: "s", Status: report.Other, RawStatus: report.Skipped, Grade: report.UnexpectedSkip}
	expectedSkip := report.Test{Name: "e", Status: report.Skipped, RawStatus: "NotExecuted", Grade: report.ExpectedSkip}
	noResult := report.Test{Name: "i", Status: report.Other, Grade: report.Incomplete}
	cell := func(name, result string, tests ...report.Test) report.Cell {
		return report.Cell{Name: name, Result: result, Tests: tests}
	}
	for _, tc := range []struct {
		name    string
		dt      []report.Cell
		scripts map[string]script
		same    bool
		says    string // in the comparison
	}{
		{"the same", []report.Cell{cell("pjdfstest/memory/3", report.Passed, pass, known)},
			map[string]script{"pjdfstest/memory-3": {"0", "known k\nran 2\n"}},
			true, "- `pjdfstest/memory/3` (dt: passed, scripts: exit 0): the same"},
		{"a verdict only the scripts give", []report.Cell{cell("pjdfstest/memory/3", report.Passed, pass, known)},
			map[string]script{"pjdfstest/memory-3": {"1", "known k\nran 2\n"}},
			false, "(dt: passed, scripts: exit 1): differs: the verdict"},
		{"different new failures", []report.Cell{cell("wpts/memory", report.Failed, newFail, known)},
			map[string]script{"wpts/memory": {"1", "known k\nnew x\nran 2\n"}},
			false, "differs: new failures only through dt: n; only through the scripts: x"},
		{"different known failures", []report.Cell{cell("wpts/memory", report.Passed, pass, known)},
			map[string]script{"wpts/memory": {"0", "known j\nran 2\n"}},
			false, "differs: known failures only through dt: k; only through the scripts: j"},
		{"different numbers of tests run", []report.Cell{cell("wpts/memory", report.Passed, pass, known)},
			map[string]script{"wpts/memory": {"0", "known k\nran 3\n"}},
			false, "differs: tests run: 2 through dt, 3 through the scripts"},
		{"skips and tests with no result only dt grades",
			[]report.Cell{cell("wpts/memory", "not run", pass, known, skipped, expectedSkip, noResult)},
			map[string]script{"wpts/memory": {"0", "known k\nran 2\n"}},
			true, "(dt: not run, scripts: exit 0): the same"},
		{"a cell not run for something else", []report.Cell{cell("wpts/memory", "not run", pass, known)},
			map[string]script{"wpts/memory": {"0", "known k\nran 2\n"}},
			false, "differs: the verdict"},
		{"an unexpected pass", []report.Cell{cell("wpts/memory", report.Failed, unexpectedPass, known, skipped)},
			map[string]script{"wpts/memory": {"0", "known k\nran 2\n"}},
			false, "differs: the verdict"},
		{"scripts that graded no test", []report.Cell{cell("pjdfstest/memory/3", "not run", noResult, known)},
			map[string]script{"pjdfstest/memory-3": {"1", ""}},
			true, "(dt: not run, scripts: exit 1): the same"},
		{"pynfs's minor version", []report.Cell{cell("pynfs/badger-s3/1", report.Passed, pass)},
			map[string]script{"pynfs/badger-s3-4.1": {"0", "ran 1\n"}},
			true, "- `pynfs/badger-s3/1` (dt: passed, scripts: exit 0): the same"},
		{"a cell no script runs, green", []report.Cell{cell("smbtorture/badger-s3", report.Passed, pass)}, nil,
			true, "- `smbtorture/badger-s3` (dt: passed): the same"},
		{"a cell no script runs, not green", []report.Cell{cell("smbtorture/badger-s3", "not run", pass, skipped)}, nil,
			false, "differs: no script runs it, so it must be green"},
		{"a cell only dt ran", []report.Cell{cell("wpts/badger", report.Passed, pass)}, nil,
			false, "- `wpts/badger` (dt: passed): differs: no results through the scripts"},
		{"a cell only the scripts ran", nil, map[string]script{"wpts/badger": {"0", "ran 1\n"}},
			false, "- `wpts/badger` (scripts: exit 0): differs: no results through dt"},
		{"another suite's cells", nil, map[string]script{"cthon04/memory-v3": {"1", "ran 1\n"}}, true, "0 of 0 cells differ"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for i, c := range tc.dt {
				// Each dt job uploads its run, named after the time it started.
				run := report.Run{Name: "20261019T040000Z-0d8dbdd8", Start: time.Now(), Stop: time.Now(), Cells: []report.Cell{c}}
				if err := run.Write(filepath.Join(dir, "dt-results-"+strconv.Itoa(i), run.Name)); err != nil {
					t.Fatal(err)
				}
			}
			for name, s := range tc.scripts {
				// A job that uploads a path under /tmp too keeps the checkout's absolute path.
				cell := filepath.Join(dir, strings.ReplaceAll(name, "/", "-"), "home/runner/work/dittofs/dittofs/test/conformance/results", name)
				write(t, filepath.Join(cell, "status"), s.status+"\n")
				if s.grades != "" {
					write(t, filepath.Join(cell, "grades"), s.grades)
				}
			}
			out, same, err := compare(dir, []string{"pjdfstest", "pynfs", "wpts", "smbtorture"})
			if err != nil {
				t.Fatal(err)
			}
			if same != tc.same || !strings.Contains(out, tc.says) {
				t.Errorf("same %v, want %v; out:\n%s\nwant it to hold %q", same, tc.same, out, tc.says)
			}
		})
	}
}

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

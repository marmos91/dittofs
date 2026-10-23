package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/harness/registry"
	"github.com/marmos91/dittofs/test/harness/report"
)

// script is a scripts' cell: the runner's exit status, the grader's grades
// and the kernel it saved, each "" when it wrote none.
type script struct{ status, grades, kernel string }

// TestCompare compares cells as a workflow run's artifacts hold them: each dt
// cell in a run of its own, as dt writes it, and each scripts' cell under the
// path its job uploads. The plan is dt's cells, unless a case names it. wpts's
// known file has i, intermittent on memory only. dt runs on kernel 6.17.
func TestCompare(t *testing.T) {
	root, r := wpts(t)
	pass := report.Test{Name: "p", Status: report.Passed}
	known := report.Test{Name: "k", Status: report.Skipped, RawStatus: report.Failed, Grade: report.Known}
	knownToo := report.Test{Name: "j", Status: report.Skipped, RawStatus: report.Failed, Grade: report.Known}
	newFail := report.Test{Name: "n", Status: report.Failed, Grade: report.New}
	unexpectedPass := report.Test{Name: "u", Status: report.Failed, RawStatus: report.Passed, Grade: report.UnexpectedPass}
	skipped := report.Test{Name: "s", Status: report.Other, RawStatus: report.Skipped, Grade: report.UnexpectedSkip}
	expectedSkip := report.Test{Name: "e", Status: report.Skipped, RawStatus: "NotExecuted", Grade: report.ExpectedSkip}
	// smbtorture fails the self-contradictory ChannelSequence draw, its reader
	// grades it an expected skip and its scripts rewrite it to a skip.
	draw := report.Test{Name: "smb2.replay.channel-sequence", Status: report.Skipped, RawStatus: report.Failed, Grade: report.ExpectedSkip}
	noResult := report.Test{Name: "o", Status: report.Other, Grade: report.Incomplete}
	intermittent := report.Test{Name: "i", Status: report.Skipped, RawStatus: report.Failed, Grade: report.Known}
	intermittentPass := report.Test{Name: "i", Status: report.Passed}
	cell := func(name, result string, tests ...report.Test) report.Cell {
		return report.Cell{Name: name, Result: result, Tests: tests}
	}
	for _, tc := range []struct {
		name    string
		dt      []report.Cell
		scripts map[string]script
		same    bool
		says    string // in the comparison
		plan    []string
	}{
		{"the same", []report.Cell{cell("pjdfstest/memory/3", report.Passed, pass, known)},
			map[string]script{"pjdfstest/memory-3": {"0", "known k\nran 2\n", ""}},
			true, "- `pjdfstest/memory/3` (dt: passed on kernel 6.17, scripts: exit 0): the same", nil},
		{"failures in another order", []report.Cell{cell("wpts/memory", report.Passed, pass, known, knownToo)},
			map[string]script{"wpts/memory": {"0", "known k\nknown j\nran 3\n", ""}},
			true, "(dt: passed on kernel 6.17, scripts: exit 0): the same", nil},
		{"a verdict only the scripts give", []report.Cell{cell("pjdfstest/memory/3", report.Passed, pass, known)},
			map[string]script{"pjdfstest/memory-3": {"1", "known k\nran 2\n", ""}},
			false, "(dt: passed on kernel 6.17, scripts: exit 1): differs: the verdict", nil},
		{"different new failures", []report.Cell{cell("wpts/memory", report.Failed, newFail, known)},
			map[string]script{"wpts/memory": {"1", "known k\nnew x\nran 2\n", ""}},
			false, "differs: new failures only through dt: n; only through the scripts: x", nil},
		{"different known failures", []report.Cell{cell("wpts/memory", report.Passed, pass, known)},
			map[string]script{"wpts/memory": {"0", "known j\nran 2\n", ""}},
			false, "differs: known failures only through dt: k; only through the scripts: j", nil},
		{"different numbers of tests run", []report.Cell{cell("wpts/memory", report.Passed, pass, known)},
			map[string]script{"wpts/memory": {"0", "known k\nran 3\n", ""}},
			false, "differs: tests run: 2 through dt, 3 through the scripts", nil},
		{"an intermittent known failure through dt only", []report.Cell{cell("wpts/memory", report.Passed, pass, known, intermittent)},
			map[string]script{"wpts/memory": {"0", "known k\nran 3\n", "7.0"}},
			true, "(dt: passed on kernel 6.17, scripts: exit 0 on kernel 7.0): the same", nil},
		{"an intermittent known failure through the scripts only", []report.Cell{cell("wpts/memory", report.Passed, pass, known, intermittentPass)},
			map[string]script{"wpts/memory": {"0", "known k\nknown i\nran 3\n", "7.0"}},
			true, "(dt: passed on kernel 6.17, scripts: exit 0 on kernel 7.0): the same", nil},
		{"an intermittent known failure and a verdict only the scripts give", []report.Cell{cell("wpts/memory", report.Passed, pass, known, intermittent)},
			map[string]script{"wpts/memory": {"1", "known k\nran 3\n", "7.0"}},
			false, "(dt: passed on kernel 6.17, scripts: exit 1 on kernel 7.0): differs: the verdict\n", nil},
		{"an intermittent known failure and different numbers of tests run", []report.Cell{cell("wpts/memory", report.Passed, pass, known, intermittent)},
			map[string]script{"wpts/memory": {"0", "known k\nran 2\n", "7.0"}},
			false, "): differs: tests run: 3 through dt, 2 through the scripts\n", nil},
		{"a known failure whose intermittent entry applies elsewhere", []report.Cell{cell("wpts/badger", report.Passed, pass, known, intermittent)},
			map[string]script{"wpts/badger": {"0", "known k\nran 3\n", "7.0"}},
			false, "differs: known failures only through dt: i; only through the scripts: none", nil},
		{"an intermittent known failure on the same kernel", []report.Cell{cell("wpts/memory", report.Passed, pass, known, intermittent)},
			map[string]script{"wpts/memory": {"0", "known k\nran 3\n", "6.17"}},
			false, "differs: known failures only through dt: i; only through the scripts: none", nil},
		{"a cell whose entries the registry can't find", []report.Cell{cell("pjdfstest/memory/3", report.Passed, pass, intermittent), cell("wpts/memory", report.Passed, pass)},
			map[string]script{"pjdfstest/memory-3": {"0", "ran 2\n", "7.0"}, "wpts/memory": {"0", "ran 1\n", ""}},
			false, "differs: can't tell its intermittent entries: no suite or group \"pjdfstest\"\n- `wpts/memory` (dt: passed on kernel 6.17, scripts: exit 0): the same", nil},
		{"skips only dt grades", []report.Cell{cell("wpts/memory", "not run", pass, known, skipped, expectedSkip)},
			map[string]script{"wpts/memory": {"0", "known k\nran 2\n", ""}},
			true, "(dt: not run on kernel 6.17, scripts: exit 0): the same", nil},
		{"the ChannelSequence draw", []report.Cell{cell("smbtorture/memory", report.Passed, pass, draw)},
			map[string]script{"smbtorture/memory": {"0", "ran 1\n", ""}},
			true, "(dt: passed on kernel 6.17, scripts: exit 0): the same", nil},
		{"tests with no result only dt grades", []report.Cell{cell("wpts/memory", "not run", pass, known, noResult)},
			map[string]script{"wpts/memory": {"0", "known k\nran 2\n", ""}},
			true, "(dt: not run on kernel 6.17, scripts: exit 0): the same", nil},
		{"a cell not run for something else", []report.Cell{cell("wpts/memory", "not run", pass, known)},
			map[string]script{"wpts/memory": {"0", "known k\nran 2\n", ""}},
			false, "differs: the verdict", nil},
		{"an unexpected pass", []report.Cell{cell("wpts/memory", report.Failed, unexpectedPass, known, skipped)},
			map[string]script{"wpts/memory": {"0", "known k\nran 2\n", ""}},
			false, "differs: the verdict", nil},
		{"scripts that graded no test", []report.Cell{cell("pjdfstest/memory/3", "not run", noResult, known)},
			map[string]script{"pjdfstest/memory-3": {"1", "", ""}},
			true, "(dt: not run on kernel 6.17, scripts: exit 1): the same", nil},
		{"scripts that pass without grading", []report.Cell{cell("pjdfstest/memory/3", report.Passed, pass, known)},
			map[string]script{"pjdfstest/memory-3": {"0", "", ""}},
			false, "differs: the scripts passed it without grading each test", nil},
		{"a cell only dt ran", []report.Cell{cell("wpts/badger", report.Passed, pass)}, nil,
			false, "- `wpts/badger` (dt: passed on kernel 6.17): differs: no results through the scripts", nil},
		{"a cell only the scripts ran", nil, map[string]script{"wpts/badger": {"0", "ran 1\n", ""}},
			false, "- `wpts/badger` (scripts: exit 0): differs: no results through dt", nil},
		{"a planned cell no side ran", []report.Cell{cell("wpts/memory", report.Passed, pass)},
			map[string]script{"wpts/memory": {"0", "ran 1\n", ""}},
			false, "- `smbtorture/badger-s3` (dt: none): differs: no results through dt", []string{"wpts/memory", "smbtorture/badger-s3"}},
		{"no cell planned", nil, nil, false, "No cells to compare.", nil},
		{"another suite's cells", []report.Cell{cell("wpts/memory", report.Passed, pass), cell("cthon04/memory/3", report.Failed, newFail)},
			map[string]script{"wpts/memory": {"0", "ran 1\n", ""}, "cthon04/memory-v3": {"1", "ran 1\n", ""}},
			true, "0 of 1 cells differ", []string{"wpts/memory"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for i, c := range tc.dt {
				// Each dt job uploads its run, named after the time it started.
				run := report.Run{Name: "20261019T040000Z-0d8dbdd8", Start: time.Now(), Stop: time.Now(), OS: "linux", Kernel: "6.17", Tier: "nightly", Cells: []report.Cell{c}}
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
				if s.kernel != "" {
					write(t, filepath.Join(cell, "kernel"), s.kernel+"\n")
				}
			}
			plan := tc.plan
			if plan == nil {
				for _, c := range tc.dt {
					plan = append(plan, c.Name)
				}
			}
			out, same, err := compare(dir, plan, []string{"pjdfstest", "wpts", "smbtorture"}, root, r)
			if err != nil {
				t.Fatal(err)
			}
			if same != tc.same || !strings.Contains(out, tc.says) {
				t.Errorf("same %v, want %v; out:\n%s\nwant it to hold %q", same, tc.same, out, tc.says)
			}
		})
	}
}

// wpts is a checkout of its own whose registry has a suite wpts, on memory
// and badger, with known failures k, j and i, i intermittent on memory only.
func wpts(t *testing.T) (string, *registry.Registry) {
	t.Helper()
	root := t.TempDir()
	const fail = "status: fail, fix: wontfix, category: env, reason: r"
	write(t, filepath.Join(root, "known.yaml"), "- { test: k, "+fail+" }\n- { test: j, "+fail+" }\n- { test: i, "+fail+", where: { profile: [memory] }, intermittent: true }\n")
	r, err := registry.Parse(root, []byte(`version: 1
defaults: { format: exit, tier: pr, timeout: { test: 30m, cell: 60m } }
suites:
  wpts: { cmd: "echo {profile}", matrix: { profile: [memory, badger] }, known: known.yaml }
`))
	if err != nil {
		t.Fatal(err)
	}
	return root, r
}

// An intermittent known failure only one side has differs when either side
// names no kernel, as when both name the same one.
func TestIntermittentNeedsBothKernels(t *testing.T) {
	root, r := wpts(t)
	for _, k := range []struct{ dt, scripts string }{{"", "7.0"}, {"6.17", ""}} {
		dir := t.TempDir()
		run := report.Run{Name: "20261019T040000Z-0d8dbdd8", OS: "linux", Kernel: k.dt, Tier: "nightly", Cells: []report.Cell{{Name: "wpts/memory", Result: report.Passed,
			Tests: []report.Test{{Name: "i", Status: report.Skipped, RawStatus: report.Failed, Grade: report.Known}}}}}
		if err := run.Write(filepath.Join(dir, "dt-results", run.Name)); err != nil {
			t.Fatal(err)
		}
		cell := filepath.Join(dir, "wpts-memory", "conformance", "results", "wpts", "memory")
		write(t, filepath.Join(cell, "status"), "0\n")
		write(t, filepath.Join(cell, "grades"), "ran 1\n")
		if k.scripts != "" {
			write(t, filepath.Join(cell, "kernel"), k.scripts+"\n")
		}
		out, same, err := compare(dir, []string{"wpts/memory"}, []string{"wpts"}, root, r)
		if want := "differs: known failures only through dt: i;"; err != nil || same || !strings.Contains(out, want) {
			t.Errorf("dt on %q, the scripts on %q: %v, same %v; out:\n%s\nwant it to hold %q", k.dt, k.scripts, err, same, out, want)
		}
	}
}

// Each side's results name the kernel it ran on: dt's run's, and the one the
// scripts saved beside their cell's status.
func TestKernels(t *testing.T) {
	dir := t.TempDir()
	run := report.Run{Name: "20261019T040000Z-0d8dbdd8", Kernel: "6.17.0-1022-azure", Cells: []report.Cell{{Name: "wpts/memory", Result: report.Passed}}}
	if err := run.Write(filepath.Join(dir, "dt-results", run.Name)); err != nil {
		t.Fatal(err)
	}
	cell := filepath.Join(dir, "wpts-memory", "conformance", "results", "wpts", "memory")
	write(t, filepath.Join(cell, "status"), "0\n")
	write(t, filepath.Join(cell, "kernel"), "7.0.0-1017-azure\n")
	write(t, filepath.Join(cell, "grades"), "ran 0\n")
	out, same, err := compare(dir, []string{"wpts/memory"}, []string{"wpts"}, "", nil)
	if want := "- `wpts/memory` (dt: passed on kernel 6.17.0-1022-azure, scripts: exit 0 on kernel 7.0.0-1017-azure): the same\n"; err != nil || !same || !strings.Contains(out, want) {
		t.Errorf("%v, same %v; out:\n%s\nwant it to hold %q", err, same, out, want)
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

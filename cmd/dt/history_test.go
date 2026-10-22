package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/harness/report"
)

// writeRuns writes runs into the checkout's results, as dt run does.
func writeRuns(t *testing.T, root string, runs ...report.Run) {
	t.Helper()
	for _, r := range runs {
		if err := r.Write(filepath.Join(root, "test", "harness", "results", r.Name)); err != nil {
			t.Fatal(err)
		}
	}
}

// dt history counts per suite, per test of SUITE, and for SUITE/TEST, whose
// name may hold a /, also lists each of its results, oldest first, so it says
// since when the test fails. A pass on a retry is flaky, as its run's
// summary.json says. --last reads fewer runs, and a suite or test
// with no result says so.
func TestHistory(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "test", "suites.yaml"), "")
	t.Chdir(root)
	cell := func(chmod report.Test) report.Cell {
		chmod.Name, chmod.Duration = "chmod/00.t", time.Second
		return report.Cell{Name: "pjdfstest/badger-s3/3", Result: report.Passed, Took: time.Minute, Tests: []report.Test{
			chmod, {Name: "open/00.t", Status: report.Passed},
		}}
	}
	writeRuns(t, root,
		report.Run{Name: "20261019T100000Z-aaaaaaaa", Cells: []report.Cell{cell(report.Test{Status: report.Passed, Flaky: true}), {Name: "unit", Result: report.Passed, Took: time.Second}}},
		report.Run{Name: "20261019T110000Z-bbbbbbbb", Cells: []report.Cell{cell(report.Test{Status: report.Skipped, RawStatus: report.Failed, Grade: report.Known})}},
	)

	for _, c := range []struct {
		args []string
		want []string
	}{
		{[]string{"history"}, []string{"SUITE      RUNS  PASS  FAIL  SKIP  FLAKY  P50   P90\npjdfstest  2     3     1     0     1      1m0s  1m0s\nunit       1     0     0     0     0      1s    1s\n"}},
		{[]string{"history", "pjdfstest"}, []string{"chmod/00.t  2     1     1     0     1      1s   1s\nopen/00.t   2     2     0"}},
		{[]string{"history", "pjdfstest/chmod/00.t"}, []string{
			"chmod/00.t  2     1     1",
			"RUN                        CELL                   TEST        RESULT          TIME\n" +
				"20261019T100000Z-aaaaaaaa  pjdfstest/badger-s3/3  chmod/00.t  passed, flaky   1s\n" +
				"20261019T110000Z-bbbbbbbb  pjdfstest/badger-s3/3  chmod/00.t  failed (known)  1s\n",
		}},
		{[]string{"history", "pjdfstest", "--last", "1"}, []string{"chmod/00.t  1     0     1"}},
		{[]string{"history", "nfstest"}, []string{"no results of nfstest in the runs read\n"}},
	} {
		out, err := dt(t, c.args...)
		for _, want := range c.want {
			if err != nil || !strings.Contains(out, want) {
				t.Errorf("dt %s: %v\n%s\nwant %q", strings.Join(c.args, " "), err, out, want)
			}
		}
	}
	if _, err := dt(t, "history", "--last", "0"); err == nil {
		t.Error("--last 0 accepted")
	}
}

// dt known prune removes from each known file of the suite and its options
// the entries whose test matched no result of their kind, a failure or a
// skip, in the cells the file grades, and leaves a file whose cells have no
// results as it is.
func TestKnownPrune(t *testing.T) {
	root := t.TempDir()
	entry := func(test, status string) string {
		return "- {test: " + test + ", status: " + status + ", fix: wontfix, category: proto, reason: r}\n"
	}
	writeFile(t, filepath.Join(root, "test", "suites.yaml"), "version: 1\ndefaults: { format: exit, tier: pr, timeout: { test: 30m, cell: 60m } }\nsuites:\n"+
		"  s: { cmd: \"true\", known: s.yaml, options: { shares: { cmd: \"true\" }, own: { cmd: \"true\", known: own.yaml }, idle: { cmd: \"true\", known: idle.yaml } } }\n"+
		"  other: { cmd: \"true\" }\n")
	writeFile(t, filepath.Join(root, "s.yaml"), entry("a", "fail")+entry("pat/*", "fail")+entry("gone", "fail")+entry("b", "skip")+entry("c", "fail")+entry("z", "fail"))
	writeFile(t, filepath.Join(root, "own.yaml"), entry("z", "fail"))
	writeFile(t, filepath.Join(root, "idle.yaml"), entry("x", "fail"))
	t.Chdir(root)
	writeRuns(t, root, report.Run{Name: "20261019T100000Z-aaaaaaaa", Cells: []report.Cell{
		{Name: "s", Result: report.Passed, Tests: []report.Test{
			{Name: "a", Status: report.Skipped, RawStatus: report.Failed, Grade: report.Known},
			{Name: "gone", Status: report.Passed},
			{Name: "b", Status: report.Skipped, Grade: report.ExpectedSkip},
			{Name: "c", Status: report.Other, RawStatus: report.Skipped, Grade: report.UnexpectedSkip},
		}},
		{Name: "s/shares", Result: report.Failed, Tests: []report.Test{{Name: "pat/x", Status: report.Failed, Grade: report.New}}},
		{Name: "s/own", Result: report.Passed, Tests: []report.Test{{Name: "z", Status: report.Skipped, RawStatus: report.Failed, Grade: report.Known}}},
		{Name: "other", Result: report.Failed, Tests: []report.Test{{Name: "c", Status: report.Failed, Grade: report.New}}},
	}})

	out, err := dt(t, "known", "prune", "s")
	want := "s.yaml:3: removed gone, a fail entry\ns.yaml:5: removed c, a fail entry\ns.yaml:6: removed z, a fail entry\n" +
		"own.yaml: nothing to remove\nidle.yaml: left as is: no cell it grades has results in the runs read\n"
	if err != nil || out != want {
		t.Errorf("dt known prune s: %v\n%s\nwant\n%s", err, out, want)
	}
	for file, want := range map[string]string{
		"s.yaml":    entry("a", "fail") + entry("pat/*", "fail") + entry("b", "skip"),
		"own.yaml":  entry("z", "fail"),
		"idle.yaml": entry("x", "fail"),
	} {
		if got, _ := os.ReadFile(filepath.Join(root, file)); string(got) != want {
			t.Errorf("%s:\n%s\nwant\n%s", file, got, want)
		}
	}
	if _, err := dt(t, "known", "prune", "nosuch"); err == nil {
		t.Error("an unknown suite accepted")
	}
}

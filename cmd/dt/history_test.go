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

// dt known prune judges each known file of the suite and its options on the
// results of the cells the file grades, and each entry on those of cells it
// applies to, by its option, matrix value, tier and OS: the run's own tier,
// or the unit's when wider, so a pr suite's cell in a merge run is at merge,
// and the run's own OS. A known failure goes when one passed and none
// failed, as an unexpected pass does, and one that only skipped proves
// nothing. A file whose cells have no results is left as it is.
func TestKnownPrune(t *testing.T) {
	root := t.TempDir()
	entry := func(test, status, where string) string {
		return "- {test: " + test + ", status: " + status + ", fix: wontfix, category: proto, reason: r" + where + "}\n"
	}
	writeFile(t, filepath.Join(root, "test", "suites.yaml"), "version: 1\ndefaults: { format: exit, tier: pr, timeout: { test: 30m, cell: 60m } }\nsuites:\n"+
		"  s: { cmd: \"true {v}\", matrix: { v: [\"1\", \"2\"] }, known: s.yaml, options: {\n"+
		"    shares: { cmd: \"true {v}\" },\n"+
		"    own: { cmd: \"true\", matrix: {}, known: own.yaml, tier: merge },\n"+
		"    idle: { cmd: \"true\", matrix: {}, known: idle.yaml } } }\n"+
		"  other: { cmd: \"true\" }\n")
	s := entry("a", "fail", "") + entry("pat/*", "fail", "") + entry("gone", "fail", "") + entry("b", "skip", "") + entry("c", "fail", "") +
		entry("z", "fail", "") + entry("absent", "fail", "") + entry("w", "fail", ", where: {option: [shares]}") +
		entry("v2", "fail", ", where: {v: [\"2\"]}") + entry("mac", "fail", ", where: {os: [macos]}") + entry("ws", "fail", ", where: {option: [shares]}") + entry("up", "fail", "") +
		entry("lin", "fail", ", where: {os: [linux]}") + entry("tp", "fail", ", where: {tier: [pr]}")
	own := entry("z", "fail", "") + entry("tm", "fail", ", where: {tier: [merge]}")
	writeFile(t, filepath.Join(root, "s.yaml"), s)
	writeFile(t, filepath.Join(root, "own.yaml"), own)
	writeFile(t, filepath.Join(root, "idle.yaml"), entry("x", "fail", ""))
	t.Chdir(root)
	passed := func(names ...string) []report.Test {
		var out []report.Test
		for _, n := range names {
			out = append(out, report.Test{Name: n, Status: report.Passed})
		}
		return out
	}
	writeRuns(t, root, report.Run{Name: "20261019T100000Z-aaaaaaaa", Tier: "pr", OS: "darwin", Cells: []report.Cell{
		{Name: "s/1", Result: report.Passed, Tests: append(passed("gone", "z", "w", "mac"),
			report.Test{Name: "a", Status: report.Skipped, RawStatus: report.Failed, Grade: report.Known},
			report.Test{Name: "b", Status: report.Skipped, Grade: report.ExpectedSkip},
			report.Test{Name: "c", Status: report.Other, RawStatus: report.Skipped, Grade: report.UnexpectedSkip},
			report.Test{Name: "up", Status: report.Failed, RawStatus: report.Passed, Grade: report.UnexpectedPass},
		)},
		{Name: "s/2", Result: report.Passed, Tests: passed("v2")},
		{Name: "s/shares/1", Result: report.Failed, Tests: append(passed("ws"), report.Test{Name: "pat/x", Status: report.Failed, Grade: report.New})},
		{Name: "s/own", Result: report.Passed, Tests: append(passed("tm"), report.Test{Name: "z", Status: report.Skipped, RawStatus: report.Failed, Grade: report.Known})},
		{Name: "other", Result: report.Failed, Tests: []report.Test{{Name: "c", Status: report.Failed, Grade: report.New}}},
	}}, report.Run{Name: "20261019T110000Z-bbbbbbbb", Tier: "merge", OS: "linux", Cells: []report.Cell{
		{Name: "s/1", Result: report.Passed, Tests: passed("lin", "tp")},
	}})

	out, err := dt(t, "known", "prune", "s")
	want := "s.yaml:3: removed gone, a fail entry\ns.yaml:6: removed z, a fail entry\n" +
		"s.yaml:9: removed v2, a fail entry\ns.yaml:10: removed mac, a fail entry\ns.yaml:11: removed ws, a fail entry\ns.yaml:12: removed up, a fail entry\ns.yaml:13: removed lin, a fail entry\n" +
		"own.yaml:2: removed tm, a fail entry\nidle.yaml: left as is: no cell it grades has results in the runs read\n"
	if err != nil || out != want {
		t.Errorf("dt known prune s: %v\n%s\nwant\n%s", err, out, want)
	}
	for file, want := range map[string]string{
		"s.yaml": entry("a", "fail", "") + entry("pat/*", "fail", "") + entry("b", "skip", "") + entry("c", "fail", "") + entry("absent", "fail", "") + entry("w", "fail", ", where: {option: [shares]}") +
			entry("tp", "fail", ", where: {tier: [pr]}"),
		"own.yaml":  entry("z", "fail", ""),
		"idle.yaml": entry("x", "fail", ""),
	} {
		if got, _ := os.ReadFile(filepath.Join(root, file)); string(got) != want {
			t.Errorf("%s:\n%s\nwant\n%s", file, got, want)
		}
	}
	if _, err := dt(t, "known", "prune", "nosuch"); err == nil {
		t.Error("an unknown suite accepted")
	}
}

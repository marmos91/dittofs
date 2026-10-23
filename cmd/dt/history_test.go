package main

import (
	"encoding/json"
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

// dt plan --estimate gives each planned cell its p50, not its p90, to a tenth
// of a minute, over the runs dt history reads, by the cell's name, so each
// shard counts as the runner it takes. A cell with no results there says so
// and counts in no sum, and the tier's runner-minutes are the sum of the
// others', the same in text and in JSON. --ci and --last only say which runs
// it reads.
func TestPlanEstimate(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "test", "suites.yaml"), "version: 1\ndefaults: { format: exit, tier: pr, timeout: { test: 1m, cell: 10m } }\nsuites:\n"+
		"  a: { cmd: \"true\" }\n  s: { cmd: \"true {test}\", tests: \"t/*.sh\", shards: 2 }\n  b: { cmd: \"true\" }\n")
	writeFile(t, filepath.Join(root, "t", "1.sh"), "")
	writeFile(t, filepath.Join(root, "t", "2.sh"), "")
	t.Chdir(root)
	cell := func(name string, took time.Duration) report.Cell {
		return report.Cell{Name: name, Result: report.Passed, Took: took}
	}
	writeRuns(t, root,
		report.Run{Name: "20261019T100000Z-aaaaaaaa", Cells: []report.Cell{cell("a", time.Minute), cell("s/shard-1", 100*time.Second), cell("s/shard-2", 5*time.Minute)}},
		report.Run{Name: "20261019T110000Z-bbbbbbbb", Cells: []report.Cell{cell("a", 2*time.Minute)}},
		report.Run{Name: "20261019T120000Z-cccccccc", Cells: []report.Cell{cell("a", 10*time.Minute)}},
	)

	out, err := dt(t, "plan", "--tier", "pr", "--estimate")
	for _, want := range []string{
		"a          ubuntu  true      10m      2.0m ",
		"s/shard-1  ubuntu  true      10m      1.7m ",
		"s/shard-2  ubuntu  true      10m      5.0m ",
		"b          ubuntu  true      10m      no history ",
		"\nrunner-minutes: 8.7, the sum of 3 cells' p50; cells with no history: 1\n",
	} {
		if err != nil || !strings.Contains(out, want) {
			t.Errorf("dt plan --estimate: %v\n%s\nwant %q", err, out, want)
		}
	}

	out, err = dt(t, "plan", "--tier", "pr", "--estimate", "--json")
	var got struct {
		Cells []struct {
			Name string          `json:"name"`
			P50  json.RawMessage `json:"p50_minutes"`
		} `json:"cells"`
		RunnerMinutes float64 `json:"runner_minutes"`
		NoHistory     int     `json:"cells_without_history"`
	}
	if err == nil {
		err = json.Unmarshal([]byte(out), &got)
	}
	var p50 []string
	for _, c := range got.Cells {
		p50 = append(p50, c.Name+"="+string(c.P50))
	}
	if err != nil || strings.Join(p50, " ") != "a=2 s/shard-1=1.7 s/shard-2=5 b=null" || got.RunnerMinutes != 8.7 || got.NoHistory != 1 {
		t.Errorf("dt plan --estimate --json: %v\n%s", err, out)
	}

	for _, flags := range [][]string{{"--ci"}, {"--last", "2"}} {
		if _, err := dt(t, append([]string{"plan", "--tier", "pr"}, flags...)...); err == nil {
			t.Errorf("%s accepted without --estimate", flags[0])
		}
	}
}

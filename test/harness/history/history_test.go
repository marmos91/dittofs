package history

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/harness/report"
)

// summary is the summary.json a run in dir/name holds.
func summary(t *testing.T, dir string, r report.Run) []byte {
	t.Helper()
	path := filepath.Join(dir, r.Name)
	if err := r.Write(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(path, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// names is each run's name, its OS when it has one, and its cells' names and
// results.
func names(runs []report.Run) []string {
	var out []string
	for _, r := range runs {
		s := r.Name
		if r.OS != "" {
			s += " on " + r.OS
		}
		for _, c := range r.Cells {
			s += " " + c.Name + "=" + c.Result
		}
		out = append(out, s)
	}
	return out
}

// Local reads the last n runs, oldest first, and leaves out a job's re-run,
// RUN-a2, as its first attempt is the one read. A commit that looks like an
// attempt, an a and digits, is still a run's, and a file among the runs isn't
// one. No results yet is no runs.
func TestLocal(t *testing.T) {
	results := t.TempDir()
	for _, name := range []string{"20261019T100000Z-aaaaaaaa", "20261019T100000Z-aaaaaaaa-a2", "20261019T110000Z-a1234567", "20261019T120000Z-cccccccc"} {
		summary(t, results, report.Run{Name: name})
	}
	if err := os.WriteFile(filepath.Join(results, "zz-notes.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for n, want := range map[int][]string{
		2:  {"20261019T110000Z-a1234567", "20261019T120000Z-cccccccc"},
		10: {"20261019T100000Z-aaaaaaaa", "20261019T110000Z-a1234567", "20261019T120000Z-cccccccc"},
	} {
		if runs, err := Local(results, n); err != nil || !slices.Equal(names(runs), want) {
			t.Errorf("last %d: %q, %v; want %q", n, names(runs), err, want)
		}
	}
	if runs, err := Local(filepath.Join(results, "none"), 1); err != nil || len(runs) != 0 {
		t.Errorf("no results: %v, %v", runs, err)
	}
}

// CI reads the last n finished develop runs of the tests workflow, through
// every page of the API, oldest first, leaving out a pull request's and a
// cancelled one, and all of them when there are fewer. Of each run it reads
// the artifacts of the blocking and the non-blocking jobs, with suite only
// those of its cells, not the expired ones, and in each artifact only the
// first attempt's summary.json: each job's run, with its own OS, under the
// workflow run's name. Every request carries the token.
func TestCI(t *testing.T) {
	dir := t.TempDir()
	cell := func(run, name, result, os string) []byte {
		var b bytes.Buffer
		z := zip.NewWriter(&b)
		w, err := z.Create(run + "/summary.json")
		if err == nil {
			_, err = w.Write(summary(t, dir, report.Run{Name: run, OS: os, Cells: []report.Cell{{Name: name, Result: result}}}))
		}
		if err == nil {
			err = z.Close()
		}
		if err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	downloads := map[string][]byte{
		"31": cell("20261021T120100Z-33333333", "pjdfstest/badger-s3/4.1", "failed", "linux"),
		"32": cell("20261021T130000Z-33333333-a2", "pjdfstest/badger-s3/4.1", "passed", "linux"),
		"33": cell("20261021T120100Z-33333333", "pynfs/badger-s3/0", "passed", "windows"),
		"34": cell("20261021T120100Z-33333333", "pjdfstest/badger-s3/3", "passed", "linux"),
		"11": cell("20261020T221400Z-11111111", "pjdfstest/badger-s3/3", "not run", "linux"),
	}
	artifact := func(id, name string, expired bool) map[string]any {
		return map[string]any{"name": name, "expired": expired, "archive_download_url": "/download/" + id}
	}
	// Each run's artifacts, two to a page.
	artifacts := map[string][]map[string]any{
		"3": {
			artifact("33", "dt-run-pynfs-badger-s3-0-1", false),
			artifact("32", "dt-run-pjdfstest-badger-s3-4.1-2", false),
			artifact("34", "dt-non-blocking-pjdfstest-badger-s3-3-1", true),
			artifact("31", "dt-run-pjdfstest-badger-s3-4.1-1", false),
		},
		"1": {artifact("11", "dt-non-blocking-pjdfstest-badger-s3-3-1", false)},
	}
	run := func(id int, event, conclusion, sha, created string) map[string]any {
		return map[string]any{"id": id, "event": event, "conclusion": conclusion, "head_sha": strings.Repeat(sha, 40), "created_at": created}
	}
	var fetched []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		q, page := r.URL.Query(), r.URL.Query().Get("page")
		var body any
		switch p := strings.TrimPrefix(r.URL.Path, "/repos/o/r/actions/"); {
		case p == "workflows/tests.yml/runs" && q.Get("branch") == "develop" && q.Get("status") == "completed":
			pages := [][]any{
				{run(4, "push", "cancelled", "4", "2026-10-21T12:30:00Z"), run(3, "push", "failure", "3", "2026-10-21T12:00:00Z"), run(2, "pull_request", "success", "2", "2026-10-21T11:00:00Z")},
				{run(1, "schedule", "success", "1", "2026-10-20T22:13:00Z")},
				{},
			}
			body = map[string]any{"workflow_runs": pages[min(int(page[0]-'1'), 2)]}
		case strings.HasPrefix(p, "runs/") && strings.HasSuffix(p, "/artifacts"):
			all := artifacts[strings.TrimSuffix(strings.TrimPrefix(p, "runs/"), "/artifacts")]
			from := 2 * (int(page[0]-'0') - 1)
			body = map[string]any{"total_count": len(all), "artifacts": all[min(from, len(all)):min(from+2, len(all))]}
		case strings.HasPrefix(r.URL.Path, "/download/"):
			id := strings.TrimPrefix(r.URL.Path, "/download/")
			fetched = append(fetched, id)
			_, _ = w.Write(downloads[id])
			return
		default:
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()
	t.Setenv("GITHUB_API_URL", srv.URL)
	t.Setenv("GITHUB_REPOSITORY", "o/r")
	t.Setenv("GITHUB_TOKEN", "tok")
	// The archive's address is the server's own, as the API gives it.
	for id := range artifacts {
		for _, a := range artifacts[id] {
			a["archive_download_url"] = srv.URL + a["archive_download_url"].(string)
		}
	}

	for _, c := range []struct {
		suite   string
		n       int
		want    []string
		fetched string
	}{
		{"pjdfstest", 2, []string{
			"20261020T221300Z-11111111 on linux pjdfstest/badger-s3/3=not run",
			"20261021T120000Z-33333333 on linux pjdfstest/badger-s3/4.1=failed",
		}, "11 32 31"},
		{"", 1, []string{
			"20261021T120000Z-33333333 on windows pynfs/badger-s3/0=passed",
			"20261021T120000Z-33333333 on linux pjdfstest/badger-s3/4.1=failed",
		}, "33 32 31"},
		{"pynfs", 5, []string{"20261021T120000Z-33333333 on windows pynfs/badger-s3/0=passed"}, "33"},
	} {
		fetched = nil
		runs, err := CI(context.Background(), c.suite, c.n)
		if err != nil || !slices.Equal(names(runs), c.want) {
			t.Errorf("suite %q, last %d: %q, %v; want %q", c.suite, c.n, names(runs), err, c.want)
		}
		if got := strings.Join(fetched, " "); got != c.fetched {
			t.Errorf("suite %q: downloaded %s, want %s", c.suite, got, c.fetched)
		}
	}

	t.Setenv("GITHUB_TOKEN", "")
	if _, err := CI(context.Background(), "", 1); err == nil || !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Errorf("without a token: %v", err)
	}
}

// Each suite counts its tests' results by what they did, a known failure as a
// failure and an unexpected pass as a pass, and a pass on a retry as flaky
// too; a result with none counts in no column. Its times are its cells', and
// a run with several of its cells, or of a test's results, counts once. A
// cell skipped for needs its machine didn't meet didn't run, so it counts in
// nothing. Each test counts the
// same way, with its own times, and a Go test's title tells the packages
// apart. P50 and P90 are by nearest rank.
func TestCounts(t *testing.T) {
	test := func(name, status, raw, grade string, took time.Duration) report.Test {
		return report.Test{Name: name, Status: status, RawStatus: raw, Grade: grade, Duration: took}
	}
	flaky := test("a.t", report.Passed, "", "", 5*time.Second)
	flaky.Flaky = true
	runs := []report.Run{
		{Name: "r1", Cells: []report.Cell{
			{Name: "pjdfstest/badger-s3/3", Result: report.Failed, Took: 10 * time.Second, Tests: []report.Test{
				test("a.t", report.Passed, "", "", time.Second),
				test("b.t", report.Skipped, report.Failed, report.Known, 2*time.Second),
				test("c.t", report.Failed, report.Passed, report.UnexpectedPass, 3*time.Second),
				test("d.t", report.Other, "NotExecuted", report.UnexpectedSkip, 0),
				test("e.t", report.Other, "", report.Incomplete, 0),
			}},
		}},
		{Name: "r2", Cells: []report.Cell{
			{Name: "pjdfstest/badger-s3/3", Result: report.Passed, Took: 30 * time.Second, Tests: []report.Test{
				flaky, test("b.t", report.Skipped, report.Failed, report.Known, 4*time.Second),
			}},
			{Name: "pjdfstest/badger-s3/4.1", Result: report.Passed, Took: 40 * time.Second, Tests: []report.Test{
				test("b.t", report.Skipped, report.Failed, report.Known, 6*time.Second),
			}},
			{Name: "unit", Result: report.Passed, Took: 20 * time.Second, Tests: []report.Test{
				{Name: "TestX", Suite: []string{"m/a"}, Status: report.Passed},
				{Name: "TestX", Suite: []string{"m/b"}, Status: report.Skipped, Grade: report.Ungraded},
				{Name: "TestY", Suite: []string{"m/b"}, Status: report.Passed},
			}},
		}},
		{Name: "r3", Cells: []report.Cell{{Name: "pjdfstest/badger-s3/3", Result: report.NotRun, Took: 20 * time.Second}}},
		{Name: "r4", Cells: []report.Cell{{Name: "pjdfstest/badger-s3/3", Result: report.Skipped}}},
	}
	show := func(counts map[string]*Count) map[string]string {
		out := map[string]string{}
		for k, n := range counts {
			out[k] = fmt.Sprintf("runs %d: %d %d %d %d, %s %s", n.Runs, n.Pass, n.Fail, n.Skip, n.Flaky, n.P50(), n.P90())
		}
		return out
	}
	for name, c := range map[string]struct{ got, want map[string]string }{
		"suites": {show(Suites(runs)), map[string]string{
			"pjdfstest": "runs 3: 3 3 1 1, 20s 40s",
			"unit":      "runs 1: 2 0 1 0, 20s 20s",
		}},
		"tests": {show(Tests(runs, "pjdfstest", "")), map[string]string{
			"a.t": "runs 2: 2 0 0 1, 1s 5s",
			"b.t": "runs 2: 0 3 0 0, 4s 6s",
			"c.t": "runs 1: 1 0 0 0, 3s 3s",
			"d.t": "runs 1: 0 0 1 0, 0s 0s",
			"e.t": "runs 1: 0 0 0 0, 0s 0s",
		}},
		"one test": {show(Tests(runs, "unit", "TestX")), map[string]string{
			"m/a TestX": "runs 1: 1 0 0 0, 0s 0s",
			"m/b TestX": "runs 1: 0 0 1 0, 0s 0s",
		}},
	} {
		if fmt.Sprint(c.got) != fmt.Sprint(c.want) {
			t.Errorf("%s: %v, want %v", name, c.got, c.want)
		}
	}
}

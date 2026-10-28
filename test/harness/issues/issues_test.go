package issues

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/test/harness/report"
)

// tracker answers the calls Post makes as GitHub's REST API does: it lists
// issues by label and state a page at a time, creates, edits and comments,
// and refuses a request without the token, a write with a token that can only
// read, a body or a title longer than GitHub allows, and a comment on a locked
// issue.
type tracker struct {
	issues   []*tracked // issue N is issues[N-1]
	comments map[int][]string
	requests int
	outage   int // how many requests to come it answers 502 Bad Gateway
}

type tracked struct {
	issue
	labels []string
	locked bool
}

func newTracker(t *testing.T) (*tracker, Client) {
	g := &tracker{comments: map[int][]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.requests++
		if g.outage > 0 {
			g.outage--
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
			return
		}
		switch auth := r.Header.Get("Authorization"); {
		case auth == "Bearer read-only" && r.Method != http.MethodGet:
			http.Error(w, "Resource not accessible by integration", http.StatusForbidden)
			return
		case auth != "Bearer tok" && auth != "Bearer read-only":
			http.Error(w, "Bad credentials", http.StatusUnauthorized)
			return
		}
		var in struct {
			Title, Body, State string
			Labels             []string
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if len(in.Body) > 65536 || len([]rune(in.Title)) > 256 {
			http.Error(w, "body or title is too long", http.StatusUnprocessableEntity)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/repos/o/r/issues")
		n, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/comments"))
		if n > 0 && g.issues[n-1].locked && strings.HasSuffix(path, "/comments") {
			http.Error(w, "Issue is locked", http.StatusForbidden)
			return
		}
		var out any
		switch {
		case r.Method == http.MethodGet && path == "":
			q := r.URL.Query()
			page, _ := strconv.Atoi(q.Get("page"))
			per, _ := strconv.Atoi(q.Get("per_page"))
			all := []issue{}
			for _, is := range g.issues {
				if slices.Contains(is.labels, q.Get("labels")) && (q.Get("state") == "all" || q.Get("state") == is.State) {
					all = append(all, is.issue)
				}
			}
			out = all[min(max(page-1, 0)*per, len(all)):min(max(page, 1)*per, len(all))]
		case r.Method == http.MethodPost && path == "":
			g.issues = append(g.issues, &tracked{issue{Number: len(g.issues) + 1, Title: in.Title, Body: in.Body, State: "open"}, in.Labels, false})
			out = g.issues[len(g.issues)-1].issue
		case r.Method == http.MethodPatch && n > 0:
			is := g.issues[n-1]
			is.Body, is.State = in.Body, in.State
			out = is.issue
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/comments"):
			g.comments[n] = append(g.comments[n], in.Body)
			out = map[string]int{"id": len(g.comments[n])}
		default:
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return g, Client{API: srv.URL, Repo: "o/r", Token: "tok", Log: "https://ci.example/runs/7"}
}

// add puts an issue in the tracker, as someone or something before dt did.
func (g *tracker) add(title, state string) {
	g.issues = append(g.issues, &tracked{issue{Number: len(g.issues) + 1, Title: title, State: state}, []string{label}, false})
}

// titled are the issues titled title.
func (g *tracker) titled(title string) []*tracked {
	var out []*tracked
	for _, is := range g.issues {
		if is.Title == title {
			out = append(out, is)
		}
	}
	return out
}

func run(name, tier string, cells ...report.Cell) report.Run {
	return report.Run{Name: name, Commit: name[len(name)-8:], Branch: "develop", Tier: tier, Cells: cells}
}

func newFailure(name, message string) report.Test {
	return report.Test{Name: name, Status: report.Failed, Grade: report.New, Message: message}
}

// The develop-status issue follows develop's failing set, found again by its
// label and title among the earlier alarm's issues, past the API's first page.
// The first post creates it, closed while only a cell not run once is in the
// set. A new failure, an unexpected pass and a cell that failed with no test to
// name open it, while known, quarantined, flaky and expected-skip results never
// appear. While it stays open, a change of the alerting set adds one comment. A
// run of another tier keeps the entries of the cells it doesn't have, a cell
// that didn't run keeps its tests' entries, and a job re-run by hand counts
// from its last attempt. It closes when the set is empty. A cell that didn't
// run alerts only on its second run in a row, remembered while the issue is
// closed. Closed by hand, it drops the entries it held, as one whose cell is
// gone.
func TestStatusIssue(t *testing.T) {
	g, c := newTracker(t)
	for range 100 {
		g.add("🔴 develop CI is RED", "closed")
	}
	g.add("something a person labelled", "open")
	post := func(runs ...report.Run) *tracked {
		t.Helper()
		if err := c.Post(context.Background(), runs, nil, io.Discard); err != nil {
			t.Fatal(err)
		}
		if found := g.titled(statusTitle); len(found) != 1 {
			t.Fatalf("%d develop-status issues", len(found))
		}
		return g.titled(statusTitle)[0]
	}
	// check checks the issue's state, its comments and that its table has
	// exactly rows.
	check := func(step string, is *tracked, state string, comments int, rows ...string) {
		t.Helper()
		if is.State != state || len(g.comments[is.Number]) != comments || !slices.Equal(is.labels, []string{label}) {
			t.Errorf("%s: %s with %d comments and labels %v, want %s with %d", step, is.State, len(g.comments[is.Number]), is.labels, state, comments)
		}
		if strings.Count(is.Body, "\n| `") != len(rows) {
			t.Errorf("%s: want only %q in\n%s", step, rows, is.Body)
		}
		for _, row := range rows {
			if !strings.Contains(is.Body, row) {
				t.Errorf("%s: no row %q in\n%s", step, row, is.Body)
			}
		}
	}
	green := func(name string) report.Cell { return report.Cell{Name: name, Result: report.Passed} }
	notRun := func(name string) report.Cell {
		return report.Cell{Name: name, Result: report.NotRun, Note: "the bucket refused a write"}
	}

	is := post(run("20261022T090000Z-aaaaaaa0", "merge", notRun("pynfs/badger-s3/0")))
	check("the first post, a cell not run once", is, "closed", 0, "| `pynfs/badger-s3/0` |  | not run | 1 |  |")

	pjdfstest := report.Cell{Name: "pjdfstest/badger-s3/4.1", Result: report.Failed, Tests: []report.Test{
		newFailure("chmod/12.t", "not ok 7"),
		newFailure("chmod/12.t", "not ok 8"),
		{Name: "unlink/14.t", Status: report.Skipped, RawStatus: report.Failed, Grade: report.Known},
		{Name: "rename/09.t", Status: report.Passed, Flaky: true, Retries: 1},
		{Name: "link/00.t", Status: report.Skipped, RawStatus: report.Failed, Grade: "quarantined"},
		{Name: "mkfifo/00.t", Status: report.Skipped, Grade: report.ExpectedSkip},
		{Name: "open/00.t", Status: report.Failed, RawStatus: report.Passed, Grade: report.UnexpectedPass},
	}}
	unit := report.Cell{Name: "unit", Result: report.Failed, Note: "the server exited"}
	is = post(run("20261022T100000Z-aaaaaaa1", "merge", pjdfstest, unit, green("pynfs/badger-s3/0")))
	check("a red merge run", is, "open", 0,
		"| `pjdfstest/badger-s3/4.1` | `chmod/12.t` | new | 1 |  |",
		"| `pjdfstest/badger-s3/4.1` | `open/00.t` | unexpected_pass | 1 |  |",
		"| `unit` |  | failed | 1 |  |")

	nfstest := report.Cell{Name: "nfstest/badger-s3/v3", Result: report.Failed, Tests: []report.Test{newFailure("cache: open", "FAIL")}}
	is = post(run("20261022T220000Z-aaaaaaa2", "nightly", pjdfstest, nfstest))
	check("a nightly run adds a failure", is, "open", 1,
		"| `nfstest/badger-s3/v3` | `cache: open` | new | 1 |  |",
		"| `pjdfstest/badger-s3/4.1` | `chmod/12.t` | new | 2 | #103 |",
		"| `pjdfstest/badger-s3/4.1` | `open/00.t` | unexpected_pass | 2 |  |",
		"| `unit` |  | failed | 1 |  |")
	if comment := g.comments[is.Number][0]; comment != "The failing set changed at aaaaaaa2 (https://ci.example/runs/7):\n\n- now failing: `nfstest/badger-s3/v3` `cache: open`, new\n" {
		t.Errorf("comment:\n%s", comment)
	}

	is = post(run("20261023T100000Z-aaaaaaa3", "merge", green("pjdfstest/badger-s3/4.1"), green("unit"), green("pynfs/badger-s3/0")))
	check("a green merge run, a nightly failure left", is, "open", 2, "| `nfstest/badger-s3/v3` | `cache: open` | new | 1 |  |")
	if comment := g.comments[is.Number][1]; !strings.Contains(comment, "- no longer failing: `pjdfstest/badger-s3/4.1` `chmod/12.t`, new\n") || !strings.Contains(comment, "- no longer failing: `unit`, failed\n") {
		t.Errorf("comment:\n%s", comment)
	}

	is = post(run("20261023T220000Z-aaaaaaa4", "nightly", notRun("nfstest/badger-s3/v3")))
	check("a nightly run where the failing cell didn't run", is, "open", 2,
		"| `nfstest/badger-s3/v3` | `cache: open` | new | 1 |  |",
		"| `nfstest/badger-s3/v3` |  | not run | 1 |  |")
	is = post(run("20261024T220000Z-aaaaaaa5", "nightly", nfstest))
	check("the cell runs again, still failing", is, "open", 2, "| `nfstest/badger-s3/v3` | `cache: open` | new | 2 | #104 |")

	rerun := run("20261025T230000Z-aaaaaaa6-a2", "nightly", green("nfstest/badger-s3/v3"))
	is = post(rerun, run("20261025T220000Z-aaaaaaa6", "nightly", notRun("nfstest/badger-s3/v3")))
	check("a nightly run that passes on its job's re-run", is, "closed", 2)
	if !strings.HasPrefix(is.Body, "develop is green.") {
		t.Errorf("body:\n%s", is.Body)
	}

	is = post(run("20261026T100000Z-aaaaaaa7", "merge", notRun("pynfs/badger-s3/0")))
	check("a cell not run once", is, "closed", 2, "| `pynfs/badger-s3/0` |  | not run | 1 |  |")
	is = post(run("20261026T110000Z-aaaaaaa8", "merge", notRun("pynfs/badger-s3/0")))
	check("a cell not run twice", is, "open", 2, "| `pynfs/badger-s3/0` |  | not run | 2 |  |")

	is.State = "closed"
	is = post(run("20261026T120000Z-aaaaaaa9", "merge", green("pynfs/badger-s3/1")))
	check("closed by hand", is, "closed", 2)
}

// A new failure that fails 2 runs in a row of its cell gets an issue of its
// own, labelled and titled by its cell and test, with its failing line, the
// commit and the log; one with an issue already open gets a comment there
// instead. Either is linked from the develop-status issue and posted once. An
// unexpected pass gets none, and a tier run with more than 10 new failing
// tests posts none.
func TestTestIssues(t *testing.T) {
	g, c := newTracker(t)
	g.add("pjdfstest/badger-s3/4.1 open/00.t fails on develop", "open")
	g.add("pjdfstest/badger-s3/4.1 open/01.t fails on develop", "closed")
	tests := []report.Test{
		newFailure("chmod/12.t", "\n  chmod.t: not ok 7 | mode 0644  \nthe second line"),
		newFailure("open/00.t", "not ok 1"),
		newFailure("open/01.t", "not ok 1"),
		{Name: "rename/00.t", Status: report.Failed, RawStatus: report.Passed, Grade: report.UnexpectedPass},
	}
	for i := range 7 {
		tests = append(tests, newFailure("mkdir/0"+strconv.Itoa(i)+".t", "not ok"))
	}
	tests[len(tests)-1].Message = strings.Repeat("y", 70000) // one line, longer than an issue may be
	cell := report.Cell{Name: "pjdfstest/badger-s3/4.1", Result: report.Failed, Tests: tests}
	post := func(r report.Run) {
		t.Helper()
		if err := c.Post(context.Background(), []report.Run{r}, nil, io.Discard); err != nil {
			t.Fatal(err)
		}
	}

	post(run("20261022T100000Z-bbbbbbb1", "merge", cell))
	if len(g.issues) != 3 || len(g.comments) != 0 {
		t.Fatalf("after one failing run: %d issues, comments %v", len(g.issues), g.comments)
	}
	post(run("20261022T110000Z-bbbbbbb2", "merge", cell))
	chmod := g.titled("pjdfstest/badger-s3/4.1 chmod/12.t fails on develop")
	if len(chmod) != 1 || !slices.Equal(chmod[0].labels, []string{label}) || chmod[0].Body != "`chmod/12.t` in `pjdfstest/badger-s3/4.1` has failed 2 runs in a row on develop.\n\n"+
		"- Commit: bbbbbbb2\n- Log: https://ci.example/runs/7\n\nThe failing line:\n\n    chmod.t: not ok 7 | mode 0644\n" {
		t.Fatalf("chmod/12.t's issue: %+v", chmod)
	}
	if len(g.issues) != 3+1+1+7 || len(g.titled("pjdfstest/badger-s3/4.1 open/01.t fails on develop")) != 2 || len(g.titled("pjdfstest/badger-s3/4.1 rename/00.t fails on develop")) != 0 {
		t.Errorf("%d issues, want one for each new failure without one open", len(g.issues))
	}
	if comments := g.comments[1]; len(comments) != 1 || !strings.Contains(comments[0], "`open/00.t` in `pjdfstest/badger-s3/4.1` has failed 2 runs in a row") {
		t.Errorf("open/00.t's issue's comments: %q", comments)
	}
	status := g.titled(statusTitle)[0]
	for _, row := range []string{"| `chmod/12.t` | new | 2 | #" + strconv.Itoa(chmod[0].Number) + " |", "| `open/00.t` | new | 2 | #1 |"} {
		if !strings.Contains(status.Body, row) {
			t.Errorf("no row %q in\n%s", row, status.Body)
		}
	}

	issues, comments := len(g.issues), len(g.comments[1])
	post(run("20261022T120000Z-bbbbbbb3", "merge", cell))
	if len(g.issues) != issues || len(g.comments[1]) != comments {
		t.Errorf("a third failing run posted again: %d issues, %d comments", len(g.issues), len(g.comments[1]))
	}

	cell.Name, cell.Tests = "unit", append(cell.Tests, newFailure("TestEleventh", "boom"))
	post(run("20261022T130000Z-bbbbbbb4", "merge", cell))
	post(run("20261022T140000Z-bbbbbbb5", "merge", cell))
	if len(g.issues) != issues {
		t.Errorf("11 new failing tests opened %d issues", len(g.issues)-issues)
	}
	post(run("20261022T150000Z-bbbbbbb6", "merge", report.Cell{Name: "pynfs/badger-s3/0", Result: report.Passed}))
	if len(g.issues) != issues {
		t.Errorf("a run without the failing cell opened %d issues", len(g.issues)-issues)
	}
}

// Only develop's merge and nightly runs post: a run of another branch or
// another tier is refused before anything is asked of the API.
func TestPostRefusesOtherRuns(t *testing.T) {
	g, c := newTracker(t)
	branch := run("20261022T100000Z-ccccccc1", "merge")
	branch.Branch = "feat/x"
	for _, r := range []report.Run{branch, run("20261022T100000Z-ccccccc2", "pr")} {
		if err := c.Post(context.Background(), []report.Run{run("20261022T090000Z-ccccccc0", "merge"), r}, nil, io.Discard); err == nil || g.requests != 0 {
			t.Errorf("%s's %s run: %v, %d requests", r.Branch, r.Tier, err, g.requests)
		}
	}
}

// A test's issue GitHub refuses, as a comment on a locked issue, doesn't stop
// the post: the other tests' issues and the develop-status issue are still
// written, and Post returns the error. A title longer than GitHub's 256
// characters, as a Go subtest's under its package path, is cut to fit.
func TestTestIssueErrorsDontStopThePost(t *testing.T) {
	g, c := newTracker(t)
	g.add("unit pkg TestLocked fails on develop", "open")
	g.issues[0].locked = true
	pkg := "github.com/marmos91/dittofs/pkg/block/engine"
	long := "TestLong/" + strings.Repeat("a_case_", 40)
	cell := report.Cell{Name: "unit", Result: report.Failed, Tests: []report.Test{
		{Name: "TestLocked", Suite: []string{"pkg"}, Status: report.Failed, Grade: report.New},
		{Name: long, Suite: []string{pkg}, Status: report.Failed, Grade: report.New},
	}}
	if err := c.Post(context.Background(), []report.Run{run("20261022T100000Z-hhhhhhh1", "merge", cell)}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := c.Post(context.Background(), []report.Run{run("20261022T110000Z-hhhhhhh2", "merge", cell)}, nil, io.Discard); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Errorf("a refused comment: %v", err)
	}
	status := g.titled(statusTitle)[0]
	for _, row := range []string{"| `unit` | `pkg TestLocked` | new | 2 |  |", " " + long + "` | new | 2 | #3 |"} {
		if !strings.Contains(status.Body, row) {
			t.Errorf("no row %q in\n%s", row, status.Body)
		}
	}
	if title := g.issues[2].Title; !strings.HasPrefix(title, "unit "+pkg+" "+long[:100]) || len([]rune(title)) > 256 {
		t.Errorf("the long test's issue: %q", title)
	}
}

// A re-run of a workflow run posts again, and the run counts once: the
// develop-status issue keeps the run it last posted, and a post of that run
// changes nothing. The next run counts.
func TestRerunCountsOnce(t *testing.T) {
	g, c := newTracker(t)
	cell := report.Cell{Name: "unit", Result: report.Failed, Tests: []report.Test{newFailure("TestA", "boom")}}
	post := func(workflowRun, name, runs string, issues int) {
		t.Helper()
		c.Run = workflowRun
		if err := c.Post(context.Background(), []report.Run{run(name, "merge", cell)}, nil, io.Discard); err != nil {
			t.Fatal(err)
		}
		if body := g.titled(statusTitle)[0].Body; !strings.Contains(body, "| `unit` | `TestA` | new | "+runs+" |") || len(g.issues) != issues {
			t.Errorf("run %s, %s: %d issues and\n%s", workflowRun, name, len(g.issues), body)
		}
	}
	post("7", "20261022T100000Z-iiiiiii1", "1", 1)
	post("7", "20261022T110000Z-iiiiiii1-a2", "1", 1)
	post("8", "20261022T120000Z-iiiiiii2", "2", 2)
}

// A post GitHub refuses fails, as a token that can't write issues makes it,
// so a lost alert shows as a failed job.
func TestPostFailsWhenGitHubRefuses(t *testing.T) {
	_, c := newTracker(t)
	red := func(name, test string) []report.Run {
		return []report.Run{run(name, "merge", report.Cell{Name: "unit", Result: report.Failed, Tests: []report.Test{newFailure(test, "boom")}})}
	}
	if err := c.Post(context.Background(), red("20261022T100000Z-eeeeeee1", "TestA"), nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	c.Token = "read-only"
	if err := c.Post(context.Background(), red("20261022T110000Z-eeeeeee2", "TestB"), nil, io.Discard); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("a refused edit: %v", err)
	}
}

// A request GitHub answers with a 5xx is sent once more, so a passing outage
// doesn't lose a post; a second 5xx in a row fails it.
func TestPostRetriesA5xxOnce(t *testing.T) {
	g, c := newTracker(t)
	red := func(name string) []report.Run {
		return []report.Run{run(name, "merge", report.Cell{Name: "unit", Result: report.Failed})}
	}
	g.outage = 1
	if err := c.Post(context.Background(), red("20261022T100000Z-ggggggg1"), nil, io.Discard); err != nil || len(g.titled(statusTitle)) != 1 {
		t.Fatalf("one 502: %v, %d develop-status issues", err, len(g.titled(statusTitle)))
	}
	g.outage = 2
	if err := c.Post(context.Background(), red("20261022T110000Z-ggggggg2"), nil, io.Discard); err == nil || !strings.Contains(err.Error(), "502") {
		t.Errorf("two 502s in a row: %v", err)
	}
}

// However many tests fail, the develop-status issue's body stays within what
// GitHub accepts: it keeps the first entries by cell and test, so the same
// ones run after run, and says how many it left out.
func TestStatusIssueKeepsItsBodyInBounds(t *testing.T) {
	g, c := newTracker(t)
	cell := report.Cell{Name: "unit", Result: report.Failed}
	for i := 299; i >= 0; i-- {
		cell.Tests = append(cell.Tests, report.Test{Name: "Test" + strconv.Itoa(1000+i) + "/" + strings.Repeat("x", 200), Suite: []string{"github.com/marmos91/dittofs/pkg/block/engine"}, Status: report.Failed, Grade: report.New})
	}
	if err := c.Post(context.Background(), []report.Run{run("20261022T100000Z-ddddddd1", "merge", cell)}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	body := g.titled(statusTitle)[0].Body
	if !strings.Contains(body, "and 250 more, not kept") || !strings.Contains(body, "Test1000/") || !strings.Contains(body, "Test1049/") || strings.Contains(body, "Test1050/") {
		t.Errorf("body:\n%s", body)
	}
}

// The entries the body keeps are the alerting ones first, so a set the cut
// shortens still alerts when it should: 50 cells not run once that sort before
// a new failure leave the issue open.
func TestStatusIssueKeepsAlertingEntries(t *testing.T) {
	g, c := newTracker(t)
	cells := []report.Cell{{Name: "unit", Result: report.Failed, Tests: []report.Test{newFailure("TestA", "boom")}}}
	for i := range 50 {
		cells = append(cells, report.Cell{Name: "cthon04/" + strconv.Itoa(10+i), Result: report.NotRun})
	}
	if err := c.Post(context.Background(), []report.Run{run("20261022T100000Z-jjjjjjj1", "merge", cells...)}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if is := g.titled(statusTitle)[0]; is.State != "open" || !strings.Contains(is.Body, "| `unit` | `TestA` | new | 1 |") || !strings.Contains(is.Body, "and 1 more") {
		t.Errorf("%s:\n%s", is.State, is.Body)
	}
}

// The environment gives the API, the repository, DittoFS's by default, the
// token, which posting needs, and the workflow run, with its page as the log.
func TestFromEnv(t *testing.T) {
	t.Setenv("GITHUB_API_URL", "")
	t.Setenv("GITHUB_REPOSITORY", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GITHUB_RUN_ID", "")
	if _, err := FromEnv(); err == nil {
		t.Error("no token accepted")
	}
	t.Setenv("GITHUB_TOKEN", "tok")
	if c, err := FromEnv(); err != nil || c != (Client{API: "https://api.github.com", Repo: "marmos91/dittofs", Token: "tok"}) {
		t.Errorf("%+v, %v", c, err)
	}
	t.Setenv("GITHUB_SERVER_URL", "https://github.com")
	t.Setenv("GITHUB_RUN_ID", "42")
	if c, err := FromEnv(); err != nil || c.Run != "42" || c.Log != "https://github.com/marmos91/dittofs/actions/runs/42" {
		t.Errorf("%+v, %v", c, err)
	}
}

// A run's commit status is success, failure or error as dt's exit status
// says, under the name asked for, on the commit's full hash, with the run's
// test counts and its log; only develop's merge and nightly runs set one.
func TestCheck(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "Bad credentials", http.StatusUnauthorized)
			return
		}
		var in struct {
			State, Context, Description string
			TargetURL                   string `json:"target_url"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		got = append(got, fmt.Sprintf("%s %s %s|%s|%s|%s", r.Method, r.URL.Path, in.State, in.Context, in.Description, in.TargetURL))
		_, _ = io.WriteString(w, "{}")
	}))
	t.Cleanup(srv.Close)
	c := Client{API: srv.URL, Repo: "o/r", Token: "tok", Log: "https://ci.example/runs/7"}
	sha := strings.Repeat("ab", 20)
	cells := []report.Cell{
		{Name: "system/dedicated", Result: report.Failed, Tests: []report.Test{
			{Name: "00-a.sh", Status: report.Passed},
			{Name: "01-b.sh", Status: report.Passed},
			newFailure("02-c.sh", "exit 1"),
			{Name: "03-d.sh", Status: report.Skipped, RawStatus: report.Failed, Grade: report.Known},
			{Name: "04-e.sh", Status: report.Failed, RawStatus: report.Passed, Grade: report.UnexpectedPass},
			{Name: "05-f.sh", Status: report.Other, Grade: report.Incomplete},
		}},
		{Name: "system/other", Result: report.NotRun},
	}
	for exit, state := range map[int]string{0: "success", 1: "failure", 2: "error"} {
		got = nil
		r := run("20261028T100000Z-abababab", "merge", cells...)
		r.Exit = exit
		if err := c.Check(context.Background(), r, sha, "system (dedicated)", io.Discard); err != nil {
			t.Fatal(err)
		}
		want := "POST /repos/o/r/statuses/" + sha + " " + state + "|system (dedicated)|2 passed, 2 new failures, 1 known, 1 cells not run|https://ci.example/runs/7"
		if !slices.Equal(got, []string{want}) {
			t.Errorf("exit %d: %q, want %q", exit, got, want)
		}
	}
	got = nil
	branch := run("20261028T100000Z-abababab", "merge")
	branch.Branch = "feat/x"
	for _, r := range []report.Run{branch, run("20261028T100000Z-abababab", "pr")} {
		if err := c.Check(context.Background(), r, sha, "system (dedicated)", io.Discard); err == nil || len(got) != 0 {
			t.Errorf("%s's %s run: %v, %q", r.Branch, r.Tier, err, got)
		}
	}
}

package run

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/harness/registry"
	"github.com/marmos91/dittofs/test/harness/report"
)

// summary is what the tests read back from a run's summary.json.
type summary struct {
	Results struct {
		Tests []struct {
			Name, Status, Message string
			Suite                 []string
			Extra                 struct{ Grade string }
		}
		Environment struct{ Commit, OSPlatform string }
		Extra       struct {
			Run, Tier, In string
			Exit          int
			Cells         []struct{ Suite, Cell, Result, Note, Log string }
		}
	}
}

func readSummary(t *testing.T, file string) summary {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var s summary
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// cases lists the summary's tests as "suite > name: status", with the grade
// in brackets and the message after a |.
func (s summary) cases() []string {
	var out []string
	for _, tc := range s.Results.Tests {
		line := strings.Join(tc.Suite, " ") + " > " + tc.Name + ": " + tc.Status
		if tc.Extra.Grade != "" {
			line += " [" + tc.Extra.Grade + "]"
		}
		if tc.Message != "" {
			line += " | " + tc.Message
		}
		out = append(out, line)
	}
	return out
}

// runIn runs target at tier pr and returns the report.
func runIn(t *testing.T, root string, r *registry.Registry, target string) Report {
	t.Helper()
	rep, _ := runTarget(t, root, r, target, false)
	return rep
}

// summary.json is written when the run starts, so a run with no cell has
// one, and rewritten after every cell with the cells finished so far: here
// the second cell finds the first one's results in it. It records the commit,
// the tier the run was asked for, where dt ran, the exit status so far, and
// each cell's log, unless the cell stopped before it had one.
func TestSummaryAfterEveryCell(t *testing.T) {
	t.Setenv("DT_IN_CONTAINER", "1")
	root, r := repo(t, `groups: { g: [first, second, away] }
suites:
  first: { cmd: "false" }
  second: { cmd: { sh: "cp {out}/../summary.json {out}/seen" } }
  away: { cmd: "true", needs: [windows] }
`, nil)
	rep, err := Run(context.Background(), nil, Options{Root: root, Results: filepath.Join(root, "results"), Out: io.Discard, Tier: "pr"})
	if err != nil {
		t.Fatal(err)
	}
	if s := readSummary(t, filepath.Join(rep.Dir, "summary.json")); len(s.Results.Extra.Cells) != 0 || s.Results.Extra.Exit != 2 {
		t.Errorf("a run with no cell: %+v", s.Results.Extra)
	}

	cells, err := Select(root, r, "g", "", "pr", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep, err = Run(context.Background(), cells, Options{Root: root, Results: filepath.Join(root, "results"), Out: io.Discard, Tier: "pr"}); err != nil {
		t.Fatal(err)
	}
	seen := readSummary(t, filepath.Join(rep.Dir, "second", "seen"))
	if got, want := seen.cases(), []string{"first > false: failed | `false` exited 1"}; !slices.Equal(got, want) || len(seen.Results.Extra.Cells) != 1 || seen.Results.Extra.Exit != 1 {
		t.Errorf("summary before the second cell: %q, %+v", got, seen.Results.Extra)
	}
	final := readSummary(t, filepath.Join(rep.Dir, "summary.json"))
	x := final.Results.Extra
	if len(x.Cells) != 3 || x.Cells[0].Result != Failed || x.Cells[0].Log != "logs/first.log" || x.Cells[1].Result != Passed || x.Cells[2].Result != Skipped || x.Cells[2].Log != "" {
		t.Errorf("cells %+v", x.Cells)
	}
	env := final.Results.Environment
	if x.Run != filepath.Base(rep.Dir) || !strings.Contains(x.Run, "-"+env.Commit) || x.Tier != "pr" || x.In != "container" || x.Exit != 1 || env.OSPlatform != runtime.GOOS {
		t.Errorf("run fields %+v, %+v", x, env)
	}
}

// An exit suite with tests has a result for each test: passed, or failed with
// why, a timeout among them. A test that ran nothing has none.
func TestExitResultsPerTest(t *testing.T) {
	root, r := repo(t, `suites:
  s: { cmd: "sh {test}", tests: "t/*.sh", timeout: { test: 1s } }
`, map[string]string{"t/a.sh": "exit 0\n", "t/b.sh": "exit 3\n", "t/c.sh": "sleep 30\n", "t/d.sh": "exit 75\n"})
	rep := runIn(t, root, r, "s")
	want := []string{
		"s > t/a.sh: passed",
		"s > t/b.sh: failed | `sh t/b.sh` exited 3",
		"s > t/c.sh: failed | timed out after 1s",
	}
	if got := readSummary(t, filepath.Join(rep.Dir, "summary.json")).cases(); !slices.Equal(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

// Without tests, each command of an exit suite is a test case, named by its
// first line as the registry writes it: passed, failed, or skipped when there
// is no {base} to compare with, which is ungraded in a suite without a known
// file and left to the known file in one with it. A command that ran nothing
// has no result.
func TestExitResultsPerCommand(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("GITHUB_EVENT_NAME", "schedule")
	root, r := repo(t, `groups: { g: [lint, listed] }
suites:
  lint: { cmd: [ "true", "true {out}", "false", "no-such-check {base}", { sh: "exit 75" }, { sh: "echo two\n# more" } ] }
  listed: { cmd: "no-such-check {base}", known: known.yaml }
`, map[string]string{"known.yaml": "[]\n"})
	rep := runIn(t, root, r, "g")
	want := []string{
		"lint > true: passed",
		"lint > true {out}: passed",
		"lint > false: failed | `false` exited 1",
		"lint > no-such-check {base}: skipped [ungraded] | no change to check",
		"lint > echo two: passed",
		"listed > no-such-check {base}: skipped | no change to check",
	}
	if got := readSummary(t, filepath.Join(rep.Dir, "summary.json")).cases(); !slices.Equal(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
	if rep.Outcomes[0].Note != "`false` exited 1, `exit 75` exited 75: it ran nothing" {
		t.Errorf("the cell's note changed: %q", rep.Outcomes[0].Note)
	}
}

// A go-json cell has a result for each test and subtest, under its package: a
// failure with what the test printed, a skip ungraded and with its reason. A
// test whose binary crashed before its result is other, with its output, and
// its package has no failure of its own besides.
func TestGoJSONResults(t *testing.T) {
	files := map[string]string{"go.mod": goModule["go.mod"], "a/a_test.go": goModule["a/a_test.go"], "p/p_test.go": `package p

import (
	"testing"
	"time"
)

func TestPanicky(t *testing.T) {
	go func() { panic("boom") }()
	time.Sleep(time.Second)
}
`}
	_, _, dir := goRun(t, "suites:\n  g: { cmd: go test ./..., format: go-json }\n", files)
	got := readSummary(t, filepath.Join(dir, "summary.json")).cases()
	slices.Sort(got)
	want := []string{
		"g example.com/m/a > TestFail: failed |     a_test.go:6: broke",
		"g example.com/m/a > TestParent/bad: failed |     a_test.go:10: sub broke",
		"g example.com/m/a > TestParent/good: passed",
		"g example.com/m/a > TestParent: failed",
		"g example.com/m/a > TestPass: passed",
		"g example.com/m/a > TestSkip: skipped [ungraded] |     a_test.go:7: nothing to check here",
		"g example.com/m/p > TestPanicky: other | panic: boom",
	}
	if len(got) != len(want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	for i := range want {
		// A crash's message goes on with its stack.
		if got[i] != want[i] && !strings.HasPrefix(got[i], want[i]+"\n") {
			t.Errorf("got %q\nwant %q", got[i], want[i])
		}
	}
}

// fakeGo puts a go on the PATH that prints script's lines as go test -json
// would, then sleeps, so a cell's limit stops it with a test running.
func fakeGo(t *testing.T, events string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\ncat <<'EOF'\n" + events + "EOF\nexec sleep 30\n"
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A go-json test still running when the cell's limit stopped it timed out: it
// failed, and says so. The one that finished keeps its result.
func TestGoJSONCellLimitResults(t *testing.T) {
	minute = 20 * time.Millisecond
	t.Cleanup(func() { minute = time.Minute })
	root, r := repo(t, "suites:\n  g: { cmd: go test ./..., format: go-json }\n", nil)
	now := `"Time":"` + time.Now().Format(time.RFC3339Nano) + `",`
	fakeGo(t, `{`+now+`"Action":"run","Package":"p","Test":"TestDone"}
{`+now+`"Action":"pass","Package":"p","Test":"TestDone","Elapsed":0.5}
{`+now+`"Action":"run","Package":"p","Test":"TestWait"}
{`+now+`"Action":"output","Package":"p","Test":"TestWait","Output":"=== RUN   TestWait\n"}
`)
	rep := runIn(t, root, r, "g")
	want := []string{"g p > TestDone: passed", "g p > TestWait: failed | stopped at the cell's limit"}
	if got := readSummary(t, filepath.Join(rep.Dir, "summary.json")).cases(); !slices.Equal(got, want) || rep.Outcomes[0].Result != Failed {
		t.Errorf("got %q, %+v\nwant %q", got, rep.Outcomes[0], want)
	}
}

// An interrupted cell didn't finish, and what its tests reported once it was
// interrupted can come from the interrupt, so it keeps no results, not even
// those of the tests that passed before.
func TestInterruptedCellHasNoResults(t *testing.T) {
	root, r := repo(t, `suites:
  s: { cmd: "sh {test}", tests: "t/*.sh" }
`, map[string]string{"t/a.sh": "exit 0\n", "t/b.sh": "touch \"$0.started\"; sleep 30\n"})
	cells, err := Select(root, r, "s", "", "pr", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			if _, err := os.Stat(filepath.Join(root, "t/b.sh.started")); err == nil {
				cancel()
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	rep, err := Run(ctx, cells, Options{Root: root, Results: filepath.Join(root, "results"), Out: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	s := readSummary(t, filepath.Join(rep.Dir, "summary.json"))
	if got := s.cases(); len(got) != 0 || s.Results.Extra.Cells[0].Result != NotRun {
		t.Errorf("tests %q, cells %+v", got, s.Results.Extra.Cells)
	}
}

// A format whose tool writes its results is read by its reader, given each
// test's output, first non-zero exit status, whether it timed out, and {out}.
// The results decide, not the exit status: a test that exits 1 with passing
// results passes, a failed result fails the cell, and a cell with no results
// didn't run. A test that timed out failed, whatever its results say.
func TestReaderResultsDecide(t *testing.T) {
	var got []report.Ran
	readers["tap"] = func(r report.Ran) ([]report.Test, error) {
		got = append(got, r)
		var tests []report.Test
		for _, line := range strings.Split(string(r.Output), "\n") {
			if name, ok := strings.CutPrefix(line, "not ok "); ok {
				tests = append(tests, report.Test{Name: name, Status: report.Failed})
			} else if name, ok := strings.CutPrefix(line, "ok "); ok {
				tests = append(tests, report.Test{Name: name, Status: report.Passed})
			}
		}
		return tests, nil
	}
	t.Cleanup(func() { delete(readers, "tap") })
	root, r := repo(t, `groups: { g: [pass, fail, none, slow] }
suites:
  pass: { cmd: "sh {test}", tests: "p/*.sh", format: tap }
  fail: { cmd: "sh {test}", tests: "f/*.sh", format: tap }
  none: { cmd: "sh {test}", tests: "n/*.sh", format: tap }
  slow: { cmd: "sh {test}", tests: "s/*.sh", format: tap, timeout: { test: 1s } }
`, map[string]string{"p/a.sh": "echo ok one; exit 1\n", "f/a.sh": "echo not ok two\n", "n/a.sh": "true\n", "s/a.sh": "echo ok three; sleep 30\n"})
	rep := runIn(t, root, r, "g")
	results := []string{Passed, Failed, NotRun, Failed}
	notes := []string{"", "1 failed: two", "no results", "s/a.sh: timed out after 1s"}
	for i, o := range rep.Outcomes {
		if o.Result != results[i] || o.Note != notes[i] {
			t.Errorf("%s: %+v, want %s with %q", o.Cell, o, results[i], notes[i])
		}
	}
	if len(got) != 4 || got[0].Test != "p/a.sh" || got[0].Exit != 1 || string(got[0].Output) != "ok one\n" || got[0].Out != filepath.Join(rep.Dir, "pass") || got[0].TimedOut || !got[3].TimedOut {
		t.Errorf("the reader got %+v", got)
	}
	want := []string{"pass > one: passed", "fail > two: failed", "slow > three: passed"}
	if cases := readSummary(t, filepath.Join(rep.Dir, "summary.json")).cases(); !slices.Equal(cases, want) {
		t.Errorf("got %q, want %q", cases, want)
	}
}

// A job's re-run attempt runs under a directory of its own, RUN-a2, so it
// never overwrites the first attempt's results; a first attempt, or a run
// outside a job, has no suffix.
func TestRunNameAttempt(t *testing.T) {
	start := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	for attempt, want := range map[string]string{"": "20261015T120000Z-abcdef12", "1": "20261015T120000Z-abcdef12", "2": "20261015T120000Z-abcdef12-a2"} {
		t.Setenv("GITHUB_RUN_ATTEMPT", attempt)
		if got := runName(start, "abcdef12"); got != want {
			t.Errorf("attempt %q: %s, want %s", attempt, got, want)
		}
	}
}

// One git status gives the checkout's commit, shortened, or "nocommit"
// before the first commit and outside a checkout; its branch, "" when it's on
// none; and whether it's dirty: it has changes its commit doesn't, an
// untracked file among them.
func TestGitState(t *testing.T) {
	// A git hook running the tests exports GIT_DIR and the like, which would
	// point these commands at the repository being committed to.
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "GIT_") {
			t.Setenv(k, "")
			_ = os.Unsetenv(k)
		}
	}
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		pre := []string{"-C", root, "-c", "user.name=dt", "-c", "user.email=dt@example.com", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + os.DevNull}
		out, err := exec.Command("git", append(pre, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	check := func(when, commit, branch string, dirty bool) {
		t.Helper()
		if c, b, d := gitState(root); c != commit || b != branch || d != dirty {
			t.Errorf("%s: %q, %q, dirty %v; want %q, %q, %v", when, c, b, d, commit, branch, dirty)
		}
	}
	git("init", "-q", "-b", "work")
	write("a.txt", "a\n")
	check("no commit yet", "nocommit", "work", true)
	git("add", ".")
	git("commit", "-q", "-m", "a")
	head := git("rev-parse", "--short=8", "HEAD")
	check("clean", head, "work", false)
	write("new.txt", "")
	check("an untracked file", head, "work", true)
	_ = os.Remove(filepath.Join(root, "new.txt"))
	write("a.txt", "b\n")
	check("a change", head, "work", true)
	git("checkout", "-q", "--", "a.txt")
	git("checkout", "-q", "--detach")
	check("detached", head, "", false)
	if c, b, d := gitState(t.TempDir()); c != "nocommit" || b != "" || d {
		t.Errorf("not a checkout: %q, %q, %v", c, b, d)
	}
}

// lines lists results as "package > name: status, duration", with the grade
// in brackets and the message's first line after a |.
func lines(tests []report.Test) []string {
	var out []string
	for _, tc := range tests {
		line := fmt.Sprintf("%s: %s, %s", strings.Join(append(slices.Clone(tc.Suite), tc.Name), " > "), tc.Status, tc.Duration.Round(time.Millisecond))
		if tc.Grade != "" {
			line += " [" + tc.Grade + "]"
		}
		if first, _, _ := strings.Cut(tc.Message, "\n"); first != "" {
			line += " | " + first
		}
		out = append(out, line)
	}
	return out
}

// Recorded go test -json runs, testdata/go-test.json, give a result for each
// test and subtest under its package, timed by go test, with the skip
// ungraded, and a failed result for each package that didn't build or failed
// outside any test. A test left open when its package ended is timed to that
// end: failed when go test's own timeout lists it as running, which takes in
// a parent blocked on its subtest; other otherwise, as after a crash or for a
// parent waiting on its parallel subtests; and, when it waited in t.Parallel
// and never continued, it didn't run and has no result. Such a package has
// no result of its own. Every test the recording starts, its package's end
// closes. A message keeps its lines' indentation, a stack's frames included.
// The cell's note counts the tests closed at their package's end, and they
// ran, as a test paused in t.Parallel didn't.
func TestGoJSONRecordedRun(t *testing.T) {
	events, err := os.ReadFile("testdata/go-test.json")
	if err != nil {
		t.Fatal(err)
	}
	var res results
	for _, line := range bytes.SplitAfter(events, []byte("\n")) {
		if len(bytes.TrimSpace(line)) > 0 {
			res.add(line, io.Discard)
		}
	}
	ungraded(&registry.Suite{}, res.cases)
	want := []string{
		"example.com/m/a > TestPass: passed, 20ms",
		"example.com/m/a > TestFail: failed, 0s |     a_test.go:9: broke",
		"example.com/m/a > TestSkip: skipped, 0s [ungraded] |     a_test.go:10: nothing to check here",
		"example.com/m/a > TestParent/good: passed, 0s",
		"example.com/m/a > TestParent/bad: failed, 0s |     a_test.go:13: sub broke",
		"example.com/m/a > TestParent: failed, 0s",
		"example.com/m/b: failed, 0s | build failed",
		"example.com/m/p > TestPanicky: other, 3ms | panic: boom",
		"example.com/m/tm > TestFine: passed, 0s",
		"example.com/m/tm: failed, 223ms | failed outside any test",
		"example.com/m/to > TestWait: failed, 3.004s | panic: test timed out after 3s",
		"example.com/m/pt > TestP: other, 3.004s",
		"example.com/m/pt > TestP/a: failed, 3.004s | panic: test timed out after 3s",
		"example.com/m/st > TestS: failed, 3.009s",
		"example.com/m/st > TestS/x: failed, 3.009s | panic: test timed out after 3s",
		"example.com/m/pc > TestQ: other, 6ms",
		"example.com/m/pc > TestQ/a: other, 6ms | panic: boom",
		"example.com/m/sc > TestR: other, 4ms",
		"example.com/m/sc > TestR/x: other, 4ms | panic: boom",
	}
	if got := lines(res.cases); !slices.Equal(got, want) || len(res.running) != 0 {
		t.Errorf("got %q\nwant %q\nstill open: %v", got, want, slices.Collect(maps.Keys(res.running)))
	}
	if got, want := res.note(nil), "3 passed, 7 failed, 1 skipped, 6 with no result: TestFail, TestParent/bad, TestP/a, TestS/x, TestWait; example.com/m/b: build failed; example.com/m/p: failed outside any test; example.com/m/tm: failed outside any test; example.com/m/pc: failed outside any test; example.com/m/sc: failed outside any test"; got != want {
		t.Errorf("note %q\nwant %q", got, want)
	}
	if !res.ran("TestS") || !res.ran("TestPanicky") || res.ran("TestP/b") {
		t.Errorf("ran: TestS %v, TestPanicky %v, TestP/b %v", res.ran("TestS"), res.ran("TestPanicky"), res.ran("TestP/b"))
	}
	i := slices.IndexFunc(res.cases, func(tc report.Test) bool { return tc.Name == "TestPanicky" })
	if i < 0 || !strings.Contains(res.cases[i].Message, "\n\t/src/m/p/p_test.go:9 +0x") {
		t.Errorf("the crash's stack lost its indentation: %q", res.cases)
	}
}

// A test with several results in a cell, as one that two commands ran, keeps
// each of them.
func TestGoJSONEveryResult(t *testing.T) {
	var res results
	for _, line := range []string{
		`{"Action":"run","Package":"p","Test":"TestFlip"}`,
		`{"Action":"fail","Package":"p","Test":"TestFlip"}`,
		`{"Action":"run","Package":"p","Test":"TestFlip"}`,
		`{"Action":"pass","Package":"p","Test":"TestFlip"}`,
	} {
		res.add([]byte(line+"\n"), io.Discard)
	}
	var got []string
	for _, tc := range res.cases {
		got = append(got, tc.Name+": "+tc.Status)
	}
	if want := []string{"TestFlip: failed", "TestFlip: passed"}; !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A package that fails while its only open tests wait in t.Parallel fails
// outside any test: those tests never ran, so they have no result to stand
// for the failure.
func TestGoJSONOnlyPausedTestsOpen(t *testing.T) {
	var res results
	for _, line := range []string{
		`{"Time":"2026-10-15T12:00:00Z","Action":"run","Package":"x","Test":"TestA"}`,
		`{"Time":"2026-10-15T12:00:00Z","Action":"pause","Package":"x","Test":"TestA"}`,
		`{"Time":"2026-10-15T12:00:01Z","Action":"output","Package":"x","Output":"panic: boom\n"}`,
		`{"Time":"2026-10-15T12:00:01Z","Action":"fail","Package":"x","Elapsed":1}`,
	} {
		res.add([]byte(line+"\n"), io.Discard)
	}
	if got, want := lines(res.cases), []string{"x: failed, 1s | failed outside any test"}; !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// cancelOn is an output that cancels the run once it's given text.
type cancelOn struct {
	text   string
	cancel context.CancelFunc
}

func (c cancelOn) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(c.text)) {
		c.cancel()
	}
	return len(p), nil
}

// A cell that finished keeps its results when the run is interrupted right
// after it, here as dt prints how it went; the next cell didn't run.
func TestFinishedCellKeepsItsResults(t *testing.T) {
	root, r := repo(t, `groups: { g: [done, next] }
suites:
  done: { cmd: "true" }
  next: { cmd: "true" }
`, nil)
	cells, err := Select(root, r, "g", "", "pr", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rep, err := Run(ctx, cells, Options{Root: root, Results: filepath.Join(root, "results"), Out: cancelOn{"[dt] done: passed", cancel}})
	if err != nil {
		t.Fatal(err)
	}
	s := readSummary(t, filepath.Join(rep.Dir, "summary.json"))
	if got, want := s.cases(), []string{"done > true: passed"}; !slices.Equal(got, want) || s.Results.Extra.Cells[1].Result != NotRun {
		t.Errorf("got %q, cells %+v; want %q", got, s.Results.Extra.Cells, want)
	}
}

// An interrupted go-json cell keeps no results, not even those of the tests
// that passed before the interrupt.
func TestGoJSONInterruptedCellHasNoResults(t *testing.T) {
	root, r := repo(t, "suites:\n  g: { cmd: go test ./..., format: go-json }\n", nil)
	now := `"Time":"` + time.Now().Format(time.RFC3339Nano) + `",`
	fakeGo(t, `{`+now+`"Action":"run","Package":"p","Test":"TestDone"}
{`+now+`"Action":"pass","Package":"p","Test":"TestDone","Elapsed":0.5}
{`+now+`"Action":"run","Package":"p","Test":"TestWait"}
{`+now+`"Action":"output","Package":"p","Test":"TestWait","Output":"=== RUN   TestWait\n"}
`)
	cells, err := Select(root, r, "g", "", "pr", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rep, err := Run(ctx, cells, Options{Root: root, Results: filepath.Join(root, "results"), Out: cancelOn{"=== RUN   TestWait", cancel}})
	if err != nil {
		t.Fatal(err)
	}
	s := readSummary(t, filepath.Join(rep.Dir, "summary.json"))
	if got := s.cases(); len(got) != 0 || rep.Outcomes[0].Result != NotRun {
		t.Errorf("tests %q, outcome %+v", got, rep.Outcomes[0])
	}
}

// A package that ends after the cell's limit ends because dt stopped it, as
// the limit's interrupt does go test run as root, which then reports the
// package failed: the test it left open timed out, so it failed, timed to the
// package's end.
func TestGoJSONPackageEndAfterTheLimit(t *testing.T) {
	res := results{limit: time.Date(2026, 10, 15, 12, 0, 5, 0, time.UTC)}
	for _, line := range []string{
		`{"Time":"2026-10-15T12:00:00Z","Action":"run","Package":"p","Test":"TestDone"}`,
		`{"Time":"2026-10-15T12:00:00Z","Action":"pass","Package":"p","Test":"TestDone"}`,
		`{"Time":"2026-10-15T12:00:01Z","Action":"run","Package":"p","Test":"TestWait"}`,
		`{"Time":"2026-10-15T12:00:05.1Z","Action":"output","Package":"p","Test":"TestWait","Output":"signal: interrupt\n"}`,
		`{"Time":"2026-10-15T12:00:05.2Z","Action":"fail","Package":"p","Elapsed":5.2}`,
	} {
		res.add([]byte(line+"\n"), io.Discard)
	}
	want := []string{"p > TestDone: passed, 0s", "p > TestWait: failed, 4.2s | stopped at the cell's limit"}
	if got := lines(res.cases); !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A reader never reads a test the interrupt stopped. An interrupt between two
// tests, which leaves the second unstarted, stops the cell too, so it keeps
// no results, not even the first test's.
func TestReaderInterrupted(t *testing.T) {
	var cancel context.CancelFunc
	var read []string
	readers["tap"] = func(r report.Ran) ([]report.Test, error) {
		read = append(read, r.Test)
		if r.Test == "b/a.sh" {
			cancel() // between the two tests
		}
		return []report.Test{{Name: r.Test, Status: report.Passed}}, nil
	}
	t.Cleanup(func() { delete(readers, "tap") })
	root, r := repo(t, `groups: { g: [during, between] }
suites:
  during: { cmd: "sh {test}", tests: "d/*.sh", format: tap }
  between: { cmd: "sh {test}", tests: "b/*.sh", format: tap }
`, map[string]string{"d/a.sh": "echo started; sleep 30\n", "b/a.sh": "true\n", "b/b.sh": "true\n"})
	for _, c := range []struct {
		target, note string
		read         []string
	}{
		{"during", "d/a.sh: interrupted; no results", nil},
		{"between", "1 tests not started: interrupted", []string{"b/a.sh"}},
	} {
		read = nil
		var ctx context.Context
		ctx, cancel = context.WithCancel(context.Background())
		cells, err := Select(root, r, c.target, "", "pr", nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		rep, err := Run(ctx, cells, Options{Root: root, Results: filepath.Join(root, "results"), Out: cancelOn{"started", cancel}})
		if err != nil {
			t.Fatal(err)
		}
		s := readSummary(t, filepath.Join(rep.Dir, "summary.json"))
		if o := rep.Outcomes[0]; o.Result != NotRun || o.Note != c.note || !slices.Equal(read, c.read) || len(s.cases()) != 0 {
			t.Errorf("%s: %+v, read %q, results %q", c.target, o, read, s.cases())
		}
		cancel()
	}
}

// A summary.json that can't be written doesn't stop the run: dt says so,
// runs the cells left, and returns the error once they're done.
func TestSummaryWriteFailureKeepsRunning(t *testing.T) {
	root, r := repo(t, `groups: { g: [breaks, next] }
suites:
  breaks: { cmd: { sh: "rm {out}/../summary.json && mkdir -p {out}/../summary.json/x" } }
  next: { cmd: { sh: "touch {out}/ran" } }
`, nil)
	cells, err := Select(root, r, "g", "", "pr", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	rep, err := Run(context.Background(), cells, Options{Root: root, Results: filepath.Join(root, "results"), Out: &out})
	if err == nil || len(rep.Outcomes) != 2 || rep.Outcomes[1].Result != Passed || !strings.Contains(out.String(), "[dt] summary.json: ") {
		t.Errorf("error %v, outcomes %+v\n%s", err, rep.Outcomes, out.String())
	}
	if _, err := os.Stat(filepath.Join(rep.Dir, "next", "ran")); err != nil {
		t.Error("the cell after the failed write didn't run")
	}
}

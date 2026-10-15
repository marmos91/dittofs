package run

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
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

var update = flag.Bool("update", false, "rewrite the golden files from what the tests get")

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
  lint: { cmd: [ "true", "false", "no-such-check {base}", { sh: "exit 75" }, { sh: "echo two\n# more" } ] }
  listed: { cmd: "no-such-check {base}", known: known.yaml }
`, map[string]string{"known.yaml": "[]\n"})
	rep := runIn(t, root, r, "g")
	want := []string{
		"lint > true: passed",
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
// package that failed outside any test failed, and a test whose binary
// crashed before its result has none: it's other, with its output.
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
		"g > example.com/m/p: failed | failed outside any test",
		"g example.com/m/a > TestFail: failed | a_test.go:6: broke",
		"g example.com/m/a > TestParent/bad: failed | a_test.go:10: sub broke",
		"g example.com/m/a > TestParent/good: passed",
		"g example.com/m/a > TestParent: failed",
		"g example.com/m/a > TestPass: passed",
		"g example.com/m/a > TestSkip: skipped [ungraded] | a_test.go:7: nothing to check here",
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
	fakeGo(t, `{"Action":"run","Package":"p","Test":"TestDone"}
{"Action":"pass","Package":"p","Test":"TestDone","Elapsed":0.5}
{"Action":"run","Package":"p","Test":"TestWait"}
{"Action":"output","Package":"p","Test":"TestWait","Output":"=== RUN   TestWait\n"}
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
// didn't run.
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
	results := []string{Passed, Failed, NotRun, Passed}
	notes := []string{"", "1 failed: two", "no results", ""}
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

// The checkout is dirty when it has changes its commit doesn't, an untracked
// file among them; its branch is "" when it's on none.
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
	git := func(args ...string) {
		t.Helper()
		pre := []string{"-C", root, "-c", "user.name=dt", "-c", "user.email=dt@example.com", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + os.DevNull}
		if out, err := exec.Command("git", append(pre, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "work")
	write("a.txt", "a\n")
	git("add", ".")
	git("commit", "-q", "-m", "a")
	check := func(when, branch string, dirty bool) {
		t.Helper()
		if b, d := gitState(root); b != branch || d != dirty {
			t.Errorf("%s: branch %q, dirty %v; want %q, %v", when, b, d, branch, dirty)
		}
	}
	check("clean", "work", false)
	write("new.txt", "")
	check("an untracked file", "work", true)
	_ = os.Remove(filepath.Join(root, "new.txt"))
	write("a.txt", "b\n")
	check("a change", "work", true)
	git("checkout", "-q", "--", "a.txt")
	git("checkout", "-q", "--detach")
	check("detached", "", false)
	if b, d := gitState(t.TempDir()); b != "" || d {
		t.Errorf("not a checkout: %q, %v", b, d)
	}
}

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

// go test -json's events, recorded in testdata/go-test.json, become CTRF as a
// go-json cell turns them: each result under its package, the skip ungraded,
// the packages that didn't build or failed outside any test, the test go
// test's own timeout stopped, which failed, and the test whose binary crashed
// with no result of its own, which is other.
func TestGoJSONToCTRF(t *testing.T) {
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
	finished := len(res.cases)
	res.finish(false)
	for i := finished; i < len(res.cases); i++ {
		res.cases[i].Duration = 0 // how long dt saw it run, which the recording doesn't keep
	}
	ungraded(&registry.Suite{}, res.cases)
	r := report.Run{
		Name: "20261015T120000Z-abcdef12", Start: time.UnixMilli(1791460800000), Stop: time.UnixMilli(1791460803000),
		Commit: "abcdef12", Branch: "develop", OS: "linux", In: "host", Tier: "pr", Exit: 1,
		Cells: []report.Cell{{Name: "unit", Result: Failed, Note: res.note(nil), Took: 3 * time.Second, Log: "logs/unit.log", Tests: res.cases}},
	}
	data, err := r.JSON()
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "go-test.summary.json", data)
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

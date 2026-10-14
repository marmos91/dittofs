package run

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/harness/registry"
)

const head = "version: 1\ndefaults: { format: exit, tier: pr, timeout: { test: 30m, cell: 60m } }\n"

// repo writes files into a new repository root and loads the registry y.
func repo(t *testing.T, y string, files map[string]string) (string, *registry.Registry) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("these suites run sh, which the go-json tests don't need")
	}
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r, err := registry.Parse(root, []byte(head+y))
	if err != nil {
		t.Fatal(err)
	}
	return root, r
}

// runTarget selects and runs target at tier pr, and returns the report and
// everything the run printed.
func runTarget(t *testing.T, root string, r *registry.Registry, target string, ci bool) (Report, string) {
	t.Helper()
	cells, err := Select(root, r, target, "", "pr", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	rep, err := Run(context.Background(), cells, Options{Root: root, Results: filepath.Join(root, "results"), CI: ci, Out: &out})
	if err != nil {
		t.Fatal(err)
	}
	return rep, out.String()
}

// Every command of a list runs, in order, even after one fails, and the cell
// fails if any did (section 4.1, cmd).
func TestEveryCommandRuns(t *testing.T) {
	root, r := repo(t, `suites:
  lint: { cmd: [ "false", "true", { sh: "echo ran > {out}/marker" } ] }
`, nil)
	rep, _ := runTarget(t, root, r, "lint", false)
	if o := rep.Outcomes[0]; o.Result != Failed || !strings.Contains(o.Note, "`false` exited 1") {
		t.Errorf("outcome %+v", o)
	}
	if _, err := os.Stat(filepath.Join(rep.Dir, "lint", "marker")); err != nil {
		t.Error("the command after the failing one didn't run")
	}
	if rep.Status() != 1 {
		t.Errorf("status %d, want 1", rep.Status())
	}
}

// A multi-line script is named in the note by its first line, so the note
// stays on one line.
func TestMultiLineScriptNote(t *testing.T) {
	root, r := repo(t, `suites:
  s:
    cmd:
      sh: |
        false
        # more
`, nil)
	rep, _ := runTarget(t, root, r, "s", false)
	if note := rep.Outcomes[0].Note; note != "`false ...` exited 1" {
		t.Errorf("note %q", note)
	}
}

// Each test runs under its own limit; one that fails or times out fails the
// cell, and the ones after it still run. A timed-out test's children die with
// it, those that made a process group of their own included.
func TestTestsRunUnderTheirLimits(t *testing.T) {
	root, r := repo(t, `suites:
  s: { cmd: "sh {test}", tests: "t/*.sh", timeout: { test: 1s } }
`, map[string]string{
		"t/a.sh": "exit 0\n",
		"t/b.sh": "exit 3\n",
		"t/c.sh": "sleep 30 & echo $! > \"$0.pid\"; wait\n",
		"t/d.sh": "echo ran > \"$0.ran\"\n",
		"t/e.sh": "set -m; sleep 30 & echo $! > \"$0.pid\"; wait\n",
	})
	rep, _ := runTarget(t, root, r, "s", false)
	note := rep.Outcomes[0].Note
	for _, want := range []string{"t/b.sh: `sh t/b.sh` exited 3", "t/c.sh: timed out after 1s", "t/e.sh: timed out after 1s"} {
		if !strings.Contains(note, want) {
			t.Errorf("note %q lacks %q", note, want)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "t/d.sh.ran")); err != nil {
		t.Error("the test after the failures didn't run")
	}
	for _, test := range []string{"t/c.sh", "t/e.sh"} {
		if alive(t, filepath.Join(root, test+".pid")) {
			t.Errorf("%s: the timed-out test's child outlived it", test)
		}
	}
}

// A command that exits but leaves a process holding its output still passes:
// the process is killed, and dt doesn't wait on it.
func TestLeftoversAreKilled(t *testing.T) {
	root, r := repo(t, `suites:
  s: { cmd: "sh {test}", tests: "t/*.sh" }
`, map[string]string{"t/a.sh": "sleep 30 & echo $! > \"$0.pid\"\nexit 0\n"})
	start := time.Now()
	rep, _ := runTarget(t, root, r, "s", false)
	took := time.Since(start)
	if o := rep.Outcomes[0]; o.Result != Passed {
		t.Errorf("outcome %+v", o)
	}
	if alive(t, filepath.Join(root, "t/a.sh.pid")) {
		t.Error("the process the test left behind outlived it")
	}
	if took > 3*time.Second {
		t.Errorf("took %s: dt waited on what the test left behind", took)
	}
}

// alive reports whether the process whose pid is in file still runs. It only
// looks: a pid can be reused, so a test never signals one.
func alive(t *testing.T, file string) bool {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(string(data))
	if _, err := strconv.Atoi(pid); err != nil {
		t.Fatalf("pid %q in %s", pid, file)
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", pid).Output()
	return err == nil && !strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

// When the cell's limit passes, the running test is stopped and the rest
// aren't started: the cell fails, and says how many tests it left.
func TestCellLimitStopsTheCell(t *testing.T) {
	minute = 200 * time.Millisecond
	t.Cleanup(func() { minute = time.Minute })
	root, r := repo(t, `suites:
  s: { cmd: "sh {test}", tests: "t/*.sh", timeout: { test: 10s, cell: 1m } }
`, map[string]string{"t/a.sh": "sleep 30\n", "t/b.sh": "exit 0\n", "t/c.sh": "exit 0\n"})
	rep, _ := runTarget(t, root, r, "s", false)
	o := rep.Outcomes[0]
	if o.Result != Failed || !strings.Contains(o.Note, "t/a.sh: stopped at the cell's limit") || !strings.Contains(o.Note, "2 tests not started") {
		t.Errorf("outcome %+v", o)
	}
}

// Without --ci a cell whose needs aren't met is skipped, and a run where
// nothing ran exits 2; with --ci the cell didn't run, which is exit 2 and an
// annotation (sections 4.4 and 4.5).
func TestUnmetNeeds(t *testing.T) {
	other := "windows" // repo skips the test there
	root, r := repo(t, `groups: { g: [away, here] }
suites:
  away: { cmd: "true", needs: [`+other+`] }
  here: { cmd: "true" }
`, nil)
	if rep, _ := runTarget(t, root, r, "g", false); rep.Outcomes[0].Result != Skipped || rep.Status() != 0 {
		t.Errorf("local: %+v, status %d", rep.Outcomes, rep.Status())
	}
	if rep, _ := runTarget(t, root, r, "away", false); rep.Status() != 2 {
		t.Errorf("nothing ran: status %d, want 2", rep.Status())
	}
	rep, out := runTarget(t, root, r, "g", true)
	if rep.Outcomes[0].Result != NotRun || rep.Status() != 2 || !strings.Contains(out, "::error title=dt away::") {
		t.Errorf("ci: %+v, status %d\n%s", rep.Outcomes, rep.Status(), out)
	}
}

// A suite dt can't run yet is reported as not run, never as passed.
func TestUnsupportedIsNotRun(t *testing.T) {
	root, r := repo(t, `suites:
  g: { cmd: go test ./..., format: go-json }
`, nil)
	rep, _ := runTarget(t, root, r, "g", false)
	if o := rep.Outcomes[0]; o.Result != NotRun || !strings.Contains(o.Note, "go-json") {
		t.Errorf("outcome %+v", o)
	}
}

// args and env follow the tier the suite runs at.
func TestTierSetsArgsAndEnv(t *testing.T) {
	root, r := repo(t, `suites:
  s:
    cmd: { sh: 'echo "$A-$B" {args} > {out}/got' }
    args: { pr: "-short" }
    env: { A: "1", nightly: { B: "2" } }
`, nil)
	for tier, want := range map[string]string{"pr": "1- -short", "nightly": "1-2"} {
		cells, err := Select(root, r, "s", "", tier, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		rep, err := Run(context.Background(), cells, Options{Root: root, Results: filepath.Join(root, "results"), Out: &bytes.Buffer{}})
		if err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(filepath.Join(rep.Dir, "s", "got"))
		if strings.TrimSpace(string(got)) != want {
			t.Errorf("tier %s: got %q, want %q", tier, got, want)
		}
	}
}

// The log has the cell, the commit, each command and its exit status and
// duration (section 4.6).
func TestLog(t *testing.T) {
	root, r := repo(t, `suites:
  s: { cmd: "false" }
`, nil)
	rep, _ := runTarget(t, root, r, "s", false)
	log, err := os.ReadFile(filepath.Join(rep.Dir, "logs", "s.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[dt] cell s, tier pr, commit ", "[dt] $ false", "[dt] exit 1 after "} {
		if !strings.Contains(string(log), want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
}

// Runs started in the same second get directories of their own.
func TestRunDirsAreUnique(t *testing.T) {
	results := t.TempDir()
	a, err := newRunDir(results, "20261013T120000Z-abcdef12")
	if err != nil {
		t.Fatal(err)
	}
	b, err := newRunDir(results, "20261013T120000Z-abcdef12")
	if err != nil {
		t.Fatal(err)
	}
	if b != a+"-2" {
		t.Errorf("second run's directory %s", b)
	}
}

// Placeholders: a line splits at spaces and never through a shell, a whole
// {args} becomes one argument per item, an sh command gets its values quoted,
// and a shell's ${VAR} is left alone (section 4.1).
func TestFill(t *testing.T) {
	vals := map[string]string{"out": "/r/o", "test": "it's.sh"}
	args := []string{"-short", "-v"}
	got, err := fill(registry.Command{Line: "go test {args} -o={out}/x {test}"}, vals, args)
	if err != nil || !slices.Equal(got, []string{"go", "test", "-short", "-v", "-o=/r/o/x", "it's.sh"}) {
		t.Errorf("line: %q, %v", got, err)
	}
	got, err = fill(registry.Command{Sh: "echo ${HOME} {test} {args}"}, vals, args)
	if want := `echo ${HOME} 'it'\''s.sh' '-short' '-v'`; err != nil || got[2] != want {
		t.Errorf("sh: %q, %v", got, err)
	}
	if _, err := fill(registry.Command{Line: "run {packages}"}, vals, nil); err == nil || !strings.Contains(err.Error(), "{packages}") {
		t.Errorf("a placeholder dt doesn't provide: %v", err)
	}
	if _, err := fill(registry.Command{Line: "{args}"}, vals, nil); err == nil {
		t.Error("a command that fills to nothing has no error")
	}
}

// Select: a group narrowed by --only and --skip, options by flag or by name,
// each unit at the wider of the two tiers, a name with values keeping every
// value, and tests chosen by name.
func TestSelect(t *testing.T) {
	root, r := repo(t, `groups: { g: [a, b, c] }
suites:
  a: { cmd: "run {p}", matrix: { p: [x, y] }, select: { pr: { p: [y] } }, options: { o: { cmd: "run o {p}" } } }
  b: { cmd: "true", tier: merge }
  c: { cmd: "sh {test}", tests: "t/*.sh" }
`, map[string]string{"t/one.sh": "", "t/two.sh": ""})
	names := func(target, option string, only, skip, tests []string) []string {
		t.Helper()
		cells, err := Select(root, r, target, option, "pr", only, skip, tests)
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		var out []string
		for _, c := range cells {
			out = append(out, c.Name+"@"+c.Tier+"="+strings.Join(c.Tests, ","))
		}
		return out
	}
	cases := []struct {
		target, option   string
		only, skip, test []string
		want             []string
	}{
		{"g", "", []string{"a", "b"}, []string{"b"}, nil, []string{"a/y@pr="}},
		{"a", "o", nil, nil, nil, []string{"a/o/y@pr="}},
		{"a/o", "", nil, nil, nil, []string{"a/o/y@pr="}},
		{"a/x", "", nil, nil, nil, []string{"a/x@pr="}},
		{"b", "", nil, nil, nil, []string{"b@merge="}},
		{"c", "", nil, nil, []string{"two.sh"}, []string{"c@pr=t/two.sh"}},
	}
	for _, c := range cases {
		if got := names(c.target, c.option, c.only, c.skip, c.test); !slices.Equal(got, c.want) {
			t.Errorf("%s --%s only %v skip %v tests %v: got %v, want %v", c.target, c.option, c.only, c.skip, c.test, got, c.want)
		}
	}
	for _, bad := range []struct {
		target, option string
		only, tests    []string
	}{
		{"nope", "", nil, nil},
		{"g", "o", nil, nil},
		{"a", "nope", nil, nil},
		{"g", "", []string{"z"}, nil},
		{"a", "", nil, []string{"one.sh"}},
		{"c", "", nil, []string{"three.sh"}},
		{"c", "", nil, []string{"two.sh", "typo.sh"}},
		{"a/z", "", nil, nil},
	} {
		if _, err := Select(root, r, bad.target, bad.option, "pr", bad.only, nil, bad.tests); err == nil {
			t.Errorf("%+v: no error", bad)
		}
	}
}

// {base} is a pull request's base, the commit before a push, or none for an
// event with no change to check; locally it's origin/develop (section 4.1).
func TestBase(t *testing.T) {
	event := filepath.Join(t.TempDir(), "event.json")
	cases := []struct{ actions, name, baseRef, before, want string }{
		{"", "", "", "", "origin/develop"},
		{"true", "pull_request", "main", "", "origin/main"},
		{"true", "push", "", "abc123", "abc123"},
		{"true", "push", "", "0000000000000000000000000000000000000000", ""},
		{"true", "merge_group", "", "", "def456"},
		{"true", "schedule", "", "", ""},
		{"true", "workflow_dispatch", "", "", ""},
	}
	for _, c := range cases {
		t.Setenv("GITHUB_ACTIONS", c.actions)
		t.Setenv("GITHUB_EVENT_NAME", c.name)
		t.Setenv("GITHUB_BASE_REF", c.baseRef)
		t.Setenv("GITHUB_EVENT_PATH", event)
		body := `{"before":"` + c.before + `"}`
		if c.name == "merge_group" {
			body = `{"merge_group":{"base_sha":"def456","head_sha":"aaa"}}`
		}
		if err := os.WriteFile(event, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := base(); got != c.want {
			t.Errorf("%+v: base %q", c, got)
		}
	}
}

// Without a {base}, the commands that use it are skipped and say so; the cell
// neither fails nor goes unrun (section 4.8).
func TestNoBaseSkipsItsCommands(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("GITHUB_EVENT_NAME", "schedule")
	root, r := repo(t, `suites:
  lint: { cmd: [ "true", "no-such-check {base}" ] }
`, nil)
	rep, out := runTarget(t, root, r, "lint", false)
	if rep.Outcomes[0].Result != Passed || !strings.Contains(out, "[dt] skipped, no change to check: no-such-check {base}") {
		t.Errorf("outcome %+v\n%s", rep.Outcomes[0], out)
	}
}

// The job summary is a table of every cell's outcome; a | in a note can't
// break it.
func TestMarkdown(t *testing.T) {
	rep := Report{Outcomes: []Outcome{
		{Cell: "lint-go", Result: Failed, Note: "`a | b` exited 1", Took: 2 * time.Second},
		{Cell: "shellcheck", Result: Passed, Took: time.Second},
	}}
	want := "### dt run lint\n\n| Cell | Result | Time | Note |\n|---|---|---|---|\n" +
		"| `lint-go` | failed | 2s | `a \\| b` exited 1 |\n| `shellcheck` | passed | 1s |  |\n"
	if got := rep.Markdown("dt run lint"); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

// A command that exits 75 ran nothing, as the scenarios' setup does when
// another run holds their lock: the test didn't run, and the cell didn't run
// unless another test failed (section 4.6).
func TestExit75IsNotRun(t *testing.T) {
	root, r := repo(t, `suites:
  held: { cmd: "sh {test}", tests: "h/*.sh" }
  mixed: { cmd: "sh {test}", tests: "m/*.sh" }
`, map[string]string{"h/a.sh": "exit 75\n", "h/b.sh": "exit 0\n", "m/a.sh": "exit 75\n", "m/b.sh": "exit 1\n"})
	rep, _ := runTarget(t, root, r, "held", false)
	if o := rep.Outcomes[0]; o.Result != NotRun || !strings.Contains(o.Note, "h/a.sh: `sh h/a.sh` exited 75: it ran nothing") || rep.Status() != 2 {
		t.Errorf("held: %+v, status %d", o, rep.Status())
	}
	if rep, _ := runTarget(t, root, r, "mixed", false); rep.Outcomes[0].Result != Failed || rep.Status() != 1 {
		t.Errorf("mixed: %+v, status %d", rep.Outcomes[0], rep.Status())
	}
}

// An interrupt leaves the running test, and every test and cell after it, not
// run: an interrupted run is no regression (section 4.4).
func TestInterruptIsNotRun(t *testing.T) {
	root, r := repo(t, `groups: { g: [slow, next] }
suites:
  slow: { cmd: "sh {test}", tests: "t/*.sh" }
  next: { cmd: "true" }
`, map[string]string{"t/a.sh": "sleep 30\n", "t/b.sh": "exit 0\n"})
	cells, err := Select(root, r, "g", "", "pr", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	rep, err := Run(ctx, cells, Options{Root: root, Results: filepath.Join(root, "results"), Out: &bytes.Buffer{}})
	if err != nil {
		t.Fatal(err)
	}
	slow, next := rep.Outcomes[0], rep.Outcomes[1]
	if slow.Result != NotRun || !strings.Contains(slow.Note, "t/a.sh: interrupted") || !strings.Contains(slow.Note, "1 tests not started: interrupted") {
		t.Errorf("slow: %+v", slow)
	}
	if next.Result != NotRun || rep.Status() != 2 {
		t.Errorf("next: %+v, status %d", next, rep.Status())
	}
}

// A command with a placeholder dt doesn't fill can't run, wherever it sits,
// so the cell runs none of its commands.
func TestUnfillableCommandIsNotRun(t *testing.T) {
	root, r := repo(t, `suites:
  s: { cmd: [ "touch ran", "run {bin}" ] }
`, nil)
	rep, _ := runTarget(t, root, r, "s", false)
	if o := rep.Outcomes[0]; o.Result != NotRun || !strings.Contains(o.Note, "{bin}") || rep.Status() != 2 {
		t.Errorf("outcome %+v, status %d", o, rep.Status())
	}
	if _, err := os.Stat(filepath.Join(root, "ran")); err == nil {
		t.Error("the cell ran a command before finding one it can't fill")
	}
}

// A suite whose tests match nothing gives no cells, which is nothing to run:
// exit 2, not a usage error.
func TestNothingToRun(t *testing.T) {
	root, r := repo(t, `groups: { g: [s] }
suites:
  s: { cmd: "true" }
`, nil)
	cells, err := Select(root, r, "g", "", "pr", nil, []string{"s"}, nil)
	if err != nil || len(cells) != 0 {
		t.Fatalf("cells %v, %v", cells, err)
	}
	var out bytes.Buffer
	rep, err := Run(context.Background(), cells, Options{Root: root, Results: filepath.Join(root, "results"), Out: &out})
	if err != nil || rep.Status() != 2 || !strings.Contains(out.String(), "[dt] nothing to run") {
		t.Errorf("status %d, %v\n%s", rep.Status(), err, out.String())
	}
}

// {out} is absolute, so a command finds it from whatever directory it runs
// in, a relative results directory included; a dir that fills to an absolute
// path, as {out} does, is used as it is.
func TestPathsHoldFromAnyDir(t *testing.T) {
	root, r := repo(t, `suites:
  rel: { cmd: { sh: "echo x > {out}/got" }, dir: sub }
  abs: { cmd: { sh: "pwd > here" }, dir: "{out}" }
`, map[string]string{"sub/.keep": ""})
	t.Chdir(root)
	for _, suite := range []string{"rel", "abs"} {
		cells, err := Select(root, r, suite, "", "pr", nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		rep, err := Run(context.Background(), cells, Options{Root: root, Results: "results", Out: &bytes.Buffer{}})
		if err != nil || rep.Outcomes[0].Result != Passed {
			t.Errorf("%s: %+v, %v", suite, rep.Outcomes, err)
		}
		if !filepath.IsAbs(rep.Dir) {
			t.Errorf("%s: run directory %q", suite, rep.Dir)
		}
	}
}

// An annotation's text can't end it early or break it: %, CR and LF are
// escaped, and in its title : and , too.
func TestAnnotation(t *testing.T) {
	got := annotation("dt a:b,c", "100% done\nthen\rmore")
	if want := "::error title=dt a%3Ab%2Cc::100%25 done%0Athen%0Dmore"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

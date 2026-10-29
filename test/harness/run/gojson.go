package run

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/marmos91/dittofs/test/harness/needs"
	"github.com/marmos91/dittofs/test/harness/report"
)

// sudoGrace is how long a command of a suite that runs as root, go test
// among them, has to stop after SIGINT before dt kills it.
const sudoGrace = 2 * time.Minute

// goJSON runs a go-json cell's commands and grades the cell from their
// events and the suite's known file: a new failure, an unexpected pass or a
// failed command fails it, as does the cell's limit; a cell where no test ran,
// that was interrupted, or with an unexpected skip didn't run. names, when
// given, are the tests to run; one that didn't run is named, and the cell
// didn't run unless it failed. It also returns each result of each test and
// subtest, and each package that failed outside any test, graded. A test
// whose output stopped before its result failed, whatever the known file
// says: the cell's limit or go test's own timeout stopped it, or its test
// binary crashed. One that waited in t.Parallel and never continued didn't
// run, and has none. A test left open when its package ended is timed to that
// end. Up to retries times, the top-level tests with a new failure run
// again, by -run.
func (t tester) goJSON(names []string, retries int) (string, string, []report.Test) {
	t.env = append(t.env, needs.ServiceEnv(t.root, t.cell.Needs)...)
	root := asRoot(t.cell)
	if root {
		if err := buildBin(t.ctx, t.root, t.vals["bin"], t.env, t.out); err != nil {
			return report.NotRun, fmt.Sprintf("dfs and dfsctl didn't build: %v", err), nil
		}
	}
	limit, _ := t.ctx.Deadline()
	res := results{limit: limit}
	problems, stop := t.goTests(names, root, &res, "")
	// decision: a package that failed outside any test isn't retried: -run
	// can't name it, and what fails a build fails it again.
	for n := 1; n <= retries && stop == "" && t.ctx.Err() == nil; n++ {
		graded := slices.Clone(res.cases)
		t.cell.grade(graded)
		var again []string
		for _, tc := range graded {
			if top, _, _ := strings.Cut(tc.Name, "/"); tc.Grade == report.New && len(tc.Suite) > 0 && !slices.Contains(again, top) {
				again = append(again, top)
			}
		}
		if len(again) == 0 {
			break
		}
		_, _ = fmt.Fprintf(t.out, "[dt] retry %d: %s\n", n, strings.Join(again, ", "))
		retry := results{limit: limit}
		if _, stop = t.goTests(again, root, &retry, fmt.Sprintf("-retry-%d", n)); stop == "" {
			retried(res.cases, graded, retry.cases, n)
		}
	}
	if stop != "" {
		// The cell didn't finish, and what its tests reported once
		// interrupted can come from the interrupt itself.
		return report.NotRun, res.note(append(problems, stop)), nil
	}
	// decision: a go-json suite with a known file is graded as a whole, so
	// every skip in every package it runs must be listed, and an unlisted one
	// leaves the cell not run. Grade package by package once a suite needs
	// only some of its packages listed.
	t.cell.grade(res.cases)
	unexpected, notRun := gradeNotes(res.cases)
	problems = append(problems, unexpected...)
	if missing := slices.DeleteFunc(slices.Clone(names), res.ran); len(missing) > 0 {
		notRun = append(notRun, strings.Join(missing, ", ")+" didn't run")
	}
	switch {
	case slices.ContainsFunc(res.cases, func(tc report.Test) bool { return tc.Grade == report.New }) || len(problems) > 0:
		return report.Failed, res.note(append(problems, notRun...)), res.cases
	case len(notRun) > 0:
		return report.NotRun, res.note(notRun), res.cases
	case len(res.tests) == 0:
		return report.NotRun, "no tests ran", res.cases
	}
	return report.Passed, res.note(nil), res.cases
}

// goTests runs each of the cell's commands into res, with -run naming the
// tests when names are given, each command's events in a file named after
// it and suffix. It returns what went wrong outside the tests' own results,
// and why the cell can't go on, an interrupt or a command it can't fill, or
// "". The cell's limit stops the commands, and fails the tests it stopped.
func (t tester) goTests(names []string, root bool, res *results, suffix string) ([]string, string) {
	var problems []string
	for i, cmd := range t.cell.Unit.Cmd {
		argv, err := fill(cmd, t.vals, t.lists)
		if err != nil {
			return problems, err.Error()
		}
		if len(names) > 0 {
			argv = append(argv, "-run", runPattern(names))
		}
		shown := strings.Join(argv, " ")
		failed := res.failures()
		code, err := t.goTest(argv, root, filepath.Join(t.vals["out"], fmt.Sprintf("go-test-%d%s.json", i+1, suffix)), res)
		if errors.Is(t.ctx.Err(), context.DeadlineExceeded) {
			res.end("", time.Now(), atCellLimit)
			return append(problems, atCellLimit), ""
		}
		switch {
		case t.ctx.Err() != nil:
			return problems, "interrupted"
		case err != nil:
			problems = append(problems, fmt.Sprintf("`%s`: %v", shown, err))
		case code != 0 && res.failures() == failed:
			problems = append(problems, fmt.Sprintf("`%s` exited %d", shown, code))
		}
		res.end("", time.Now(), "")
	}
	return problems, ""
}

// goTest runs a go test command with -json -count=1 added after its go test,
// so nothing replays from the test cache. For a suite that needs root, go
// test builds as the user and runs each test binary through -exec under
// sudo, with its own flags, in its package's directory; cancelling it then
// sends SIGINT, so the tests' own cleanup runs.
func (t tester) goTest(argv []string, asRoot bool, events string, res *results) (int, error) {
	added, shown := []string{"-json", "-count=1"}, []string{"-json", "-count=1"}
	if asRoot {
		run, hidden, err := t.sudo()
		if err != nil {
			return -1, err
		}
		added, shown = append(added, "-exec", run), append(shown, "-exec", hidden)
	}
	_, _ = fmt.Fprintf(t.out, "[dt] $ %s\n", strings.Join(slices.Concat(argv[:2], shown, argv[2:]), " "))
	argv = slices.Concat(argv[:2], added, argv[2:])
	cmd := exec.CommandContext(t.ctx, argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = t.dir, t.env
	if asRoot {
		interruptTree(cmd, sudoGrace)
	} else {
		killTree(cmd)
	}
	return followed(cmd, events, res, t.out)
}

// buildBin builds dfs and dfsctl from the checkout at root, as the user, into
// {bin}, the run's bin directory, once per run: for a cell with a fixture, and
// for a suite whose tests run as root, which take the binaries from
// DITTOFS_E2E_BIN, so root never runs go build. A cell without {bin}, as a
// knfsd fixture's, builds nothing.
func buildBin(ctx context.Context, root, bin string, env []string, w io.Writer) error {
	if bin == "" {
		return nil
	}
	names := []string{"dfs", "dfsctl"}
	if !slices.ContainsFunc(names, func(n string) bool { _, err := os.Stat(filepath.Join(bin, n)); return err != nil }) {
		return nil // an earlier cell of this run built them
	}
	build := []string{"go", "build", "-o", bin + string(filepath.Separator), "./cmd/dfs", "./cmd/dfsctl"}
	_, _ = fmt.Fprintf(w, "[dt] $ %s\n", strings.Join(build, " "))
	code, err := execute(ctx, build, root, env, w, false)
	if err == nil && code != 0 {
		err = fmt.Errorf("go build exited %d", code)
	}
	return err
}

// sudo is the -exec program that runs a test binary as root, and the same
// program to print, with each variable but DITTOFS_E2E_BIN as NAME=$NAME, so
// no value, a credential among them, reaches the terminal or the log. sudo
// resets the environment, so it passes the suite's variables, its services',
// DITTOFS_E2E_BIN and PATH explicitly, as go test words them.
func (t tester) sudo() (run, shown string, err error) {
	// decision: the caller's own environment stays behind. A variable set by
	// hand to narrow the tests, such as DITTOFS_E2E_SKIP_KERBEROS, doesn't
	// reach them, which fails closed: the tests it would skip run. What a
	// suite needs goes in its env, which is passed. Forward the caller's
	// variables if narrowing a root suite by hand turns out to be needed.
	words := slices.Concat(
		[]string{"sudo", "-n", "env", "PATH=" + os.Getenv("PATH"), "DITTOFS_E2E_BIN=" + t.vals["bin"]},
		t.own, needs.ServiceEnv(t.root, t.cell.Needs))
	runs, shows := make([]string, len(words)), make([]string, len(words))
	for i, w := range words {
		if runs[i], err = execQuote(w); err != nil {
			return "", "", err
		}
		shows[i] = runs[i]
		if name, _, isVar := strings.Cut(w, "="); isVar && name != "DITTOFS_E2E_BIN" {
			shows[i] = name + "=$" + name
		}
	}
	return strings.Join(runs, " "), strings.Join(shows, " "), nil
}

// execQuote quotes a word of go test's -exec, which splits at spaces and
// takes a field in single or double quotes whole, with no escapes inside.
func execQuote(w string) (string, error) {
	switch {
	case w != "" && !strings.ContainsAny(w, " \t\n\r'\""):
		return w, nil
	case !strings.Contains(w, "'"):
		return "'" + w + "'", nil
	case !strings.Contains(w, `"`):
		return `"` + w + `"`, nil
	}
	return "", fmt.Errorf("%q holds both quote characters, which go test's -exec can't pass", w)
}

// followed runs cmd with its output going to the file events, and passes the
// events through res as they're written. A file, unlike a pipe, can't be held
// open by a process the tests leave behind: dt reads to its end and stops.
// What the command leaves running in its process group is killed when it
// exits.
func followed(cmd *exec.Cmd, events string, res *results, w io.Writer) (int, error) {
	f, err := os.Create(events)
	if err != nil {
		return -1, err
	}
	defer func() { _ = f.Close() }()
	cmd.Stdout, cmd.Stderr = f, f
	done := make(chan struct{})
	read := make(chan error, 1)
	go func() { read <- follow(events, done, func(line []byte) { res.add(line, w) }) }()
	err = cmd.Run()
	killLeftovers(cmd)
	code, err := exitCode(err)
	close(done)
	if rerr := <-read; err == nil {
		err = rerr
	}
	return code, err
}

// follow calls line for each line of the file as it grows, until done closes
// and it has read what's left.
func follow(name string, done <-chan struct{}, line func([]byte)) error {
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReader(f)
	var partial []byte
	final := false
	for {
		chunk, err := r.ReadBytes('\n')
		partial = append(partial, chunk...)
		switch {
		case err == nil:
			line(partial)
			partial = nil
		case !errors.Is(err, io.EOF):
			return err
		case final:
			if len(partial) > 0 {
				line(partial)
			}
			return nil
		default:
			select {
			case <-done:
				final = true
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
}

// event is one test2json event.
type event struct {
	Action, Package, Test, Output, FailedBuild string
	Time                                       time.Time
	Elapsed                                    float64 // seconds
}

// atCellLimit says why a test the cell's limit stopped failed.
const atCellLimit = "stopped at the cell's limit"

// results are a go-json cell's tests, from their events.
type results struct {
	limit    time.Time           // the cell's limit; a package that ends after it, dt stopped
	tests    map[string]string   // "package test" to fail once a command failed it, else its last pass or skip
	failedIn map[string]bool     // packages with a failed test
	broken   []string            // packages that failed outside any test
	cases    []report.Test       // each result of each test, and each package that failed outside any test
	running  map[string]*started // "package test" for the tests started and not finished
}

// started is a test that started, and its output so far.
type started struct {
	at     time.Time
	output []string
	paused bool // waiting in t.Parallel, not yet continued
}

// statuses are the test2json actions that end a test, as results.
var statuses = map[string]string{"pass": report.Passed, "fail": report.Failed, "skip": report.Skipped}

// add records one line of go test -json output, and passes the tests' own
// output through as go test -v prints it. A line that isn't an event, such as
// an error from the go command, passes through as it is. A package that
// fails with no failed test of its own, such as one that didn't build or
// exited in TestMain, is recorded as broken. When a package ends, the tests
// it left open never got a result: they're recorded then, and stand for the
// package's failure rather than a failure outside any test.
func (r *results) add(line []byte, w io.Writer) {
	var e event
	if json.Unmarshal(line, &e) != nil || e.Action == "" {
		_, _ = w.Write(line)
		return
	}
	if r.tests == nil {
		r.tests, r.failedIn, r.running = map[string]string{}, map[string]bool{}, map[string]*started{}
	}
	k := e.Package + " " + e.Test
	switch {
	case e.Action == "output" || e.Action == "build-output":
		_, _ = io.WriteString(w, e.Output)
		if s := r.running[k]; s != nil {
			s.output = append(s.output, e.Output)
		}
	case e.Test != "" && e.Action == "run":
		r.running[k] = &started{at: e.Time}
	case e.Test != "" && (e.Action == "pause" || e.Action == "cont"):
		if s := r.running[k]; s != nil {
			s.paused = e.Action == "pause"
		}
	case e.Test != "" && statuses[e.Action] != "":
		if r.tests[k] != "fail" {
			r.tests[k] = e.Action
		}
		if e.Action == "fail" {
			r.failedIn[e.Package] = true
		}
		tc := report.Test{Name: e.Test, Suite: []string{e.Package}, Status: statuses[e.Action], Duration: seconds(e.Elapsed)}
		if s := r.running[k]; s != nil && e.Action != "pass" {
			tc.Message = message(s.output)
		}
		delete(r.running, k)
		r.cases = append(r.cases, tc)
	case e.Test == "" && (e.Action == "pass" || e.Action == "fail"):
		// A package that ends after the cell's limit ends because dt stopped
		// it, as the limit's interrupt does a suite run as root: the tests it
		// left open timed out.
		stopped := ""
		if !r.limit.IsZero() && !e.Time.Before(r.limit) {
			stopped = atCellLimit
		}
		recorded := r.end(e.Package, e.Time, stopped)
		if e.Action == "fail" && !r.failedIn[e.Package] {
			why := "failed outside any test"
			if e.FailedBuild != "" {
				why = "build failed"
			}
			r.broken = append(r.broken, e.Package+": "+why)
			if !recorded {
				// decision: a package's failure outside its tests, as a
				// build that failed, is new whatever the known file says:
				// an entry names tests, and none can stand for a package.
				r.cases = append(r.cases, report.Test{Name: e.Package, Status: report.Failed, Grade: report.New, Duration: seconds(e.Elapsed), Message: why})
			}
		}
	}
}

// end records the tests still open in pkg, or in every package when pkg is
// "", as ended when given, and forgets them. One still waiting in t.Parallel
// never ran, so it has no result. The rest failed, and are new whatever the
// known file says: a crash or a hang is a regression until shown otherwise.
// stopped says why when dt stopped them, at the cell's limit; otherwise their
// test binary crashed or go test's own timeout stopped it, which leaves a
// parent waiting on its subtests open too, and each keeps its output as its
// message. end reports whether it recorded any.
func (r *results) end(pkg string, when time.Time, stopped string) bool {
	recorded := false
	for _, k := range slices.Sorted(maps.Keys(r.running)) {
		p, test, _ := strings.Cut(k, " ")
		if pkg != "" && p != pkg {
			continue
		}
		s := r.running[k]
		delete(r.running, k)
		if s.paused {
			continue
		}
		r.cases = append(r.cases, report.Test{Name: test, Suite: []string{p}, Status: report.Failed, Grade: report.New,
			Duration: when.Sub(s.at), Message: cmp.Or(stopped, message(s.output))})
		recorded = true
		r.failedIn[p] = true
		r.tests[k] = "fail"
	}
	return recorded
}

// framing matches the lines go test frames a test's own output with.
var framing = regexp.MustCompile(`^(=== (RUN|PAUSE|CONT|NAME)|--- (PASS|FAIL|SKIP):) `)

// message is a test's own output without go test's framing or blank lines.
// Each line keeps its indentation, as a stack's frames do, and loses the
// space after it.
func message(output []string) string {
	var kept []string
	for _, line := range strings.Split(strings.Join(output, ""), "\n") {
		if text := strings.TrimSpace(line); text != "" && !framing.MatchString(text) {
			kept = append(kept, strings.TrimRightFunc(line, unicode.IsSpace))
		}
	}
	return strings.Join(kept, "\n")
}

// seconds is test2json's elapsed seconds as a duration.
func seconds(s float64) time.Duration {
	return time.Duration(s * float64(time.Second))
}

// ran reports whether a test with exactly that name, subtest path included,
// ran in any package.
func (r *results) ran(name string) bool {
	for k := range r.tests {
		if _, test, _ := strings.Cut(k, " "); test == name {
			return true
		}
	}
	return false
}

// failures counts the failed tests and the broken packages.
func (r *results) failures() int {
	n := len(r.broken)
	for _, a := range r.tests {
		if a == "fail" {
			n++
		}
	}
	return n
}

// note gives the counts of the tests that passed, failed and skipped, a
// failed test whose every failure is known counted apart, and one a retry
// passed counted as flaky; then the new
// failures, without a parent whose subtest failed; then the broken packages
// and the problems.
//
// decision: a test whose failures are all known or quarantined is counted as
// known, not as failed, so "N failed" counts only what fails the cell. A test
// with several results, one of them new, is still counted as failed.
func (r *results) note(problems []string) string {
	grades := map[string]map[string]bool{} // "package test" to its results' grades
	for _, tc := range r.cases {
		if len(tc.Suite) > 0 {
			k := tc.Suite[0] + " " + tc.Name
			if grades[k] == nil {
				grades[k] = map[string]bool{}
			}
			grades[k][tc.Grade] = true
			grades[k]["flaky"] = grades[k]["flaky"] || tc.Flaky
		}
	}
	count := map[string]int{}
	var failed []string
	for k, a := range r.tests {
		switch {
		case a != "fail" || grades[k][report.New]:
		case grades[k][report.Known], grades[k][report.Quarantined]:
			a = "known"
		case grades[k]["flaky"]:
			a = "flaky"
		}
		count[a]++
		if a == "fail" {
			failed = append(failed, k)
		}
	}
	slices.Sort(failed)
	var leaves []string
	for _, k := range failed {
		if !slices.ContainsFunc(failed, func(o string) bool { return strings.HasPrefix(o, k+"/") }) {
			_, name, _ := strings.Cut(k, " ")
			leaves = append(leaves, name)
		}
	}
	note := fmt.Sprintf("%d passed, %d failed, %d skipped", count["pass"], count["fail"], count["skip"])
	for _, a := range []string{"known", "flaky"} {
		if count[a] > 0 {
			note += fmt.Sprintf(", %d %s", count[a], a)
		}
	}
	if len(leaves) > 0 {
		note += ": " + some(leaves)
	}
	for _, p := range slices.Concat(r.broken, problems) {
		note += "; " + p
	}
	return note
}

// runPattern is a -run pattern for the named tests, TestParent/name for a
// subtest: each element is anchored, and the names are joined with |.
func runPattern(names []string) string {
	patterns := make([]string, len(names))
	for i, n := range names {
		elems := strings.Split(n, "/")
		for j, e := range elems {
			elems[j] = "^" + regexp.QuoteMeta(e) + "$"
		}
		patterns[i] = strings.Join(elems, "/")
	}
	return strings.Join(patterns, "|")
}

// goPackages lists the packages whose tests the build tag changes: those
// whose test files differ with the tag. Finding none is an error, never an
// empty run.
func goPackages(root, tag string) ([]string, error) {
	plain, err := testFiles(root)
	if err != nil {
		return nil, err
	}
	tagged, err := testFiles(root, "-tags="+tag)
	if err != nil {
		return nil, err
	}
	var pkgs []string
	for pkg, files := range tagged {
		if plain[pkg] != files {
			pkgs = append(pkgs, pkg)
		}
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("no package's tests change with the %s tag", tag)
	}
	slices.Sort(pkgs)
	return pkgs, nil
}

// testFiles maps each package of the module at root to its test files.
func testFiles(root string, flags ...string) (map[string]string, error) {
	cmd := exec.Command("go", slices.Concat([]string{"list"}, flags, []string{"-f", "{{.ImportPath}}\t{{.TestGoFiles}} {{.XTestGoFiles}}", "./..."})...)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exit.Stderr)))
		}
		return nil, fmt.Errorf("go list %s: %w", strings.Join(flags, " "), err)
	}
	files := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		pkg, list, _ := strings.Cut(line, "\t")
		files[pkg] = list
	}
	return files, nil
}

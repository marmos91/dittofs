package run

import (
	"bufio"
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

// sudoGrace is how long go test has to stop after SIGINT, on a suite that
// runs as root, before dt kills it.
const sudoGrace = 2 * time.Minute

// goJSON runs a go-json cell's commands and grades the cell from their
// events: a failed test, package or command fails it, as does the cell's
// limit, a cell where no test ran or that was interrupted didn't run, and
// skips are reported, not graded. names, when given, are the tests to run;
// one that didn't run is named, and the cell didn't run unless it failed.
// It also returns each result of each test and subtest, and each package that
// failed outside any test. A test the cell's limit or go test's own timeout
// stopped failed; one whose output stopped before its result otherwise, as
// when its test binary crashed, is other, with that output; one that waited
// in t.Parallel and never continued didn't run, and has none. A test left
// open when its package ended is timed to that end.
func (t tester) goJSON(names []string) (string, string, []report.Test) {
	t.env = append(t.env, needs.ServiceEnv(t.root, t.cell.Needs)...)
	root := asRoot(t.cell)
	if root {
		if err := t.buildBin(); err != nil {
			return NotRun, fmt.Sprintf("dfs and dfsctl didn't build: %v", err), nil
		}
	}
	limit, _ := t.ctx.Deadline()
	res := results{limit: limit}
	var problems []string
	for i, cmd := range t.cell.Unit.Cmd {
		argv, err := fill(cmd, t.vals, t.lists)
		if err != nil {
			return NotRun, err.Error(), nil
		}
		if len(names) > 0 {
			// go test passes everything after -args to the test binaries as
			// it is, so -run goes before it.
			at := slices.IndexFunc(argv, func(a string) bool { return a == "-args" || a == "--args" })
			if at < 0 {
				at = len(argv)
			}
			argv = slices.Insert(argv, at, "-run", runPattern(names))
		}
		shown := strings.Join(argv, " ")
		failed := res.failures()
		code, err := t.goTest(argv, root, filepath.Join(t.vals["out"], fmt.Sprintf("go-test-%d.json", i+1)), &res)
		switch {
		case errors.Is(t.ctx.Err(), context.DeadlineExceeded):
			res.end("", time.Now(), atCellLimit)
			return Failed, res.note(append(problems, atCellLimit)), res.cases
		case t.ctx.Err() != nil:
			// The cell didn't finish, and what its tests reported once
			// interrupted can come from the interrupt itself.
			return NotRun, res.note(append(problems, "interrupted")), nil
		case err != nil:
			problems = append(problems, fmt.Sprintf("`%s`: %v", shown, err))
		case code != 0 && res.failures() == failed:
			problems = append(problems, fmt.Sprintf("`%s` exited %d", shown, code))
		}
		res.end("", time.Now(), "")
	}
	var notRun []string
	if missing := slices.DeleteFunc(slices.Clone(names), res.ran); len(missing) > 0 {
		notRun = append(notRun, strings.Join(missing, ", ")+" didn't run")
	}
	switch {
	case res.failures() > 0 || len(problems) > 0:
		return Failed, res.note(append(problems, notRun...)), res.cases
	case len(notRun) > 0:
		return NotRun, res.note(notRun), res.cases
	case len(res.tests) == 0:
		return NotRun, "no tests ran", res.cases
	}
	return Passed, res.note(nil), res.cases
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

// asRoot reports whether a cell's go tests run as root through sudo: a
// go-json suite that needs root, run by dt as a user.
func asRoot(c Cell) bool {
	return c.Unit.Format == "go-json" && slices.Contains(c.Needs, "root") && os.Geteuid() != 0
}

// buildBin builds dfs and dfsctl as the user into {bin}, the run's bin
// directory, once per run, for a suite whose tests run as root: they take the
// binaries from DITTOFS_E2E_BIN, so root never runs go build.
func (t tester) buildBin() error {
	bin := t.vals["bin"]
	names := []string{"dfs", "dfsctl"}
	if !slices.ContainsFunc(names, func(n string) bool { _, err := os.Stat(filepath.Join(bin, n)); return err != nil }) {
		return nil // an earlier cell of this run built them
	}
	build := []string{"go", "build", "-o", bin + string(filepath.Separator), "./cmd/dfs", "./cmd/dfsctl"}
	_, _ = fmt.Fprintf(t.out, "[dt] $ %s\n", strings.Join(build, " "))
	code, err := execute(t.ctx, build, t.root, t.env, t.out)
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
	if cmd.Process != nil {
		killLeftovers(cmd)
	}
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
	timedOut map[string]bool     // packages go test's own timeout stopped
	listed   map[string]bool     // "package test" for the tests that timeout lists as running
}

// started is a test that started, and its output so far.
type started struct {
	at     time.Time
	output []string
	paused bool // waiting in t.Parallel, not yet continued
}

// listedRunning matches a test go test's own timeout lists as running.
var listedRunning = regexp.MustCompile(`^\t\t(\S+) \(\S+\)\n?$`)

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
		r.timedOut, r.listed = map[string]bool{}, map[string]bool{}
	}
	k := e.Package + " " + e.Test
	switch {
	case e.Action == "output" || e.Action == "build-output":
		_, _ = io.WriteString(w, e.Output)
		if s := r.running[k]; s != nil {
			s.output = append(s.output, e.Output)
		}
		if strings.HasPrefix(e.Output, "panic: test timed out after ") {
			r.timedOut[e.Package] = true
		}
		if r.timedOut[e.Package] {
			if m := listedRunning.FindStringSubmatch(e.Output); m != nil {
				r.listed[e.Package+" "+m[1]] = true
			}
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
				r.cases = append(r.cases, report.Test{Name: e.Package, Status: report.Failed, Duration: seconds(e.Elapsed), Message: why})
			}
		}
	}
}

// end records the tests still open in pkg, or in every package when pkg is
// "", as ended when given, and forgets them. One still waiting in t.Parallel
// never ran, so it has no result. Of the rest, those stopped says why they
// were stopped, the cell's limit, failed; those go test's own timeout lists
// as running failed, a parent blocked on its subtest among them; any other's
// output stopped before its result, so it's other, as after a crash or for a
// parent waiting on its parallel subtests. The last two keep their output as
// their message. end reports whether it recorded any.
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
		tc := report.Test{Name: test, Suite: []string{p}, Status: report.Other, Duration: when.Sub(s.at), Message: message(s.output)}
		switch {
		case stopped != "":
			tc.Status, tc.Message = report.Failed, stopped
		case r.listed[k]:
			tc.Status = report.Failed
		}
		r.cases = append(r.cases, tc)
		recorded = true
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

// note gives the counts, the failed tests, without a parent whose subtest
// failed, the broken packages and the problems.
func (r *results) note(problems []string) string {
	count := map[string]int{}
	var failed []string
	for k, a := range r.tests {
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

package run

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/marmos91/dittofs/test/harness/needs"
)

// sudoGrace is how long go test has to stop after SIGINT, on a suite that
// runs as root, before dt kills it.
const sudoGrace = 2 * time.Minute

// goJSON runs a go-json cell's commands and grades the cell from their
// events: a failed test, package or command fails it, as does the cell's
// limit, a cell where no test ran or that was interrupted didn't run, and
// skips are reported, not graded. names, when given, are the tests to run;
// one that didn't run is named, and the cell didn't run unless it failed.
func (t tester) goJSON(names []string) (string, string) {
	t.env = append(t.env, needs.ServiceEnv(t.root, t.cell.Needs)...)
	root := asRoot(t.cell)
	if root {
		if err := t.buildBin(); err != nil {
			return NotRun, fmt.Sprintf("dfs and dfsctl didn't build: %v", err)
		}
	}
	var res results
	var problems []string
	for i, cmd := range t.cell.Unit.Cmd {
		argv, err := fill(cmd, t.vals, t.lists)
		if err != nil {
			return NotRun, err.Error()
		}
		if len(names) > 0 {
			argv = append(argv, "-run", runPattern(names))
		}
		shown := strings.Join(argv, " ")
		failed := res.failures()
		code, err := t.goTest(argv, root, filepath.Join(t.vals["out"], fmt.Sprintf("go-test-%d.json", i+1)), &res)
		switch {
		case errors.Is(t.ctx.Err(), context.DeadlineExceeded):
			return Failed, res.note(append(problems, "stopped at the cell's limit"))
		case t.ctx.Err() != nil:
			return NotRun, res.note(append(problems, "interrupted"))
		case err != nil:
			problems = append(problems, fmt.Sprintf("`%s`: %v", shown, err))
		case code != 0 && res.failures() == failed:
			problems = append(problems, fmt.Sprintf("`%s` exited %d", shown, code))
		}
	}
	var notRun []string
	if missing := slices.DeleteFunc(slices.Clone(names), res.ran); len(missing) > 0 {
		notRun = append(notRun, strings.Join(missing, ", ")+" didn't run")
	}
	switch {
	case res.failures() > 0 || len(problems) > 0:
		return Failed, res.note(append(problems, notRun...))
	case len(notRun) > 0:
		return NotRun, res.note(notRun)
	case len(res.tests) == 0:
		return NotRun, "no tests ran"
	}
	return Passed, res.note(nil)
}

// goTest runs a go test command with -json -count=1 added after its go test,
// so nothing replays from the test cache. For a suite that needs root, go
// test builds as the user and runs each test binary through -exec under
// sudo, with its own flags, in its package's directory; cancelling it then
// sends SIGINT, so the tests' own cleanup runs.
func (t tester) goTest(argv []string, asRoot bool, events string, res *results) (int, error) {
	added := []string{"-json", "-count=1"}
	if asRoot {
		run, err := t.sudo()
		if err != nil {
			return -1, err
		}
		added = append(added, "-exec", run)
	}
	argv = slices.Concat(argv[:2], added, argv[2:])
	_, _ = fmt.Fprintf(t.out, "[dt] $ %s\n", strings.Join(argv, " "))
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

// sudo is the -exec program that runs a test binary as root. sudo resets the
// environment, so it passes the suite's variables, its services',
// DITTOFS_E2E_BIN and PATH explicitly, as go test words them.
func (t tester) sudo() (string, error) {
	// decision: the caller's own environment stays behind. A variable set by
	// hand to narrow the tests, such as DITTOFS_E2E_SKIP_KERBEROS,
	// DITTOFS_E2E_LOCAL_ONLY or MINIO_ENDPOINT, doesn't reach them, which
	// fails closed: the tests it would skip run, and those it would point at
	// a service start their own. What a suite needs goes in its env, which is
	// passed. Forward a prefix of the caller's variables if narrowing a root
	// suite by hand turns out to be needed.
	words := slices.Concat(
		[]string{"sudo", "-n", "env", "PATH=" + os.Getenv("PATH"), "DITTOFS_E2E_BIN=" + t.vals["bin"]},
		t.own, needs.ServiceEnv(t.root, t.cell.Needs))
	quoted := make([]string, len(words))
	for i, w := range words {
		var err error
		if quoted[i], err = execQuote(w); err != nil {
			return "", err
		}
	}
	return strings.Join(quoted, " "), nil
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
}

// results are a go-json cell's tests, from their events.
type results struct {
	tests    map[string]string // "package test" to its last pass, fail or skip
	failedIn map[string]bool   // packages with a failed test
	broken   []string          // packages that failed outside any test
}

// add records one line of go test -json output, and passes the tests' own
// output through as go test -v prints it. A line that isn't an event, such as
// an error from the go command, passes through as it is. A package that
// fails with no failed test of its own, such as one that didn't build or
// exited in TestMain, is recorded as broken.
func (r *results) add(line []byte, w io.Writer) {
	var e event
	if json.Unmarshal(line, &e) != nil || e.Action == "" {
		_, _ = w.Write(line)
		return
	}
	switch {
	case e.Action == "output" || e.Action == "build-output":
		_, _ = io.WriteString(w, e.Output)
	case e.Test != "" && (e.Action == "pass" || e.Action == "fail" || e.Action == "skip"):
		if r.tests == nil {
			r.tests, r.failedIn = map[string]string{}, map[string]bool{}
		}
		r.tests[e.Package+" "+e.Test] = e.Action
		if e.Action == "fail" {
			r.failedIn[e.Package] = true
		}
	case e.Test == "" && e.Action == "fail" && !r.failedIn[e.Package]:
		why := "failed outside any test"
		if e.FailedBuild != "" {
			why = "build failed"
		}
		r.broken = append(r.broken, e.Package+": "+why)
	}
}

// ran reports whether a test of that name, a subtest's whole, ran in any
// package.
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
	if len(leaves) > 5 {
		leaves = append(leaves[:5], fmt.Sprintf("and %d more", len(leaves)-5))
	}
	if len(leaves) > 0 {
		note += ": " + strings.Join(leaves, ", ")
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

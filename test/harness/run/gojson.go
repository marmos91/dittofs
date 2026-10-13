package run

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/marmos91/dittofs/test/harness/needs"
)

// elevate runs a command as root. It's a variable so the tests can run the
// root path without sudo.
var elevate = []string{"sudo", "-n"}

// sudoGrace is how long a command run as root has to stop after SIGTERM,
// which sudo passes on, before dt kills sudo itself.
const sudoGrace = 2 * time.Minute

// goJSON runs a go-json cell's commands and grades the cell from their
// events (section 4.6): a failed test, package or command fails it, a cell
// where no test ran didn't run, and skips are reported, not graded. names,
// when given, are the top-level tests to run.
func (t tester) goJSON(names []string) (string, string) {
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
		events := filepath.Join(t.vals["out"], fmt.Sprintf("go-test-%d", i+1))
		failed := res.failures()
		var code int
		if slices.Contains(t.cell.Needs, "root") && os.Geteuid() != 0 {
			code, err = t.goTestAsRoot(argv, events, &res)
		} else {
			code, err = t.goTest(argv, events+".json", &res)
		}
		switch {
		case t.ctx.Err() != nil:
			return Failed, res.note(append(problems, stopped(t.ctx)))
		case err != nil:
			problems = append(problems, fmt.Sprintf("`%s`: %v", shown, err))
		case code != 0 && res.failures() == failed:
			problems = append(problems, fmt.Sprintf("`%s` exited %d", shown, code))
		}
	}
	switch {
	case res.failures() > 0 || len(problems) > 0:
		return Failed, res.note(problems)
	case len(res.tests) == 0:
		return NotRun, "no tests ran"
	}
	return Passed, res.note(nil)
}

// goTest runs a go test command with -json -count=1 added after its go test,
// so nothing replays from the test cache.
func (t tester) goTest(argv []string, events string, res *results) (int, error) {
	argv = slices.Concat(argv[:2], []string{"-json", "-count=1"}, argv[2:])
	_, _ = fmt.Fprintf(t.out, "[dt] $ %s\n", strings.Join(argv, " "))
	cmd := exec.CommandContext(t.ctx, argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = t.dir, t.env
	killTree(cmd)
	return followed(cmd, events, res, t.out)
}

// goTestAsRoot builds the command's packages as the user, with go test -c,
// and runs each test binary under sudo through test2json, in its package's
// directory as go test would (section 3). sudo resets the environment, so the
// suite's variables, its services' and PATH are passed explicitly.
func (t tester) goTestAsRoot(argv []string, events string, res *results) (int, error) {
	build, flags, pkgs := splitGoTest(argv)
	bin := filepath.Join(t.vals["out"], "bin") + string(filepath.Separator)
	compile := slices.Concat([]string{"go", "test", "-c", "-o", bin}, build, pkgs)
	_, _ = fmt.Fprintf(t.out, "[dt] $ %s\n", strings.Join(compile, " "))
	if code, err := execute(t.ctx, compile, t.dir, t.env, t.out); err != nil || code != 0 {
		res.broken = append(res.broken, "the test binaries didn't build")
		return code, err
	}
	dirs, err := goList(t.dir, t.env, build, pkgs)
	if err != nil {
		return -1, err
	}
	// go tool test2json, built as the user: -n prints the binary it runs.
	find := exec.Command("go", "tool", "-n", "test2json")
	find.Dir, find.Env = t.dir, t.env
	tool, err := find.Output()
	if err != nil {
		return -1, fmt.Errorf("go tool -n test2json: %w", err)
	}
	env := slices.Concat([]string{"env", "PATH=" + os.Getenv("PATH")}, t.own, needs.ServiceEnv(t.cell.Needs))
	status := 0
	for _, pkg := range slices.Sorted(maps.Keys(dirs)) {
		binary := bin + path.Base(pkg) + ".test"
		if _, err := os.Stat(binary); err != nil {
			continue // a package without tests has no binary
		}
		run := slices.Concat(elevate, env, []string{strings.TrimSpace(string(tool)), "-t", "-p", pkg, binary, "-test.v=test2json"}, flags)
		_, _ = fmt.Fprintf(t.out, "[dt] $ (in %s) %s\n", dirs[pkg], strings.Join(run, " "))
		cmd := exec.CommandContext(t.ctx, run[0], run[1:]...)
		cmd.Dir = dirs[pkg]
		stopTree(cmd, sudoGrace)
		code, err := followed(cmd, events+"-"+path.Base(pkg)+".json", res, t.out)
		if err != nil || t.ctx.Err() != nil {
			return code, err
		}
		status = max(status, code)
	}
	return status, nil
}

// followed runs cmd with its output going to the file events, and passes the
// events through res as they're written. A file, unlike a pipe, can't be held
// open by a process the tests leave behind: dt reads to its end and stops.
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
	code, err := exitCode(cmd.Run())
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
	packages map[string]string // a failed package to "failed" or "build failed"
	broken   []string          // what failed before any test could run
}

// add records one line of go test -json output, and passes the tests' own
// output through as go test -v prints it. A line that isn't an event, such as
// an error from the go command, passes through as it is.
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
			r.tests = map[string]string{}
		}
		r.tests[e.Package+" "+e.Test] = e.Action
	case e.Test == "" && e.Action == "fail":
		if r.packages == nil {
			r.packages = map[string]string{}
		}
		r.packages[e.Package] = "failed"
		if e.FailedBuild != "" {
			r.packages[e.Package] = "build failed"
		}
	}
}

// problems are what failed outside any test: the build of the binaries, and
// each package that failed with no failed test of its own, such as one that
// didn't build or timed out.
func (r *results) problems() []string {
	out := slices.Clone(r.broken)
	for _, pkg := range slices.Sorted(maps.Keys(r.packages)) {
		if !slices.ContainsFunc(slices.Collect(maps.Keys(r.tests)), func(k string) bool {
			return strings.HasPrefix(k, pkg+" ") && r.tests[k] == "fail"
		}) {
			out = append(out, pkg+": "+r.packages[pkg])
		}
	}
	return out
}

// failures counts the failed tests and the problems.
func (r *results) failures() int {
	n := len(r.problems())
	for _, a := range r.tests {
		if a == "fail" {
			n++
		}
	}
	return n
}

// note gives the counts, the failed tests, without a parent whose subtest
// failed, and the problems.
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
	for _, p := range slices.Concat(r.problems(), problems) {
		note += "; " + p
	}
	return note
}

// runPattern is a -run pattern for exactly the named top-level tests.
func runPattern(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = regexp.QuoteMeta(n)
	}
	return "^(" + strings.Join(quoted, "|") + ")$"
}

// Flags of go test: the ones its test binary takes, and the ones that take no
// value when given without one.
var (
	testFlags = setOf("run", "skip", "short", "timeout", "count", "parallel", "failfast", "shuffle", "v", "cpu", "bench", "benchtime", "benchmem", "list", "fullpath")
	boolFlags = setOf("short", "failfast", "v", "benchmem", "fullpath", "race", "cover", "msan", "asan", "trimpath", "a", "n", "x", "work", "json")
)

func setOf(items ...string) map[string]bool {
	m := map[string]bool{}
	for _, i := range items {
		m[i] = true
	}
	return m
}

// splitGoTest splits a go test command's arguments into its build flags, its
// test binary's flags, as -test.NAME, and its packages.
func splitGoTest(argv []string) (build, test, pkgs []string) {
	args := argv[2:]
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			pkgs = append(pkgs, a)
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !hasValue && !boolFlags[name] && i+1 < len(args) {
			i++
			value, hasValue = args[i], true
		}
		flag := "-" + name
		if testFlags[name] {
			flag = "-test." + name
		}
		if hasValue {
			flag += "=" + value
		}
		if testFlags[name] {
			test = append(test, flag)
		} else {
			build = append(build, flag)
		}
	}
	return build, test, pkgs
}

// goList maps each package to its directory.
func goList(dir string, env, build, pkgs []string) (map[string]string, error) {
	cmd := exec.Command("go", slices.Concat([]string{"list"}, build, []string{"-f", "{{.ImportPath}}\t{{.Dir}}"}, pkgs)...)
	cmd.Dir, cmd.Env = dir, env
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %w", err)
	}
	dirs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if pkg, d, ok := strings.Cut(line, "\t"); ok {
			dirs[pkg] = d
		}
	}
	return dirs, nil
}

// goPackages lists the packages whose tests the build tag changes: those
// whose test files differ with the tag (section 4.1). Finding none is an
// error, never an empty run.
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

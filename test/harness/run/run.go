// Package run runs the cells of a dt run: each test's commands under the
// test's and the cell's limits, with their output passed through and logged,
// and each test's results kept in the run's summary
// (docs/internals/rfcs/rfc-29-test-harness.md).
package run

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/marmos91/dittofs/test/harness/fixture"
	"github.com/marmos91/dittofs/test/harness/known"
	"github.com/marmos91/dittofs/test/harness/needs"
	"github.com/marmos91/dittofs/test/harness/plan"
	"github.com/marmos91/dittofs/test/harness/registry"
	"github.com/marmos91/dittofs/test/harness/report"
)

// Report is a whole run.
type Report struct {
	Dir      string // the run's results directory
	Outcomes []report.Cell
}

// Status is the run's exit status: 1 when a cell failed, else 2 when one
// didn't run or nothing ran, else 0.
func (r Report) Status() int {
	ran, notRun := false, false
	for _, o := range r.Outcomes {
		switch o.Result {
		case report.Failed:
			return 1
		case report.NotRun:
			notRun = true
		case report.Passed:
			ran = true
		}
	}
	if notRun || !ran {
		return 2
	}
	return 0
}

// Options say where a run happens and writes.
type Options struct {
	Root     string    // the repository
	Results  string    // where runs are written, each in a directory of its own
	CI       bool      // --ci: an unmet need is an error, not a skip, and new failures and unexpected passes are annotated
	Out      io.Writer // where the commands' output passes through
	Tier     string    // the tier the run was asked for, recorded in its results
	Retry    int       // --retry: how many times a new failure's test runs again
	Place    string    // --place: the place the run is on, which meets that place's host need, recorded in its results
	Parallel int       // --parallel: how many cells run at once; below 2, one after another
}

// minute is a variable so the tests can shorten a cell's limit.
var minute = time.Minute

// Run runs the cells, each logged under a new directory in o.Results, where
// summary.json holds every finished cell's results, in the cells' order: it's
// written when the run starts and rewritten after each cell, so a run stopped
// at any point still has them. Cells start in order, up to o.Parallel at once,
// each once none running conflicts with it; with more than one at once, each
// line a cell prints is led by its name. A summary.json that can't be written
// doesn't stop the run: dt says so, and Run returns the first such error once
// the cells are done. Cancelling ctx stops the cells running and the ones
// after them.
func Run(ctx context.Context, cells []Cell, o Options) (Report, error) {
	commit, branch, dirty := gitState(o.Root)
	// Absolute, so {out} means the same from any directory a command runs in.
	results, err := filepath.Abs(o.Results)
	if err != nil {
		return Report{}, err
	}
	start := time.Now()
	dir, err := newRunDir(results, runName(start, commit))
	if err != nil {
		return Report{}, err
	}
	rep := Report{Dir: dir}
	kernel, _ := exec.Command("uname", "-r").Output() // none where there is no uname
	sum := report.Run{Name: filepath.Base(dir), Start: start, Stop: start, Commit: commit, Branch: branch, Dirty: dirty, OS: runtime.GOOS,
		Kernel: strings.TrimSpace(string(kernel)), In: "host", Tier: o.Tier, Place: o.Place, Exit: rep.Status()}
	if os.Getenv("DT_IN_CONTAINER") == "1" {
		sum.In = "container"
	}
	var writeErr error
	write := func() {
		if err := sum.Write(dir); err != nil {
			_, _ = fmt.Fprintf(o.Out, "[dt] summary.json: %v\n", err)
			if writeErr == nil {
				writeErr = err
			}
		}
	}
	write()
	if len(cells) == 0 {
		_, _ = fmt.Fprintln(o.Out, "[dt] nothing to run")
	}
	// Cells that run at once would build the run's {bin} and lay out its
	// {tools} at once: do both before any starts, outside every cell's limits.
	if o.Parallel > 1 {
		prepare(ctx, cells, o, rep.Dir)
	}
	// mu guards what the cells share: running, done, rep, sum and o.Out.
	var mu sync.Mutex
	freed := sync.NewCond(&mu)
	running := map[int]bool{}
	done := make([]*report.Cell, len(cells))
	// waits reports whether c must wait: o.Parallel cells run, or one that
	// conflicts with c does.
	waits := func(c Cell) bool {
		if len(running) >= max(o.Parallel, 1) {
			return true
		}
		for j := range running {
			if conflict(cells[j], c) {
				return true
			}
		}
		return false
	}
	var wg sync.WaitGroup
	for i, c := range cells {
		mu.Lock()
		for waits(c) {
			freed.Wait()
		}
		running[i] = true
		mu.Unlock()
		wg.Go(func() {
			co := o
			var out *prefixed
			if o.Parallel > 1 {
				out = &prefixed{mu: &mu, w: o.Out, prefix: "[" + c.Name + "] "}
				co.Out = out
			}
			start := time.Now()
			cell := runCell(ctx, c, co, rep.Dir, commit)
			cell.Took = time.Since(start).Round(time.Second)
			if out != nil {
				out.flush()
			}
			mu.Lock()
			defer mu.Unlock()
			delete(running, i)
			done[i] = &cell
			rep.Outcomes = rep.Outcomes[:0]
			for _, d := range done {
				if d != nil {
					rep.Outcomes = append(rep.Outcomes, *d)
				}
			}
			_, _ = fmt.Fprintf(o.Out, "[dt] %s: %s %s\n", c.Name, cell.Result, cell.Note)
			if o.CI {
				for _, a := range annotations(cell) {
					_, _ = fmt.Fprintln(o.Out, a)
				}
			}
			sum.Cells = rep.Outcomes
			sum.Stop, sum.Exit = time.Now(), rep.Status()
			// ponytail: the whole summary is rewritten after each cell: about
			// 25 ms for the unit suite's 12,500 results, 0.2 s for 200,000.
			// Write each cell's results to a file of its own when a run's time
			// outside its cells, its stop less its start less its cells'
			// seconds, passes 10 seconds.
			write()
			freed.Broadcast()
		})
	}
	wg.Wait()
	return rep, writeErr
}

// conflict reports whether cells a and b can't run at once: either's suite
// doesn't say parallel, either needs free disk, which nothing reserves, both
// need the host's rpcbind, as a knfsd server does too, or both need the same port.
// The needs are the cell's, its fixture's among them.
//
// decision: a suite says parallel only once someone has checked that it
// holds nothing else of the host's: no fixed port outside its needs, no
// container with a fixed name, no lock, mount or service of the host's but
// rpcbind, nothing it measures (docs/internals/testing-parallel.md).
func conflict(a, b Cell) bool {
	if !a.Unit.Parallel || !b.Unit.Parallel {
		return true
	}
	disk := func(n string) bool { return strings.HasPrefix(n, "disk:") }
	if slices.ContainsFunc(a.Needs, disk) || slices.ContainsFunc(b.Needs, disk) {
		return true
	}
	hostRPC := func(c Cell) bool { return slices.Contains(c.Needs, "rpcbind") || slices.Contains(c.Needs, "knfsd") }
	if hostRPC(a) && hostRPC(b) {
		return true
	}
	return slices.ContainsFunc(a.Needs, func(n string) bool { return strings.HasPrefix(n, "port:") && slices.Contains(b.Needs, n) })
}

// prepare builds the run's {bin} and lays out its {tools} once, for cells
// that run at once. A step that fails is said here and left to the cells,
// which try it again and report it as their own.
func prepare(ctx context.Context, cells []Cell, o Options, dir string) {
	var tools []string
	bin := false
	for _, c := range cells {
		tools = append(tools, c.Unit.Tools()...)
		bin = bin || c.Unit.Fixture != nil && c.Unit.Fixture.Server != "knfsd" || c.Unit.Format == "go-json" && asRoot(c)
	}
	if bin {
		if err := buildBin(ctx, o.Root, filepath.Join(dir, "bin"), os.Environ(), o.Out); err != nil {
			_, _ = fmt.Fprintf(o.Out, "[dt] dfs and dfsctl didn't build: %v\n", err)
		}
	}
	if len(tools) > 0 {
		slices.Sort(tools)
		if err := layOutTools(ctx, o.Root, filepath.Join(dir, "tools"), slices.Compact(tools), o.Out); err != nil {
			_, _ = fmt.Fprintf(o.Out, "[dt] the tools weren't laid out: %v\n", err)
		}
	}
}

// prefixed passes a cell's output on with each line led by the cell's name, a
// whole line at a time, so cells that run at once don't cut into each other's
// lines. mu is the run's, shared by every cell's writer.
type prefixed struct {
	mu     *sync.Mutex
	w      io.Writer
	prefix string
	part   []byte // the line not ended yet
}

func (l *prefixed) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.part = append(l.part, p...)
	for {
		i := bytes.IndexByte(l.part, '\n')
		if i < 0 {
			return len(p), nil
		}
		if _, err := fmt.Fprintf(l.w, "%s%s\n", l.prefix, l.part[:i]); err != nil {
			return len(p), err
		}
		l.part = l.part[i+1:]
	}
}

// flush passes on a last line that has no end.
func (l *prefixed) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.part) > 0 {
		_, _ = fmt.Fprintf(l.w, "%s%s\n", l.prefix, l.part)
		l.part = nil
	}
}

// runName names a run by its start time and commit, and a job's re-run by its
// attempt too, as RUN-a2, so it never overwrites the first attempt's results.
func runName(start time.Time, commit string) string {
	name := start.UTC().Format("20060102T150405Z") + "-" + commit
	if n, _ := strconv.Atoi(os.Getenv("GITHUB_RUN_ATTEMPT")); n > 1 {
		name += "-a" + strconv.Itoa(n)
	}
	return name
}

// annotations are a cell's error annotations, one for each of what turns the
// run red: a new failure with its message, an unexpected pass naming the known
// entry a fix removes, or the cell itself with its note.
func annotations(c report.Cell) []string {
	tests, alone := c.Red()
	var out []string
	for _, t := range tests {
		message := cmp.Or(t.Message, "failed")
		if t.Grade == report.UnexpectedPass {
			message = "passed unexpectedly: remove its known entry (" + t.Reason + ")"
		}
		out = append(out, annotation("dt "+c.Name+": "+t.Title(), message))
	}
	if alone {
		out = append(out, annotation("dt "+c.Name, c.Result+": "+c.Note))
	}
	return out
}

// annotation is an error annotation with title and message, escaped as the
// workflow command's format asks: properties also escape : and ,.
func annotation(title, message string) string {
	data := strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A")
	property := strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A", ":", "%3A", ",", "%2C")
	return "::error title=" + property.Replace(title) + "::" + data.Replace(message)
}

// runCell runs one cell and returns how it went: its result, a note on it,
// its log once it has one, and its tests' results. A cell with a fixture has
// its server started before its first test and stopped after its last.
func runCell(ctx context.Context, c Cell, o Options, dir, commit string) (res report.Cell) {
	didntRun := func(why string) report.Cell { return report.Cell{Name: c.Name, Result: report.NotRun, Note: why} }
	if ctx.Err() != nil {
		return didntRun("interrupted")
	}
	matrix := c.Unit.Matrix.Fill(c.Matrix) // the matrix's placeholders
	var spec registry.Fixture              // the cell's fixture, its matrix values filled in
	if f := c.Unit.Fixture; f != nil {
		var err error
		if spec, err = cellFixture(*f, matrix); err != nil {
			return didntRun(err.Error())
		}
		// What dt can't do yet decides before the needs, as in Blocked.
		if why := fixture.Unsupported(&spec, hostOS); why != "" {
			return didntRun(why)
		}
	}
	// Docker meets a podman need the machine's own podman can't, and the
	// commands then run in podman's image: the need's one check decides both.
	podman := false
	check := func(n string) (err error) {
		if n == "host:"+o.Place {
			return nil
		}
		if n != "podman" {
			return needs.Check(o.Root, n)
		}
		podman, err = needs.Podman()
		return err
	}
	if why, unmet := Blocked(c.Unit, c.Needs, check); why != "" {
		// A machine that met the needs could run the cell, so without --ci
		// it's skipped; what dt can't do yet is never a skip.
		if len(unmet) > 0 && !o.CI {
			return report.Cell{Name: c.Name, Result: report.Skipped, Note: why}
		}
		return didntRun(why)
	}
	out := filepath.Join(dir, report.Slug(c.Name))
	vals := map[string]string{"base": base(), "out": out, "test": ""}
	maps.Copy(vals, matrix)
	lists := map[string][]string{"args": strings.Fields(c.Unit.Args[c.Tier]), "names": strings.Fields(c.Unit.Names)}
	if c.Unit.Tests.File != "" {
		lists["test-args"] = []string{""} // until each test's, from its line, fills it
	}
	if c.Unit.Packages != nil {
		pkgs, err := goPackages(o.Root, c.Unit.Packages.Tag)
		if err != nil {
			return didntRun(err.Error())
		}
		lists["packages"] = pkgs
	}
	if c.Unit.Format == "go-json" && asRoot(c) {
		vals["bin"] = filepath.Join(dir, "bin") // goJSON builds it before the commands run
	}
	if len(c.Unit.Tools()) > 0 {
		vals["tools"] = filepath.Join(dir, "tools") // laid out below, before the tests are found
	}
	if c.Unit.Fixture != nil {
		if spec.Server != "knfsd" { // the kernel's server runs no dfs
			vals["bin"] = filepath.Join(dir, "bin")
		}
		for _, k := range fixture.Placeholders(spec) {
			vals[k] = "" // filled once the server is up
		}
	}
	// A command dt can't fill can't run, whichever test it's for.
	for _, cmd := range c.Unit.Cmd {
		if _, err := fill(cmd, vals, lists); err != nil {
			return didntRun(err.Error())
		}
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return didntRun(err.Error())
	}
	logName := path.Join("logs", report.Slug(c.Name)+".log")
	log, err := os.Create(filepath.Join(dir, logName))
	if err != nil {
		return didntRun(err.Error())
	}
	defer func() { _ = log.Close() }()
	_, _ = fmt.Fprintf(log, "[dt] cell %s, tier %s, commit %s\n", c.Name, c.Tier, commit)
	w := io.MultiWriter(o.Out, log)
	// Once the log exists, a cell that doesn't run still points at it.
	didntRun = func(why string) report.Cell {
		return report.Cell{Name: c.Name, Result: report.NotRun, Note: why, Log: logName}
	}

	cellCtx, cancel := context.WithTimeout(ctx, time.Duration(c.TimeoutMinutes)*minute)
	defer cancel()
	if vals["tools"] != "" {
		if err := layOutTools(cellCtx, o.Root, vals["tools"], c.Unit.Tools(), w); err != nil {
			return didntRun("the test tools: " + err.Error())
		}
	}
	tests, lines, err := plan.ListTests(o.Root, c.Unit, "")
	if err != nil {
		return didntRun(err.Error())
	}
	if len(c.Tests) > 0 {
		tests = c.Tests
	}
	var paths map[string]string // each test's file, when only the cell finds them
	if c.Unit.Tests.Deferred() {
		if tests, paths, err = deferredTests(o.Root, c.Unit, vals, lists, c.Tests); err != nil {
			return didntRun(err.Error())
		}
	}
	if !c.Unit.Tests.Set() {
		tests = []string{""} // the commands are the one test
	}
	if len(tests) == 0 {
		return didntRun("no tests found")
	}
	// The fixture's own steps share one limit: building {bin}, starting the
	// server and pulling the image, so a stalled module download or pull
	// leaves the cell not run instead of using up its limit. Each is tried
	// once: the server starts inside the fixture, so a retry could hide its
	// flakiness.
	setup, setupDone := context.WithTimeout(cellCtx, fixtureSetup)
	defer setupDone()
	setupFailed := func(step string, err error) report.Cell {
		switch {
		case cellCtx.Err() != nil:
			return didntRun(stopped(cellCtx))
		case setup.Err() != nil:
			return didntRun(fmt.Sprintf("the fixture wasn't set up within %s, and isn't retried", fixtureSetup))
		}
		return didntRun(step + ": " + err.Error())
	}
	var server fixtureServer
	if c.Unit.Fixture != nil {
		if err := buildBin(setup, o.Root, vals["bin"], os.Environ(), w); err != nil {
			return setupFailed("the fixture", fmt.Errorf("dfs and dfsctl didn't build: %w", err))
		}
		if spec.Kerberos {
			build := []string{"docker", "build", "-q", "-t", fixture.KDCImage, filepath.Join("test", "smb-conformance", "kdc")}
			_, _ = fmt.Fprintf(w, "[dt] $ %s\n", strings.Join(build, " "))
			code, err := execute(setup, build, o.Root, os.Environ(), w, false)
			if err == nil && code != 0 {
				err = fmt.Errorf("docker build exited %d", code)
			}
			if err != nil {
				return setupFailed("the fixture", fmt.Errorf("the KDC's image didn't build: %w", err))
			}
		}
		if spec.Shares != "" {
			spec.Shares = filepath.Join(o.Root, spec.Shares)
		}
		server, err = startFixture(setup, spec, needs.ServiceEnv(o.Root, c.Needs), vals["bin"], out, w)
		if err != nil {
			return setupFailed("the fixture", err)
		}
		defer func() {
			// The code under test crashing fails its cell, unless an
			// interrupt stopped the cell first.
			if err := server.Stop(); err != nil && ctx.Err() == nil {
				_, _ = fmt.Fprintf(w, "[dt] fixture: %v\n", err)
				res.Result, res.Note = report.Failed, strings.TrimSuffix("the fixture: "+err.Error()+"; "+res.Note, "; ")
			}
		}()
		maps.Copy(vals, server.Placeholders())
	}
	own, err := suiteEnv(c.Unit, c.Tier, vals, lists)
	if err != nil {
		return didntRun(err.Error())
	}
	// With an image, dir is a directory in the container, and the docker
	// client runs from the repository root.
	workdir, inside := o.Root, ""
	if c.Unit.Dir != "" {
		d, err := replace(c.Unit.Dir, vals, lists, nil)
		if err != nil {
			return didntRun(err.Error())
		}
		switch {
		case c.Unit.Image != "":
			inside = d
		case filepath.IsAbs(d):
			workdir = d
		default:
			workdir = filepath.Join(o.Root, d)
		}
	}
	// The templates take the fixture's placeholders, so they're filled once
	// it's up.
	if c.Unit.Templates != "" {
		if err := renderTemplates(o.Root, c.Unit.Templates, filepath.Join(out, "config"), vals, lists); err != nil {
			return didntRun("the templates: " + err.Error())
		}
	}
	if c.Unit.Image != "" {
		if err := pull(setup, c.Unit.Image, o.Root, os.Environ(), w); err != nil {
			return setupFailed("the image", err)
		}
	}
	if podman {
		if err := preparePodman(setup, o.Root, os.Environ(), w); err != nil {
			return setupFailed("podman's image", err)
		}
	}
	setupDone()
	t := tester{cell: c, ctx: cellCtx, root: o.Root, dir: workdir, inside: inside, podman: podman, env: append(os.Environ(), own...), own: own, vals: vals, lists: lists, paths: paths, lines: lines, server: server, out: w, log: log}
	if c.Unit.Format == "go-json" {
		result, note, tests := t.goJSON(c.Tests, o.Retry)
		return report.Cell{Name: c.Name, Result: result, Note: note, Log: logName, Tests: tests}
	}
	read := readers[c.Unit.Format]
	var failed, notRun, didntStart []string
	timedOut := map[string]string{} // why each test that timed out failed
	interrupted := false
	first := vals["share"] // the fixture's first share, which a test's share= replaces
	cold := true           // until the cell's first test runs
	// attempt runs one test, and returns its results and why it ran nothing,
	// when it didn't.
	attempt := func(test string) ([]report.Test, []string) {
		prefix := ""
		if test != "" {
			prefix = test + ": "
		}
		if first != "" {
			vals["share"] = cmp.Or(lines[test].Share, first)
		}
		if spec.Reset == "each" {
			// decision: a test whose share wasn't reset doesn't run, since it
			// would run on what the test before it left there. The reset is
			// one of the fixture's steps, so it isn't retried.
			if err := server.Reset(vals["share"]); err != nil {
				return nil, []string{prefix + "its share wasn't reset: " + err.Error()}
			}
		}
		var output bytes.Buffer
		if read != nil {
			t.capture = &output
		}
		start := time.Now()
		r := t.run(test, c.limit(test, lines, cold))
		took := time.Since(start) // the test's time, without the copy of its logs below
		cold = false
		if t.podman && r.result == report.Failed {
			t.keepLogs(test, r.timedOut)
		}
		interrupted = interrupted || r.interrupted
		var why []string
		if len(r.notRun) > 0 {
			why = append(why, prefix+strings.Join(r.notRun, ", "))
		}
		// A command that doesn't start for one test mostly doesn't for any,
		// in the same way, so each reason is noted once.
		for _, reason := range r.didntStart {
			if !slices.Contains(didntStart, reason) {
				didntStart = append(didntStart, reason)
			}
		}
		switch {
		case read == nil && test == "":
			return r.steps, why // exit without tests: each command is a test case
		case read == nil && r.result == report.NotRun:
			return nil, why
		case read == nil:
			tc := report.Test{Name: test, Status: report.Passed, Duration: took}
			if r.result == report.Failed {
				tc.Status, tc.Message = report.Failed, r.note
			}
			if r.timedOut {
				tc.Grade = report.New // a hang, which no entry excuses
			}
			// decision: dt reads fio-json alone. dfsbench-json's numbers are
			// for the tracked benchmark runs, and aren't recorded until then.
			if c.Unit.Metrics == "fio-json" {
				// A job that continues on errors exits 0, so its report
				// decides too.
				numbers, failed, err := readFio(filepath.Join(out, cmp.Or(paths[test], test)+".json"))
				switch {
				case len(failed) > 0:
					tc.Status, tc.Message = report.Failed, strings.TrimSuffix(strings.Join(append(failed, r.note), "; "), "; ")
				case err != nil && tc.Status == report.Passed:
					// decision: a test that passed without a report has no
					// numbers, which is lost coverage: its cell isn't run.
					why = append(why, prefix+"fio's report: "+err.Error())
				}
				tc.Metrics = numbers
			}
			return []report.Test{tc}, why
		case r.interrupted:
			return nil, why // its not-run note says so, and nothing it reported counts
		case len(r.didntStart) > 0:
			// It ran in part at most, so what a reader finds isn't its
			// result: it has none, and the note says why.
			return nil, why
		}
		got, err := read(report.Ran{Test: test, Prefix: lines[test].Prefix, Output: output.Bytes(), Exit: r.exit, TimedOut: r.timedOut, Took: took, Out: out})
		if err != nil {
			why = append(why, prefix+err.Error())
		}
		if r.timedOut {
			// A test that timed out failed, whatever its results say: the
			// reader can only say which of its tests was open.
			timedOut[test] = prefix + r.note
		}
		return got, why
	}
	var cases []report.Test
	var from []string // the test each result came from
	unstarted := 0
	for _, test := range tests {
		if cellCtx.Err() != nil {
			unstarted++
			continue
		}
		got, why := attempt(test)
		cases = append(cases, got...)
		from = append(from, slices.Repeat([]string{test}, len(got))...)
		notRun = append(notRun, why...)
	}
	notRun = append(notRun, didntStart...)
	if unstarted > 0 {
		notRun = append(notRun, fmt.Sprintf("%d tests not started: %s", unstarted, stopped(cellCtx)))
		interrupted = interrupted || errors.Is(cellCtx.Err(), context.Canceled)
	}
	if read != nil && len(cases) == 0 {
		// The results decide, not the exit status: a cell with none didn't
		// run.
		notRun = append(notRun, "no results")
	}
	// A retry runs again what gave a new failure, on the same fixture and
	// within the cell's limit: each test it came from, or, for a tool that
	// takes test names, the test by name through {names}. What else the retry
	// meets doesn't count: it only decides which failures were flaky.
	//
	// decision: a suite whose tests dt can't name, a command list or a tool
	// run once for all its tests without names, isn't retried.
	for n := 1; n <= o.Retry && !interrupted && cellCtx.Err() == nil; n++ {
		graded := slices.Clone(cases)
		c.grade(graded)
		var again, names []string
		for i, tc := range graded {
			switch {
			case tc.Grade != report.New:
			case c.Unit.Names != "":
				names = append(names, tc.Name)
			case c.Unit.Tests.Set():
				again = append(again, from[i])
			}
		}
		again = slices.Compact(again)
		shown := again
		if len(names) > 0 {
			lists["names"] = slices.Compact(names)
			again, shown = []string{""}, lists["names"]
		}
		if len(again) == 0 {
			break
		}
		_, _ = fmt.Fprintf(w, "[dt] retry %d: %s\n", n, strings.Join(shown, ", "))
		// A retry by name runs the same command, which writes its results
		// where the attempt before it did: those are kept, under a name no
		// reader reads.
		if ext := resultExt[c.Unit.Format]; len(names) > 0 && ext != "" {
			for _, f := range resultFiles(out, ext) {
				if err := os.Rename(f, f+".attempt-"+strconv.Itoa(n)); err != nil {
					_, _ = fmt.Fprintf(w, "[dt] %v\n", err)
				}
			}
		}
		var results []report.Test
		for _, test := range again {
			if cellCtx.Err() != nil {
				break
			}
			// Only a test run whole again can clear its timeout: a retry by
			// name runs the failed tests, not those a timeout left unrun.
			if len(names) == 0 {
				delete(timedOut, test)
			}
			got, why := attempt(test)
			results = append(results, got...)
			if interrupted {
				notRun = append(notRun, why...)
			}
		}
		retried(cases, graded, results, n)
	}
	if interrupted {
		// The cell didn't finish, and what its tests reported once
		// interrupted can come from the interrupt itself.
		cases = nil
	}
	for _, test := range tests {
		if why, ok := timedOut[test]; ok {
			failed = append(failed, why)
		}
	}
	// The grades decide how the cell went. A new failure fails it: the note
	// gives an exit suite's failing tests, or its failing commands, each with
	// why, and a reader's by name. A flaky test passes, and is named.
	c.grade(cases)
	var names, flaky []string
	for _, tc := range cases {
		switch {
		case tc.Flaky:
			flaky = append(flaky, tc.Name)
		case tc.Grade != report.New:
		case read != nil:
			names = append(names, tc.Name)
		case c.Unit.Tests.Set():
			failed = append(failed, tc.Name+": "+tc.Message)
		default:
			failed = append(failed, tc.Message)
		}
	}
	if len(names) > 0 {
		failed = append(failed, fmt.Sprintf("%d failed: %s", len(names), some(names)))
	}
	unexpected, lost := gradeNotes(cases)
	failed, notRun = append(failed, unexpected...), append(notRun, lost...)
	notes := slices.Concat(failed, notRun)
	if len(flaky) > 0 {
		notes = append(notes, fmt.Sprintf("%d flaky: %s", len(flaky), some(flaky)))
	}
	cell := report.Cell{Name: c.Name, Result: report.Passed, Note: strings.Join(notes, "; "), Log: logName, Tests: cases}
	switch {
	case len(failed) > 0:
		cell.Result = report.Failed
	case len(notRun) > 0:
		cell.Result = report.NotRun
	}
	return cell
}

// retried records the n-th retry of the results graded holds as new
// failures, from the results the retry gave, again: a failure whose test
// passed in every result of the retry passes, flaky, and each failure the
// retry ran again counts it. cases are the results as reported, before
// grading, so a flaky one is graded as a pass.
func retried(cases, graded, again []report.Test, n int) {
	ran, failed := map[string]bool{}, map[string]bool{}
	for _, tc := range again {
		ran[tc.Title()] = true
		failed[tc.Title()] = failed[tc.Title()] || tc.Status != report.Passed
	}
	for i := range cases {
		test := cases[i].Title()
		if graded[i].Grade != report.New || !ran[test] {
			continue
		}
		cases[i].Retries = n
		if !failed[test] {
			cases[i].Status, cases[i].Grade, cases[i].Flaky = report.Passed, "", true
		}
	}
}

// limit is a test's limit, zero for none. A scenario's size limit assumes
// setup.sh's images and caches are there, so the cell's first scenario, which
// fills them, also gets the fixture's setup time, which the cell's limit
// counts though the suite has no fixture.
func (c Cell) limit(test string, lines map[string]plan.Line, first bool) time.Duration {
	limit := plan.TestLimit(c.Unit.Timeout, test, lines)
	if first && limit > 0 && c.Unit.Timeout.Test == "size" {
		limit += fixtureSetup
	}
	return limit
}

// grade grades the cell's results against its suite's known file, where the
// cell runs on this machine.
func (c Cell) grade(cases []report.Test) {
	known.Grade(cases, c.Unit.Known != "", c.Known, c.At(runtime.GOOS))
}

// At is where the cell runs on goos, as a known entry's where names it: its
// matrix values, its tier, its option and the OS, named as the platform need
// names it.
func (c Cell) At(goos string) map[string]string {
	at := map[string]string{"tier": c.Tier, "option": c.Option, "os": goos}
	if goos == "darwin" {
		at["os"] = "macos"
	}
	maps.Copy(at, c.Matrix)
	return at
}

// gradeNotes names the results that fail their cell, or leave it not run, by
// their grade alone: unexpected passes, then unexpected skips and incomplete
// results.
func gradeNotes(cases []report.Test) (failed, notRun []string) {
	names := map[string][]string{}
	for _, tc := range cases {
		names[tc.Grade] = append(names[tc.Grade], tc.Name)
	}
	for _, g := range []struct {
		grade, says string
		to          *[]string
	}{
		{report.UnexpectedPass, "passed unexpectedly", &failed},
		{report.UnexpectedSkip, "skipped unexpectedly", &notRun},
		{report.Incomplete, "incomplete", &notRun},
	} {
		if n := names[g.grade]; len(n) > 0 {
			*g.to = append(*g.to, fmt.Sprintf("%d %s: %s", len(n), g.says, some(n)))
		}
	}
	return failed, notRun
}

// readers read the formats whose results a tool writes: every format but
// exit and go-json, which dt reads itself.
var readers = map[string]report.Reader{"junit": readJUnit, "nfstest": readNFStest, "subunit": readSubunit, "tap": readTAP, "trx": readTRX}

// exitNotRun is the exit status of a command that ran nothing, as the
// scenarios' setup gives when another run holds their lock.
const exitNotRun = 75

// tester runs one cell's tests.
type tester struct {
	cell     Cell
	ctx      context.Context
	root     string   // the repository
	dir      string   // where the commands run, or the docker client for an image
	inside   string   // where the commands run in the image, when dir sets it
	podman   bool     // the commands run in podman's image
	env      []string // dt's environment and the suite's
	own      []string // the suite's alone
	vals     map[string]string
	lists    map[string][]string
	paths    map[string]string    // the file each test runs as {test}, when it isn't its name
	lines    map[string]plan.Line // what each test's line in a .txt list gives it
	server   fixtureServer        // the cell's fixture, which may run the commands on its client
	out, log io.Writer
	capture  io.Writer // also gets the commands' output, for a reader
}

// testRun is how one test's commands went.
type testRun struct {
	result      string        // how the test went
	note        string        // why it failed, when it did
	notRun      []string      // why commands of it ran nothing, an interrupt included
	didntStart  []string      // why commands of it didn't start, each named by its first line
	steps       []report.Test // each command that ran to a result, named by its first line
	exit        int           // the first non-zero exit status
	timedOut    bool          // stopped at the test's limit or the cell's
	interrupted bool          // stopped by an interrupt
}

// run runs every command for one test, all of them even after one fails, and
// says how the test went: failed when a command failed or the test ran out of
// time, not run when a command ran nothing or the run was interrupted, else
// passed. A command skipped for want of {base} is a skipped step. A limit of
// zero is no limit but the cell's.
func (t tester) run(test string, limit time.Duration) testRun {
	ctx := t.ctx
	if limit > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, limit)
		defer cancel()
	}
	t.vals["test"] = cmp.Or(t.paths[test], test)
	if _, ok := t.lists["test-args"]; ok {
		t.lists["test-args"] = nil
		for _, a := range t.lines[test].Args {
			v, err := replace(a, t.vals, nil, nil)
			if err != nil {
				return testRun{result: report.NotRun, notRun: []string{err.Error()}}
			}
			t.lists["test-args"] = append(t.lists["test-args"], v)
		}
	}
	var r testRun
	var failed, notRun []string
	image := t.cell.Unit.Image
	cmds := t.cell.Unit.Cmd
	if len(cmds) == 0 {
		cmds = registry.Commands{{}} // the image's own command
	}
	for _, cmd := range cmds {
		step := report.Test{Name: cmp.Or(cmdName(cmd), image), Status: report.Failed}
		if t.vals["base"] == "" && slices.Contains(registry.Placeholders(cmd.Line+" "+cmd.Sh), "base") {
			_, _ = fmt.Fprintf(t.out, "[dt] skipped, no change to check: %s%s\n", cmd.Line, cmd.Sh)
			step.Status, step.Message = report.Skipped, "no change to check"
			r.steps = append(r.steps, step)
			continue
		}
		var argv []string
		var err error
		if cmd != (registry.Command{}) {
			if argv, err = fill(cmd, t.vals, t.lists); err != nil {
				return testRun{result: report.NotRun, notRun: []string{err.Error()}}
			}
		}
		// decision: dt makes the directories of a file below {out} that an
		// argument names, so a test named by its path, as a glob's are, can
		// write {out}/{test}.json. A script makes its own: its values are
		// quoted, so dt doesn't look for paths in it.
		if cmd.Line != "" {
			for _, p := range belowOut(argv, t.vals["out"]) {
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					return testRun{result: report.NotRun, notRun: []string{err.Error()}}
				}
			}
		}
		shown := cmp.Or(strings.Join(argv, " "), image)
		if cmd.Sh != "" {
			shown = argv[2]
		}
		printed, container := shown, ""
		if image != "" {
			if argv, container, err = t.inImage(argv); err != nil {
				return testRun{result: report.NotRun, notRun: []string{err.Error()}}
			}
			printed = strings.Join(argv, " ")
		}
		if t.podman {
			argv, container = t.inPodman(argv)
			printed = strings.Join(argv, " ")
		}
		if t.server != nil {
			wrapped, err := t.server.Command(argv, t.dir, t.own)
			if err != nil {
				return testRun{result: report.NotRun, notRun: []string{err.Error()}}
			}
			if !slices.Equal(wrapped, argv) {
				argv, printed = wrapped, strings.Join(wrapped, " ")
			}
		}
		dir, sudo := t.dir, asRoot(t.cell)
		if sudo {
			var hidden []string
			argv, hidden = sudoIn(t.dir, t.own, argv)
			printed, dir = strings.Join(hidden, " "), t.root
		}
		if pw := t.vals["password"]; pw != "" { // a credential stays out of the log
			shown = strings.ReplaceAll(shown, pw, "{password}")
			printed = strings.ReplaceAll(printed, pw, "{password}")
		}
		_, _ = fmt.Fprintf(t.out, "[dt] $ %s\n", printed)
		w := t.out
		if t.capture != nil {
			w = io.MultiWriter(t.out, t.capture)
		}
		start := time.Now()
		code, err := execute(ctx, argv, dir, t.env, w, sudo)
		step.Duration = time.Since(start)
		// A docker client that started, which execute says by returning no
		// error, and was then killed leaves its container running.
		if container != "" && err == nil && ctx.Err() != nil {
			removeContainer(container, t.dir, t.env, w)
		}
		_, _ = fmt.Fprintf(t.log, "[dt] exit %d after %s\n", code, step.Duration.Round(time.Millisecond))
		if r.exit == 0 {
			r.exit = code
		}
		switch {
		case ctx.Err() != nil && t.ctx.Err() == nil:
			step.Message, step.Grade = fmt.Sprintf("timed out after %s", limit), report.New // a hang, which no entry excuses
		case errors.Is(t.ctx.Err(), context.DeadlineExceeded):
			step.Message, step.Grade = "stopped at the cell's limit", report.New
		case t.ctx.Err() != nil:
			r.result, r.notRun, r.interrupted = report.NotRun, []string{"interrupted"}, true
			return r
		case err != nil:
			// A command that didn't start ran nothing: lost coverage, not a
			// regression, as with a container that didn't start.
			r.didntStart = append(r.didntStart, fmt.Sprintf("`%s` didn't start: %v", step.Name, err))
			continue
		case sudo && notStarted(code):
			// Run as root, the command is started by env, which exits 127
			// when it can't find it, 126 when it can't run it and 125 when it
			// fails itself, as when it can't enter dir.
			r.didntStart = append(r.didntStart, fmt.Sprintf("`%s` didn't start: env exited %d", step.Name, code))
			continue
		case code == exitNotRun:
			notRun = append(notRun, fmt.Sprintf("`%s` exited %d: it ran nothing", firstLine(shown), code))
			continue
		case (image != "" || t.podman) && notStarted(code):
			notRun = append(notRun, fmt.Sprintf("`%s` exited %d: its container didn't start", firstLine(shown), code))
			continue
		case code != 0:
			step.Message = fmt.Sprintf("`%s` exited %d", firstLine(shown), code)
			failed = append(failed, step.Message)
		default:
			step.Status = report.Passed
		}
		r.steps = append(r.steps, step)
		if ctx.Err() != nil { // the test, or the cell, ran out of time
			r.result, r.note, r.notRun, r.timedOut = report.Failed, step.Message, notRun, true
			return r
		}
	}
	r.notRun = notRun
	switch {
	case len(failed) > 0:
		r.result, r.note = report.Failed, strings.Join(failed, ", ")
	case len(notRun) > 0 || len(r.didntStart) > 0:
		r.result = report.NotRun
	default:
		r.result = report.Passed
	}
	return r
}

// asRoot reports whether a cell's commands run as root through sudo: those of
// a suite whose own needs name root, run by dt as a user. The needs its
// fixture implies are for the fixture's own steps, which call sudo themselves.
func asRoot(c Cell) bool {
	return slices.Contains(c.Unit.Needs, "root") && os.Geteuid() != 0
}

// sudoIn is argv run as root in dir, and the same command to print, with each
// variable as NAME=$NAME, so no value reaches the terminal or the log. sudo
// resets the environment, so env gives the command PATH and the suite's
// variables, own, explicitly. env enters dir too, as root: the user dt runs
// as may have no access to it, as to a mount whose share grants that user
// nothing.
//
// decision: the values of own are on sudo's command line, where any user can
// read them while it runs, as the variables a Go suite's tests get through
// -exec are. No suite that runs as root has a credential in its env: pass
// such a variable through sudo's --preserve-env if one ever does.
func sudoIn(dir string, own, argv []string) (run, shown []string) {
	vars := append([]string{"PATH=" + os.Getenv("PATH")}, own...)
	hidden := make([]string, len(vars))
	for i, v := range vars {
		name, _, _ := strings.Cut(v, "=")
		hidden[i] = name + "=$" + name
	}
	sudo := []string{"sudo", "-n", "env", "-C", dir}
	return slices.Concat(sudo, vars, argv), slices.Concat(sudo, hidden, argv)
}

// belowOut are the paths below the cell's directory, out, that arguments
// name, as --output={out}/x.json names one.
func belowOut(argv []string, out string) []string {
	var paths []string
	for _, a := range argv {
		if _, rest, ok := strings.Cut(a, out); ok && (strings.HasPrefix(rest, "/") || strings.HasPrefix(rest, string(filepath.Separator))) {
			paths = append(paths, out+rest)
		}
	}
	return paths
}

// cmdName names a command as a test case: its first line, as the registry
// writes it, so the name is the same whatever its placeholders hold.
func cmdName(c registry.Command) string {
	first, _, _ := strings.Cut(strings.TrimSpace(c.Line+c.Sh), "\n")
	return strings.TrimSpace(first)
}

// some joins names, the first five and how many more.
func some(names []string) string {
	if len(names) > 5 {
		names = append(names[:5:5], fmt.Sprintf("and %d more", len(names)-5))
	}
	return strings.Join(names, ", ")
}

// firstLine names a command in a note by its first line, so a multi-line
// script keeps the note, and an annotation made from it, on one line.
func firstLine(s string) string {
	first, rest, _ := strings.Cut(s, "\n")
	if strings.TrimSpace(rest) != "" {
		return first + " ..."
	}
	return first
}

// stopped says why the cell's context ended.
func stopped(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "stopped at the cell's limit"
	}
	return "interrupted"
}

// execute runs one command with its output going to w, and returns its exit
// status. A command that outlives ctx is killed with everything it started,
// and whatever a command leaves running in its process group is killed when
// it exits. One that runs as root through sudo, which dt can't kill, is
// interrupted instead, as a Go suite that runs as root is.
func execute(ctx context.Context, argv []string, dir string, env []string, w io.Writer, sudo bool) (int, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = dir, env
	// The command writes to a pipe of dt's own, not one exec makes, so Wait
	// returns when the command exits, whatever it left holding the output.
	r, pw, err := os.Pipe()
	if err != nil {
		return -1, err
	}
	cmd.Stdout, cmd.Stderr = pw, pw
	if sudo {
		interruptTree(cmd, sudoGrace)
	} else {
		killTree(cmd)
	}
	err = cmd.Start()
	_ = pw.Close()
	if err != nil {
		_ = r.Close()
		return -1, err
	}
	copied := make(chan struct{})
	go func() {
		_, _ = io.Copy(w, r)
		close(copied)
	}()
	err = cmd.Wait()
	killLeftovers(cmd)
	// What still holds the output now runs outside the command's group:
	// give it a moment to finish writing, then stop reading.
	select {
	case <-copied:
	case <-time.After(5 * time.Second):
		_ = r.Close()
		<-copied
	}
	_ = r.Close()
	return exitCode(err)
}

// exitCode turns a command's error into its exit status, keeping the error
// only when the command didn't run to an exit.
func exitCode(err error) (int, error) {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

// hostOS is the platform dt runs on, as runtime.GOOS names it; the tests set it.
var hostOS = runtime.GOOS

// Container reports whether dt run --in auto moves the run into dtc: a cell
// that can't run here can there, as the container meets every need check
// finds missing, and no cell needs what is met here but not in the container,
// as another platform. A cell dtc can't run counts for neither, unless it runs
// here, which keeps the run here.
func Container(cells []Cell, check func(need string) error) bool {
	move := false
	for _, c := range cells {
		if inDtc(c.Unit) != "" {
			if unsupported(c.Unit, hostOS) == "" && !slices.ContainsFunc(c.Needs, func(n string) bool { return check(n) != nil }) {
				return false
			}
			continue
		}
		missing, there := false, true
		for _, n := range c.Needs {
			here := check(n) == nil
			inside := needs.InContainer(n) || strings.HasPrefix(n, "service:") && here
			if here && !inside {
				return false
			}
			if !here {
				missing, there = true, there && inside
			}
		}
		move = move || missing && there
	}
	return move
}

// Blocked says why dt can't run a cell of suite u that needs list on this
// machine, or returns "" when it can. What dt doesn't do yet comes first, and
// then each need that check finds unmet, as "need: reason"; unmet names those
// needs, and is empty when what stops the cell is something dt doesn't do
// yet. dt run and dt list both decide through it, so they can't disagree.
func Blocked(u *registry.Suite, list []string, check func(need string) error) (why string, unmet []string) {
	if why := unsupported(u, hostOS); why != "" {
		return why, nil
	}
	var reasons []string
	for _, n := range list {
		if err := check(n); err != nil {
			unmet = append(unmet, n)
			reasons = append(reasons, n+": "+err.Error())
		}
	}
	return strings.Join(reasons, "; "), unmet
}

// inDtc says why dt can't run suite u in dtc, or returns "": dtc meets the
// linux need and runs dt as root, which a fixture whose users hold self
// refuses.
func inDtc(u *registry.Suite) string {
	if why := unsupported(u, "linux"); why != "" {
		return why
	}
	if u.Fixture != nil && slices.Contains(u.Fixture.Users, "self") {
		return "users: self needs an ordinary user, and dtc runs dt as root"
	}
	return ""
}

// unsupported says why dt can't run the suite yet on platform goos, or
// returns "": only a fixture field dt doesn't do yet, since it reads every
// format.
func unsupported(u *registry.Suite, goos string) string {
	if u.Format == "go-json" && u.Fixture != nil && u.Fixture.Client != "" {
		return "dt doesn't run go test on a namespaced client yet"
	}
	// decision: no suite that runs as root has a namespaced client, so dt
	// doesn't run one. Its commands would enter the client's namespaces as
	// root, without dropping to the user, as they do when dt runs as root.
	if slices.Contains(u.Needs, "root") && u.Fixture != nil && u.Fixture.Client != "" {
		return "dt doesn't run a suite that needs root on a namespaced client yet"
	}
	if u.Fixture != nil {
		return fixture.Unsupported(u.Fixture, goos)
	}
	return ""
}

// base is the ref a change is compared against: the base branch of the pull
// request being checked, a merge group's base commit, the commit before the
// push being checked, or origin/develop when dt checks no event. It's "" when
// the event has no change to check: a schedule, a dispatch, or a push without
// a previous commit.
func base() string {
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		return "origin/develop"
	}
	switch os.Getenv("GITHUB_EVENT_NAME") {
	case "pull_request":
		return "origin/" + os.Getenv("GITHUB_BASE_REF")
	case "push", "merge_group":
		var event struct {
			Before     string `json:"before"`
			MergeGroup struct {
				BaseSHA string `json:"base_sha"`
			} `json:"merge_group"`
		}
		data, err := os.ReadFile(os.Getenv("GITHUB_EVENT_PATH"))
		if err != nil || json.Unmarshal(data, &event) != nil {
			return ""
		}
		if event.MergeGroup.BaseSHA != "" {
			return event.MergeGroup.BaseSHA
		}
		if strings.Trim(event.Before, "0") != "" {
			return event.Before
		}
	}
	return ""
}

// newRunDir makes the run's directory and its logs directory. A run started
// in the same second as another gets the next free suffix, -2, -3 and so on.
func newRunDir(results, name string) (string, error) {
	if err := os.MkdirAll(results, 0o755); err != nil {
		return "", err
	}
	for i := 1; ; i++ {
		dir := filepath.Join(results, name)
		if i > 1 {
			dir += "-" + strconv.Itoa(i)
		}
		err := os.Mkdir(dir, 0o755)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		return dir, os.Mkdir(filepath.Join(dir, "logs"), 0o755)
	}
}

// gitState is the checkout's commit, shortened, or "nocommit" outside a
// checkout or before its first commit; its branch, "" when it's on none; and
// whether it has changes, untracked files included, that its commit doesn't.
// One git status gives all three, and it takes no lock, so a git command
// running beside it isn't held up.
func gitState(root string) (commit, branch string, dirty bool) {
	commit = "nocommit"
	out, err := exec.Command("git", "--no-optional-locks", "-C", root, "status", "--porcelain=v2", "--branch").Output()
	if err != nil {
		return commit, "", false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if oid, ok := strings.CutPrefix(line, "# branch.oid "); ok && oid != "(initial)" {
			commit = oid[:min(8, len(oid))]
		}
		if head, ok := strings.CutPrefix(line, "# branch.head "); ok && head != "(detached)" {
			branch = head
		}
		if line != "" && !strings.HasPrefix(line, "#") {
			dirty = true
		}
	}
	return commit, branch, dirty
}

// Package run runs the cells of a dt run: each test's commands under the
// test's and the cell's limits, with their output passed through and logged,
// and each test's results kept in the run's summary.
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
	"strconv"
	"strings"
	"time"

	"github.com/marmos91/dittofs/test/harness/fixture"
	"github.com/marmos91/dittofs/test/harness/needs"
	"github.com/marmos91/dittofs/test/harness/plan"
	"github.com/marmos91/dittofs/test/harness/registry"
	"github.com/marmos91/dittofs/test/harness/report"
)

// How a cell went.
const (
	Passed  = "passed"
	Failed  = "failed"  // a test failed or timed out
	NotRun  = "not run" // it couldn't run, or its limit left tests unstarted
	Skipped = "skipped" // without --ci, this machine doesn't meet its needs
)

// Outcome is how one cell went.
type Outcome struct {
	Cell   string
	Result string
	Note   string
	Took   time.Duration
}

// Report is a whole run.
type Report struct {
	Dir      string // the run's results directory
	Outcomes []Outcome
}

// Status is the run's exit status: 1 when a cell failed, else 2 when one
// didn't run or nothing ran, else 0.
func (r Report) Status() int {
	ran, notRun := false, false
	for _, o := range r.Outcomes {
		switch o.Result {
		case Failed:
			return 1
		case NotRun:
			notRun = true
		case Passed:
			ran = true
		}
	}
	if notRun || !ran {
		return 2
	}
	return 0
}

// Markdown is the run's outcomes as a Markdown table under title, for a job
// summary.
func (r Report) Markdown(title string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### %s\n\n| Cell | Result | Time | Note |\n|---|---|---|---|\n", title)
	for _, o := range r.Outcomes {
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", o.Cell, o.Result, o.Took, strings.ReplaceAll(o.Note, "|", `\|`))
	}
	return b.String()
}

// Options say where a run happens and writes.
type Options struct {
	Root    string    // the repository
	Results string    // where runs are written, each in a directory of its own
	CI      bool      // --ci: an unmet need is an error, not a skip, and problems are annotated
	Out     io.Writer // where the commands' output passes through
	Tier    string    // the tier the run was asked for, recorded in its results
}

// minute is a variable so the tests can shorten a cell's limit.
var minute = time.Minute

// Run runs the cells one after another, each logged under a new directory in
// o.Results, where summary.json holds every finished cell's results: it's
// written when the run starts and rewritten after each cell, so a run stopped
// at any point still has them. A summary.json that can't be written doesn't
// stop the run: dt says so, and Run returns the first such error once the
// cells are done. Cancelling ctx stops the cell running and the ones after it.
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
	sum := report.Run{Name: filepath.Base(dir), Start: start, Stop: start, Commit: commit, Branch: branch, Dirty: dirty, OS: runtime.GOOS, In: "host", Tier: o.Tier, Exit: rep.Status()}
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
	for _, c := range cells {
		start := time.Now()
		cell := runCell(ctx, c, o, rep.Dir, commit)
		cell.Took = time.Since(start).Round(time.Second)
		rep.Outcomes = append(rep.Outcomes, Outcome{c.Name, cell.Result, cell.Note, cell.Took})
		_, _ = fmt.Fprintf(o.Out, "[dt] %s: %s %s\n", c.Name, cell.Result, cell.Note)
		if o.CI && (cell.Result == Failed || cell.Result == NotRun) {
			_, _ = fmt.Fprintln(o.Out, annotation("dt "+c.Name, cell.Result+": "+cell.Note))
		}
		ungraded(c.Unit, cell.Tests)
		sum.Cells = append(sum.Cells, cell)
		sum.Stop, sum.Exit = time.Now(), rep.Status()
		// ponytail: the whole summary is rewritten after each cell: about
		// 25 ms for the unit suite's 12,500 results, 0.2 s for 200,000.
		// Write each cell's results to a file of its own when a run's time
		// outside its cells, its stop less its start less its cells'
		// seconds, passes 10 seconds.
		write()
	}
	return rep, writeErr
}

// ungraded grades the skips of a suite without a known file: they're
// reported and counted, and pass.
func ungraded(u *registry.Suite, tests []report.Test) {
	if u.Known != "" {
		return
	}
	for i := range tests {
		if tests[i].Status == report.Skipped {
			tests[i].Grade = report.Ungraded
		}
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
	didntRun := func(why string) report.Cell { return report.Cell{Name: c.Name, Result: NotRun, Note: why} }
	if ctx.Err() != nil {
		return didntRun("interrupted")
	}
	var spec registry.Fixture // the cell's fixture, its matrix values filled in
	if f := c.Unit.Fixture; f != nil {
		var err error
		if spec, err = cellFixture(*f, c.Matrix); err != nil {
			return didntRun(err.Error())
		}
		// What dt can't do yet decides before the needs, as in Blocked.
		if why := fixture.Unsupported(&spec); why != "" {
			return didntRun(why)
		}
	}
	if why, unmet := Blocked(c.Unit, c.Needs, func(n string) error { return needs.Check(o.Root, n) }); why != "" {
		// A machine that met the needs could run the cell, so without --ci
		// it's skipped; what dt can't do yet is never a skip.
		if len(unmet) > 0 && !o.CI {
			return report.Cell{Name: c.Name, Result: Skipped, Note: why}
		}
		return didntRun(why)
	}
	out := filepath.Join(dir, report.Slug(c.Name))
	vals := map[string]string{"base": base(), "out": out, "test": ""}
	for k, v := range c.Matrix {
		vals[k] = v
	}
	lists := map[string][]string{"args": strings.Fields(c.Unit.Args[c.Tier])}
	if c.Unit.Packages != nil {
		pkgs, err := goPackages(o.Root, c.Unit.Packages.Tag)
		if err != nil {
			return didntRun(err.Error())
		}
		lists["packages"] = pkgs
	}
	if asRoot(c) {
		vals["bin"] = filepath.Join(dir, "bin") // goJSON builds it before the commands run
	}
	if len(c.Unit.Tools()) > 0 {
		vals["tools"] = filepath.Join(dir, "tools") // laid out below, before the tests are found
	}
	if c.Unit.Fixture != nil {
		vals["bin"] = filepath.Join(dir, "bin")
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
		return report.Cell{Name: c.Name, Result: NotRun, Note: why, Log: logName}
	}

	cellCtx, cancel := context.WithTimeout(ctx, time.Duration(c.TimeoutMinutes)*minute)
	defer cancel()
	if vals["tools"] != "" {
		if err := layOutTools(cellCtx, o.Root, vals["tools"], c.Unit.Tools(), w); err != nil {
			return didntRun("the test tools: " + err.Error())
		}
	}
	tests, limits, err := plan.ListTests(o.Root, c.Unit, "")
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
	each := make([]time.Duration, len(tests))
	for i, test := range tests {
		if each[i], err = plan.TestLimit(c.Unit.Timeout, test, limits); err != nil {
			return didntRun(err.Error())
		}
	}
	if c.Unit.Fixture != nil {
		server, why := setUpFixture(cellCtx, spec, o.Root, vals["bin"], out, w)
		if why != "" {
			return didntRun(why)
		}
		defer func() {
			// The code under test crashing fails its cell, unless an
			// interrupt stopped the cell first.
			if err := server.Stop(); err != nil && ctx.Err() == nil {
				_, _ = fmt.Fprintf(w, "[dt] fixture: %v\n", err)
				res.Result, res.Note = Failed, strings.TrimSuffix("the fixture: "+err.Error()+"; "+res.Note, "; ")
			}
		}()
		maps.Copy(vals, server.Placeholders())
	}
	own, err := suiteEnv(c.Unit, c.Tier, vals, lists)
	if err != nil {
		return didntRun(err.Error())
	}
	workdir := o.Root
	if c.Unit.Dir != "" {
		d, err := replace(c.Unit.Dir, vals, lists, nil)
		if err != nil {
			return didntRun(err.Error())
		}
		workdir = d
		if !filepath.IsAbs(d) {
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
		if err := pull(cellCtx, c.Unit.Image, workdir, os.Environ(), w); err != nil {
			return didntRun("the image: " + err.Error())
		}
	}
	t := tester{cell: c, ctx: cellCtx, root: o.Root, dir: workdir, env: append(os.Environ(), own...), own: own, vals: vals, lists: lists, paths: paths, out: w, log: log}
	if c.Unit.Format == "go-json" {
		result, note, tests := t.goJSON(c.Tests)
		return report.Cell{Name: c.Name, Result: result, Note: note, Log: logName, Tests: tests}
	}
	read := readers[c.Unit.Format]
	var failed, notRun []string
	var cases []report.Test
	unstarted, interrupted := 0, false
	for i, test := range tests {
		if cellCtx.Err() != nil {
			unstarted++
			continue
		}
		prefix := ""
		if test != "" {
			prefix = test + ": "
		}
		var output bytes.Buffer
		if read != nil {
			t.capture = &output
		}
		start := time.Now()
		r := t.run(test, each[i])
		interrupted = interrupted || r.interrupted
		switch {
		case read == nil:
			// exit: each test is a test case, or without tests each command.
			switch r.result {
			case Failed:
				failed = append(failed, prefix+r.note)
			case NotRun:
				notRun = append(notRun, prefix+r.note)
			}
			if test == "" {
				cases = append(cases, r.steps...)
			} else if r.result != NotRun {
				status := report.Passed
				if r.result == Failed {
					status = report.Failed
				}
				cases = append(cases, report.Test{Name: test, Status: status, Duration: time.Since(start), Message: r.note})
			}
		case r.interrupted:
			notRun = append(notRun, prefix+"interrupted")
		default:
			got, err := read(report.Ran{Test: test, Output: output.Bytes(), Exit: r.exit, TimedOut: r.timedOut, Out: out})
			if err != nil {
				notRun = append(notRun, prefix+err.Error())
			}
			if r.timedOut {
				// A test that timed out failed, whatever its results say:
				// the reader can only say which of its tests was open.
				failed = append(failed, prefix+r.note)
			}
			if r.result == NotRun {
				notRun = append(notRun, prefix+r.note)
			}
			cases = append(cases, got...)
		}
	}
	if unstarted > 0 {
		notRun = append(notRun, fmt.Sprintf("%d tests not started: %s", unstarted, stopped(cellCtx)))
		interrupted = interrupted || errors.Is(cellCtx.Err(), context.Canceled)
	}
	if read != nil {
		// The results decide, not the exit status: a failed result fails
		// the cell, and a cell with none didn't run.
		var names []string
		for _, tc := range cases {
			if tc.Status == report.Failed {
				names = append(names, tc.Name)
			}
		}
		if len(names) > 0 {
			failed = append(failed, fmt.Sprintf("%d failed: %s", len(names), some(names)))
		}
		if len(cases) == 0 {
			notRun = append(notRun, "no results")
		}
	}
	if interrupted {
		// The cell didn't finish, and what its tests reported once
		// interrupted can come from the interrupt itself.
		cases = nil
	}
	cell := report.Cell{Name: c.Name, Result: Passed, Log: logName, Tests: cases}
	switch {
	case len(failed) > 0:
		cell.Result, cell.Note = Failed, strings.Join(append(failed, notRun...), "; ")
	case len(notRun) > 0:
		cell.Result, cell.Note = NotRun, strings.Join(notRun, "; ")
	}
	return cell
}

// readers read the formats whose results a tool writes: every format but
// exit and go-json, which dt reads itself.
var readers = map[string]report.Reader{}

// exitNotRun is the exit status of a command that ran nothing, as the
// scenarios' setup gives when another run holds their lock.
const exitNotRun = 75

// tester runs one cell's tests.
type tester struct {
	cell     Cell
	ctx      context.Context
	root     string // the repository
	dir      string
	env      []string // dt's environment and the suite's
	own      []string // the suite's alone
	vals     map[string]string
	lists    map[string][]string
	paths    map[string]string // the file each test runs as {test}, when it isn't its name
	out, log io.Writer
	capture  io.Writer // also gets the commands' output, for a reader
}

// testRun is how one test's commands went.
type testRun struct {
	result, note string        // how the test went, and why
	steps        []report.Test // each command that ran to a result, named by its first line
	exit         int           // the first non-zero exit status
	timedOut     bool          // stopped at the test's limit or the cell's
	interrupted  bool          // stopped by an interrupt
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
	var r testRun
	var failed, notRun []string
	image := t.cell.Unit.Image
	cmds := t.cell.Unit.Cmd
	if len(cmds) == 0 {
		cmds = registry.Commands{{}} // the image's own command
	}
	for _, cmd := range cmds {
		step := report.Test{Name: cmp.Or(cmdName(cmd), image), Status: report.Failed}
		if t.vals["base"] == "" && uses(cmd, "base") {
			_, _ = fmt.Fprintf(t.out, "[dt] skipped, no change to check: %s%s\n", cmd.Line, cmd.Sh)
			step.Status, step.Message = report.Skipped, "no change to check"
			r.steps = append(r.steps, step)
			continue
		}
		var argv []string
		var err error
		if cmd != (registry.Command{}) {
			if argv, err = fill(cmd, t.vals, t.lists); err != nil {
				return testRun{result: NotRun, note: err.Error()}
			}
		}
		shown := cmp.Or(strings.Join(argv, " "), image)
		if cmd.Sh != "" {
			shown = argv[2]
		}
		printed, container := shown, ""
		if image != "" {
			if argv, container, err = t.inImage(argv); err != nil {
				return testRun{result: NotRun, note: err.Error()}
			}
			printed = strings.Join(argv, " ")
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
		code, err := execute(ctx, argv, t.dir, t.env, w)
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
			step.Message = fmt.Sprintf("timed out after %s", limit)
		case errors.Is(t.ctx.Err(), context.DeadlineExceeded):
			step.Message = "stopped at the cell's limit"
		case t.ctx.Err() != nil:
			r.result, r.note, r.interrupted = NotRun, "interrupted", true
			return r
		case err != nil:
			step.Message = fmt.Sprintf("`%s`: %v", firstLine(shown), err)
			failed = append(failed, step.Message)
		case code == exitNotRun:
			notRun = append(notRun, fmt.Sprintf("`%s` exited %d: it ran nothing", firstLine(shown), code))
			continue
		case image != "" && notStarted(code):
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
			r.result, r.note, r.timedOut = Failed, step.Message, true
			return r
		}
	}
	switch {
	case len(failed) > 0:
		r.result, r.note = Failed, strings.Join(append(failed, notRun...), ", ")
	case len(notRun) > 0:
		r.result, r.note = NotRun, strings.Join(notRun, ", ")
	default:
		r.result = Passed
	}
	return r
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
// it exits.
func execute(ctx context.Context, argv []string, dir string, env []string, w io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = dir, env
	// The command writes to a pipe of dt's own, not one exec makes, so Wait
	// returns when the command exits, whatever it left holding the output.
	r, pw, err := os.Pipe()
	if err != nil {
		return -1, err
	}
	cmd.Stdout, cmd.Stderr = pw, pw
	killTree(cmd)
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

// Blocked says why dt can't run a cell of suite u that needs list on this
// machine, or returns "" when it can. What dt doesn't do yet comes first, and
// then each need that check finds unmet, as "need: reason"; unmet names those
// needs, and is empty when what stops the cell is something dt doesn't do
// yet. dt run and dt list both decide through it, so they can't disagree.
func Blocked(u *registry.Suite, list []string, check func(need string) error) (why string, unmet []string) {
	if why := unsupported(u); why != "" {
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

// unsupported says why dt can't run the suite yet, or returns "".
func unsupported(u *registry.Suite) string {
	if u.Fixture != nil {
		if why := fixture.Unsupported(u.Fixture); why != "" {
			return why
		}
	}
	switch {
	case u.Format != "exit" && u.Format != "go-json" && readers[u.Format] == nil:
		return fmt.Sprintf("dt doesn't read %s results yet", u.Format)
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

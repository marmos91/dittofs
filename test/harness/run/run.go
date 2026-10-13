// Package run runs the cells of a dt run: each test's commands under the
// test's and the cell's limits, with their output passed through and logged
// (docs/internals/rfc-test-harness.md, sections 4.4 and 4.6).
package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/marmos91/dittofs/test/harness/needs"
	"github.com/marmos91/dittofs/test/harness/plan"
	"github.com/marmos91/dittofs/test/harness/registry"
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

// Status is the run's exit status (section 4.4): 1 when a cell failed, else
// 2 when one didn't run or nothing ran, else 0.
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
}

// minute is a variable so the tests can shorten a cell's limit.
var minute = time.Minute

// Run runs the cells one after another, each logged under a new directory in
// o.Results. Cancelling ctx stops the cell running and the ones after it.
func Run(ctx context.Context, cells []Cell, o Options) (Report, error) {
	commit := gitShort(o.Root)
	dir, err := newRunDir(o.Results, time.Now().UTC().Format("20060102T150405Z")+"-"+commit)
	if err != nil {
		return Report{}, err
	}
	rep := Report{Dir: dir}
	if len(cells) == 0 {
		_, _ = fmt.Fprintln(o.Out, "[dt] nothing to run")
	}
	for _, c := range cells {
		start := time.Now()
		result, note := runCell(ctx, c, o, rep.Dir, commit)
		rep.Outcomes = append(rep.Outcomes, Outcome{c.Name, result, note, time.Since(start).Round(time.Second)})
		_, _ = fmt.Fprintf(o.Out, "[dt] %s: %s %s\n", c.Name, result, note)
		if o.CI && (result == Failed || result == NotRun) {
			_, _ = fmt.Fprintf(o.Out, "::error title=dt %s::%s: %s\n", c.Name, result, note)
		}
	}
	return rep, nil
}

// runCell runs one cell and returns its result and a note on it.
func runCell(ctx context.Context, c Cell, o Options, dir, commit string) (string, string) {
	if ctx.Err() != nil {
		return NotRun, "interrupted"
	}
	if why := unsupported(c.Unit); why != "" {
		return NotRun, why
	}
	if unmet := needs.Unmet(c.Needs); len(unmet) > 0 {
		if o.CI {
			return NotRun, strings.Join(unmet, "; ")
		}
		return Skipped, strings.Join(unmet, "; ")
	}
	tests, limits, err := plan.ListTests(o.Root, c.Unit, "")
	if err != nil {
		return NotRun, err.Error()
	}
	if len(c.Tests) > 0 {
		tests = c.Tests
	}
	if !c.Unit.Tests.Set() {
		tests = []string{""} // the commands are the one test
	}
	if len(tests) == 0 {
		return NotRun, "no tests found"
	}
	each := make([]time.Duration, len(tests))
	for i, test := range tests {
		if each[i], err = plan.TestLimit(c.Unit.Timeout, test, limits); err != nil {
			return NotRun, err.Error()
		}
	}

	slug := strings.ReplaceAll(c.Name, "/", "-")
	out := filepath.Join(dir, slug)
	vals := map[string]string{"base": base(), "out": out, "test": ""}
	for k, v := range c.Matrix {
		vals[k] = v
	}
	args := strings.Fields(c.Unit.Args[c.Tier])
	// A command dt can't fill can't run, whichever test it's for.
	for _, cmd := range c.Unit.Cmd {
		if _, err := fill(cmd, vals, args); err != nil {
			return NotRun, err.Error()
		}
	}
	env, err := environ(c.Unit, c.Tier, vals, args)
	if err != nil {
		return NotRun, err.Error()
	}
	workdir := o.Root
	if c.Unit.Dir != "" {
		d, err := replace(c.Unit.Dir, vals, args, nil)
		if err != nil {
			return NotRun, err.Error()
		}
		workdir = filepath.Join(o.Root, d)
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return NotRun, err.Error()
	}
	log, err := os.Create(filepath.Join(dir, "logs", slug+".log"))
	if err != nil {
		return NotRun, err.Error()
	}
	defer func() { _ = log.Close() }()
	_, _ = fmt.Fprintf(log, "[dt] cell %s, tier %s, commit %s\n", c.Name, c.Tier, commit)

	cellCtx, cancel := context.WithTimeout(ctx, time.Duration(c.TimeoutMinutes)*minute)
	defer cancel()
	t := tester{cell: c, ctx: cellCtx, dir: workdir, env: env, vals: vals, args: args, out: io.MultiWriter(o.Out, log), log: log}
	var failed, notRun []string
	unstarted := 0
	for i, test := range tests {
		if cellCtx.Err() != nil {
			unstarted++
			continue
		}
		switch result, note := t.run(test, each[i]); result {
		case Failed:
			failed = append(failed, note)
		case NotRun:
			notRun = append(notRun, note)
		}
	}
	if unstarted > 0 {
		notRun = append(notRun, fmt.Sprintf("%d tests not started: %s", unstarted, stopped(cellCtx)))
	}
	switch {
	case len(failed) > 0:
		return Failed, strings.Join(append(failed, notRun...), "; ")
	case len(notRun) > 0:
		return NotRun, strings.Join(notRun, "; ")
	}
	return Passed, ""
}

// exitNotRun is the exit status of a command that ran nothing, as the
// scenarios' setup does when another run holds their lock (section 4.6).
const exitNotRun = 75

// tester runs one cell's tests.
type tester struct {
	cell     Cell
	ctx      context.Context
	dir      string
	env      []string
	vals     map[string]string
	args     []string
	out, log io.Writer
}

// run runs every command for one test, all of them even after one fails, and
// says how the test went: failed when a command failed or the test ran out of
// time, not run when a command ran nothing or the run was interrupted, else
// passed. A limit of zero is no limit but the cell's.
func (t tester) run(test string, limit time.Duration) (string, string) {
	ctx := t.ctx
	if limit > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, limit)
		defer cancel()
	}
	t.vals["test"] = test
	prefix := ""
	if test != "" {
		prefix = test + ": "
	}
	var failed, notRun []string
	for _, cmd := range t.cell.Unit.Cmd {
		if t.vals["base"] == "" && uses(cmd, "base") {
			_, _ = fmt.Fprintf(t.out, "[dt] skipped, no change to check: %s%s\n", cmd.Line, cmd.Sh)
			continue
		}
		argv, err := fill(cmd, t.vals, t.args)
		if err != nil {
			return NotRun, prefix + err.Error()
		}
		shown := strings.Join(argv, " ")
		if cmd.Sh != "" {
			shown = argv[2]
		}
		_, _ = fmt.Fprintf(t.out, "[dt] $ %s\n", shown)
		start := time.Now()
		code, err := execute(ctx, argv, t.dir, t.env, t.out)
		_, _ = fmt.Fprintf(t.log, "[dt] exit %d after %s\n", code, time.Since(start).Round(time.Millisecond))
		switch {
		case ctx.Err() != nil && t.ctx.Err() == nil:
			return Failed, fmt.Sprintf("%stimed out after %s", prefix, limit)
		case errors.Is(t.ctx.Err(), context.DeadlineExceeded):
			return Failed, prefix + "stopped at the cell's limit"
		case t.ctx.Err() != nil:
			return NotRun, prefix + "interrupted"
		case err != nil:
			failed = append(failed, fmt.Sprintf("`%s`: %v", firstLine(shown), err))
		case code == exitNotRun:
			notRun = append(notRun, fmt.Sprintf("`%s` exited %d: it ran nothing", firstLine(shown), code))
		case code != 0:
			failed = append(failed, fmt.Sprintf("`%s` exited %d", firstLine(shown), code))
		}
	}
	switch {
	case len(failed) > 0:
		return Failed, prefix + strings.Join(append(failed, notRun...), ", ")
	case len(notRun) > 0:
		return NotRun, prefix + strings.Join(notRun, ", ")
	}
	return Passed, ""
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
// status. A command that outlives ctx is killed with everything it started.
func execute(ctx context.Context, argv []string, dir string, env []string, w io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, env, w, w
	// A leftover child holding the output open can't keep dt waiting.
	cmd.WaitDelay = 5 * time.Second
	killTree(cmd)
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

// unsupported says why dt can't run the suite yet, or returns "".
func unsupported(u *registry.Suite) string {
	switch {
	case u.Fixture != nil:
		return "dt doesn't set up fixtures yet"
	case u.Image != "":
		return "dt doesn't run commands in images yet"
	case u.Format != "exit":
		return fmt.Sprintf("dt doesn't read %s results yet", u.Format)
	}
	return ""
}

// base is the ref a change is compared against (section 4.1): the base branch
// of the pull request being checked, the commit before the push being
// checked, or origin/develop when dt checks no event. It's "" when the event
// has no change to check: a schedule, a dispatch, or a push without a
// previous commit.
func base() string {
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		return "origin/develop"
	}
	switch os.Getenv("GITHUB_EVENT_NAME") {
	case "pull_request":
		return "origin/" + os.Getenv("GITHUB_BASE_REF")
	case "push":
		var event struct {
			Before string `json:"before"`
		}
		data, err := os.ReadFile(os.Getenv("GITHUB_EVENT_PATH"))
		if err == nil && json.Unmarshal(data, &event) == nil && strings.Trim(event.Before, "0") != "" {
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

// gitShort is the checkout's commit, for the run's name and its logs.
func gitShort(root string) string {
	out, err := exec.Command("git", "-C", root, "rev-parse", "--short=8", "HEAD").Output()
	if err != nil {
		return "nocommit"
	}
	return strings.TrimSpace(string(out))
}

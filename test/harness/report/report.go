// Package report holds a dt run's results, cell by cell and test by test, and
// writes them as the run's summary.json, in the Common Test Report Format
// (CTRF) at one pinned version, with each cell's as a JUnit file and the run's
// report.md, both generated from the same results. It reads summary.json back,
// shows a run as dt report does, and writes a cell's results as smbtorture's
// baseline file.
package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// specVersion is the one CTRF version dt writes, whatever CTRF publishes later.
const specVersion = "0.1.0"

// A test's status, in CTRF's words. A cell's result is passed, failed,
// skipped, for needs this machine doesn't meet, or NotRun.
const (
	Passed  = "passed"
	Failed  = "failed"
	Skipped = "skipped"
	Other   = "other"   // the test has no result: its output stopped before one
	NotRun  = "not run" // the cell couldn't run, or its limit left tests unstarted
)

// Grades dt gives a result. A passing result no entry stands against has
// none. Grading sets a result's status too, so that CTRF and JUnit count as
// failed only what fails its cell, and as other, an error in JUnit, only
// what leaves it not run.
const (
	// New is a failure no known entry matches, or a crash or a hang, which
	// no entry excuses: failed.
	New = "new"
	// Known is a failure a known-failure entry matches: skipped.
	Known = "known"
	// Quarantined is a result an entry of the quarantine list matches:
	// skipped when it failed, and passed when it passed.
	Quarantined = "quarantined"
	// UnexpectedPass is a pass an entry still lists: failed.
	UnexpectedPass = "unexpected_pass"
	// ExpectedSkip is a skip an expected-skip entry matches: skipped.
	ExpectedSkip = "expected_skip"
	// UnexpectedSkip is a skip no expected-skip entry matches, in a suite
	// with a known file: other.
	UnexpectedSkip = "unexpected_skip"
	// Ungraded is a skip in a suite without a known file: it's reported and
	// counted, and passes.
	Ungraded = "ungraded"
	// Incomplete is a result whose output stopped before it: other.
	Incomplete = "incomplete"
)

// Test is one result of one test. A test with several results in a cell has
// one Test for each.
type Test struct {
	Name      string
	Suite     []string // where the test sits within its cell, such as a Go test's package
	Status    string   // passed, failed, skipped or other, once graded
	RawStatus string   // the tool's status, when grading changed it
	Duration  time.Duration
	Message   string // what the tool said about the result
	Grade     string // how dt graded the result, when it did
	Reason    string // why the result was expected: the reason of the known entry that graded it
	Issue     string // the issue that entry names
	Retries   int    // how many times a retry ran it again
	Flaky     bool   // it failed, then passed on a retry
}

// Cell is how one cell went, and its tests' results.
type Cell struct {
	Name   string // its suite, then its option and matrix values, joined with /
	Result string // passed, failed, not run, or skipped for needs this machine doesn't meet
	Note   string
	Took   time.Duration
	Log    string // the cell's log, relative to the run's directory; "" when it has none
	Tests  []Test
}

// Run is a whole run, as far as it got.
type Run struct {
	Name        string // the run's directory
	Start, Stop time.Time
	Commit      string
	Branch      string // "" when the checkout isn't on a branch
	Dirty       bool   // the checkout has changes its commit doesn't
	OS          string
	Kernel      string // the release of the kernel it ran on, as uname -r gives it; "" where there is none
	In          string // host, or container when dt runs in its dev container
	Tier        string // the tier the run was asked for
	Exit        int    // the run's exit status, from the cells it finished
	Cells       []Cell
}

// Slug is a cell's name as one path element, which names the cell's
// directory, its log and its JUnit file.
func Slug(cell string) string {
	return strings.ReplaceAll(cell, "/", "-")
}

// Ran is what one test's commands leave for a reader. Test is "" when the
// suite lists no tests, so the commands ran once for the whole cell.
type Ran struct {
	Test     string
	Prefix   string        // the test's prefix= in a .txt list: what the names the tool reports leave off
	Output   []byte        // what the commands printed
	Exit     int           // the first non-zero exit status among them, else 0
	TimedOut bool          // they were stopped at the test's limit or the cell's
	Took     time.Duration // how long they ran
	Out      string        // the cell's directory, {out}, where tools write their result files
}

// A Reader reads one format's results from what a test's commands left. The
// results decide how the cell went, not the commands' exit status, which a
// reader may still use; a test that timed out fails the cell whatever they
// say.
type Reader func(Ran) ([]Test, error)

// Write writes each cell's JUnit file under dir/junit, then dir/report.md,
// which is what dt report --format md shows, then the run to
// dir/summary.json, last, so the summary never names a JUnit file that isn't
// there. Each file is replaced whole, by a rename, so a run stopped at any
// moment leaves its last complete summary. Like the logs, anyone can read
// them, so a run as root leaves files its user can read.
//
// ponytail: every cell's JUnit file is rewritten with the summary, though only
// the cell just added changed: about 15 ms more for the unit suite's 12,500
// results. Write only the new cells' files when a run's time outside its
// cells passes 10 seconds.
func (r Run) Write(dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, "junit"), 0o755); err != nil {
		return err
	}
	for _, c := range r.Cells {
		x, err := c.junit()
		if err != nil {
			return err
		}
		if err := replace(filepath.Join(dir, junitPath(c.Name)), x); err != nil {
			return err
		}
	}
	var md bytes.Buffer
	if err := r.Show(&md, "md", false); err != nil {
		return err
	}
	if err := replace(filepath.Join(dir, "report.md"), md.Bytes()); err != nil {
		return err
	}
	data, err := r.json()
	if err != nil {
		return err
	}
	return replace(filepath.Join(dir, "summary.json"), data)
}

// Read reads the run in dir back from its summary.json, as Write wrote it.
func Read(dir string) (Run, error) {
	path := filepath.Join(dir, "summary.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return Run{}, err
	}
	return Parse(path, data)
}

// Parse reads a run from data, a summary.json as Write writes it, named path
// in its errors. A test goes back to the cell whose name begins its suite, the
// longest such name when one cell's name begins another's.
func Parse(path string, data []byte) (Run, error) {
	var doc document
	if err := json.Unmarshal(data, &doc); err != nil {
		return Run{}, fmt.Errorf("%s: %w", path, err)
	}
	res := doc.Results
	r := Run{Name: res.Extra.Run, Start: time.UnixMilli(res.Summary.Start), Stop: time.UnixMilli(res.Summary.Stop),
		Commit: res.Environment.Commit, Branch: res.Environment.BranchName, Dirty: res.Extra.Dirty,
		OS: res.Environment.OSPlatform, Kernel: res.Environment.OSRelease, In: res.Extra.In, Tier: res.Extra.Tier, Exit: res.Extra.Exit}
	var at [][]string
	for _, c := range res.Extra.Cells {
		name := c.Suite
		if c.Cell != "" {
			name += "/" + c.Cell
		}
		r.Cells = append(r.Cells, Cell{Name: name, Result: c.Result, Note: c.Note, Took: time.Duration(c.Seconds) * time.Second, Log: c.Log})
		at = append(at, strings.Split(name, "/"))
	}
	for _, t := range res.Tests {
		in := -1
		for i, a := range at {
			if len(a) <= len(t.Suite) && slices.Equal(a, t.Suite[:len(a)]) && (in < 0 || len(a) > len(at[in])) {
				in = i
			}
		}
		if in < 0 {
			return Run{}, fmt.Errorf("%s: the test %q is in no cell", path, t.Name)
		}
		out := Test{Name: t.Name, Status: t.Status, RawStatus: t.RawStatus, Duration: time.Duration(t.Duration) * time.Millisecond, Message: t.Message}
		if own := t.Suite[len(at[in]):]; len(own) > 0 {
			out.Suite = own
		}
		if t.Extra != nil {
			out.Grade, out.Reason, out.Issue = t.Extra.Grade, t.Extra.Reason, t.Extra.Issue
		}
		out.Retries, out.Flaky = t.Retries, t.Flaky
		r.Cells[in].Tests = append(r.Cells[in].Tests, out)
	}
	return r, nil
}

// replace writes data to path through a temporary file beside it, renamed
// over path, so a reader never sees part of it.
func replace(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(0o644)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
	return err
}

// json is the run as a CTRF document: each test's suite is its cell's name,
// split at /, then its own suite, and dt's own fields go in extra.
func (r Run) json() ([]byte, error) {
	doc := document{ReportFormat: "CTRF", SpecVersion: specVersion, GeneratedBy: "dt"}
	res := &doc.Results
	res.Tool.Name = "dt"
	res.Tests = []test{}
	res.Environment = environment{Commit: r.Commit, BranchName: r.Branch, OSPlatform: r.OS, OSRelease: r.Kernel}
	res.Extra = extra{Run: r.Name, Dirty: r.Dirty, Tier: r.Tier, In: r.In, Exit: r.Exit, Cells: []cell{}}
	sum := &res.Summary
	sum.Start, sum.Stop = r.Start.UnixMilli(), r.Stop.UnixMilli()
	for _, c := range r.Cells {
		suite, name, _ := strings.Cut(c.Name, "/")
		res.Extra.Cells = append(res.Extra.Cells, cell{suite, name, c.Result, c.Note, int64(c.Took.Round(time.Second) / time.Second), c.Log, junitPath(c.Name)})
		at := strings.Split(c.Name, "/")
		for _, t := range c.Tests {
			out := test{Name: t.Name, Suite: slices.Concat(at, t.Suite), Status: t.Status, RawStatus: t.RawStatus, Duration: t.Duration.Milliseconds(), Message: t.Message, Retries: t.Retries, Flaky: t.Flaky}
			if x := (testExtra{Grade: t.Grade, Reason: t.Reason, Issue: t.Issue}); x != (testExtra{}) {
				out.Extra = &x
			}
			res.Tests = append(res.Tests, out)
			switch t.Status {
			case Passed:
				sum.Passed++
			case Failed:
				sum.Failed++
			case Skipped:
				sum.Skipped++
			default:
				sum.Other++
			}
			if t.Flaky {
				sum.Flaky++
			}
		}
	}
	sum.Tests = len(res.Tests)
	data, err := json.MarshalIndent(doc, "", "  ")
	return append(data, '\n'), err
}

// document is CTRF's, with the fields dt fills.
type document struct {
	ReportFormat string  `json:"reportFormat"`
	SpecVersion  string  `json:"specVersion"`
	GeneratedBy  string  `json:"generatedBy"`
	Results      results `json:"results"`
}

type results struct {
	Tool        tool        `json:"tool"`
	Summary     summary     `json:"summary"`
	Tests       []test      `json:"tests"`
	Environment environment `json:"environment"`
	Extra       extra       `json:"extra"`
}

type tool struct {
	Name string `json:"name"`
}

type summary struct {
	Tests   int   `json:"tests"`
	Passed  int   `json:"passed"`
	Failed  int   `json:"failed"`
	Skipped int   `json:"skipped"`
	Pending int   `json:"pending"`
	Other   int   `json:"other"`
	Flaky   int   `json:"flaky"`
	Start   int64 `json:"start"`
	Stop    int64 `json:"stop"`
}

type test struct {
	Name      string     `json:"name"`
	Suite     []string   `json:"suite"`
	Status    string     `json:"status"`
	RawStatus string     `json:"rawStatus,omitempty"`
	Duration  int64      `json:"duration"`
	Message   string     `json:"message,omitempty"`
	Retries   int        `json:"retries,omitempty"`
	Flaky     bool       `json:"flaky,omitempty"`
	Extra     *testExtra `json:"extra,omitempty"`
}

type testExtra struct {
	Grade  string `json:"grade,omitempty"`
	Reason string `json:"reason,omitempty"`
	Issue  string `json:"issue,omitempty"`
}

type environment struct {
	Commit     string `json:"commit"`
	BranchName string `json:"branchName,omitempty"`
	OSPlatform string `json:"osPlatform"`
	OSRelease  string `json:"osRelease,omitempty"`
}

// extra is the run's own fields: its name, its checkout, where and at which
// tier it ran, its exit status, and each cell.
type extra struct {
	Run   string `json:"run"`
	Dirty bool   `json:"dirty"`
	Tier  string `json:"tier"`
	In    string `json:"in"`
	Exit  int    `json:"exit"`
	Cells []cell `json:"cells"`
}

type cell struct {
	Suite   string `json:"suite"`
	Cell    string `json:"cell,omitempty"`
	Result  string `json:"result"`
	Note    string `json:"note,omitempty"`
	Seconds int64  `json:"seconds"`
	Log     string `json:"log,omitempty"`
	JUnit   string `json:"junit"`
}

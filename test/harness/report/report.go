// Package report holds a dt run's results, cell by cell and test by test, and
// writes them as the run's summary.json, in the Common Test Report Format
// (CTRF) at one pinned version.
package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// SpecVersion is the one CTRF version dt writes, whatever CTRF publishes later.
const SpecVersion = "0.1.0"

// A test's status, in CTRF's words.
const (
	Passed  = "passed"
	Failed  = "failed"
	Skipped = "skipped"
	Other   = "other" // the test has no result: its output stopped before one
)

// Ungraded is the grade of a skip in a suite without a known file: it's
// reported and counted, and passes.
const Ungraded = "ungraded"

// Test is one result of one test. A test with several results in a cell has
// one Test for each.
type Test struct {
	Name     string
	Suite    []string // where the test sits within its cell, such as a Go test's package
	Status   string   // passed, failed, skipped or other
	Duration time.Duration
	Message  string // what the tool said about the result
	Grade    string // how dt graded the result, when it did
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
	In          string // host, or container when dt runs in its dev container
	Tier        string // the tier the run was asked for
	Exit        int    // the run's exit status, from the cells it finished
	Cells       []Cell
}

// Ran is what one test's commands leave for a reader. Test is "" when the
// suite lists no tests, so the commands ran once for the whole cell.
type Ran struct {
	Test     string
	Output   []byte // what the commands printed
	Exit     int    // the first non-zero exit status among them, else 0
	TimedOut bool   // they were stopped at the test's limit or the cell's
	Out      string // the cell's directory, {out}, where tools write their result files
}

// A Reader reads one format's results from what a test's commands left. The
// results decide how the cell went, not the commands' exit status, which a
// reader may still use.
type Reader func(Ran) ([]Test, error)

// Write writes the run to dir/summary.json. The file is replaced whole, by a
// rename, so a run stopped at any moment leaves its last complete summary.
// Like the logs, anyone can read it, so a run as root leaves one its user can
// read.
func (r Run) Write(dir string) error {
	data, err := r.JSON()
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".summary-*.json")
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
		err = os.Rename(f.Name(), filepath.Join(dir, "summary.json"))
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
	return err
}

// JSON is the run as a CTRF document: each test's suite is its cell's name,
// split at /, then its own suite, and dt's own fields go in extra.
func (r Run) JSON() ([]byte, error) {
	doc := document{ReportFormat: "CTRF", SpecVersion: SpecVersion, GeneratedBy: "dt"}
	res := &doc.Results
	res.Tool.Name = "dt"
	res.Tests = []test{}
	res.Environment = environment{Commit: r.Commit, BranchName: r.Branch, OSPlatform: r.OS}
	res.Extra = extra{Run: r.Name, Dirty: r.Dirty, Tier: r.Tier, In: r.In, Exit: r.Exit, Cells: []cell{}}
	sum := &res.Summary
	sum.Start, sum.Stop = r.Start.UnixMilli(), r.Stop.UnixMilli()
	for _, c := range r.Cells {
		suite, name, _ := strings.Cut(c.Name, "/")
		res.Extra.Cells = append(res.Extra.Cells, cell{suite, name, c.Result, c.Note, int64(c.Took.Round(time.Second) / time.Second), c.Log})
		at := strings.Split(c.Name, "/")
		for _, t := range c.Tests {
			out := test{Name: t.Name, Suite: slices.Concat(at, t.Suite), Status: t.Status, Duration: t.Duration.Milliseconds(), Message: t.Message}
			if t.Grade != "" {
				out.Extra = &testExtra{Grade: t.Grade}
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
	Name     string     `json:"name"`
	Suite    []string   `json:"suite"`
	Status   string     `json:"status"`
	Duration int64      `json:"duration"`
	Message  string     `json:"message,omitempty"`
	Extra    *testExtra `json:"extra,omitempty"`
}

type testExtra struct {
	Grade string `json:"grade"`
}

type environment struct {
	Commit     string `json:"commit"`
	BranchName string `json:"branchName,omitempty"`
	OSPlatform string `json:"osPlatform"`
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
}

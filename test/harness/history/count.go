package history

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"github.com/marmos91/dittofs/test/harness/report"
)

// Outcome is what the tool reported for a result, before dt graded it:
// passed, failed or skipped, or other when it has none. A known failure
// failed and an unexpected pass passed, whatever their status; a skip keeps
// any status its tool gave it, such as a test that didn't execute.
//
// decision: history counts what the tests did, not their grades, so a
// known, a quarantined or a flaky test shows how often it really fails.
func Outcome(t report.Test) string {
	switch raw := cmp.Or(t.RawStatus, t.Status); raw {
	case report.Passed, report.Failed, report.Other:
		return raw
	}
	return report.Skipped
}

// Count is how a suite or a test went over the runs read: in how many it has
// a result, its results by outcome, of which the passes on a retry are also
// flaky, and their times. A result with no outcome counts in Runs only.
type Count struct {
	Runs                    int
	Pass, Fail, Skip, Flaky int
	Times                   []time.Duration // a suite's cells' or a test's results'
	last                    string          // the last run counted
}

// P50 and P90 are the median and the 90th percentile of the times, by
// nearest rank.
func (c *Count) P50() time.Duration { return percentile(c.Times, 50) }
func (c *Count) P90() time.Duration { return percentile(c.Times, 90) }

func percentile(times []time.Duration, p int) time.Duration {
	if len(times) == 0 {
		return 0
	}
	sorted := slices.Sorted(slices.Values(times))
	return sorted[(len(sorted)*p+99)/100-1]
}

// Suites counts each suite over runs, by its name: its tests' results and its
// cells' times. A cell skipped for needs its machine didn't meet didn't run,
// so it doesn't count.
func Suites(runs []report.Run) map[string]*Count { return cells(runs, Suite) }

// Cells counts each cell over runs, by its name, as Suites counts a suite.
func Cells(runs []report.Run) map[string]*Count {
	return cells(runs, func(c report.Cell) string { return c.Name })
}

// cells counts the cells of runs under the key each gives.
func cells(runs []report.Run, key func(report.Cell) string) map[string]*Count {
	out := map[string]*Count{}
	for _, r := range runs {
		for _, c := range r.Cells {
			if c.Result == report.Skipped {
				continue
			}
			n := at(out, key(c), r.Name)
			n.Times = append(n.Times, c.Took)
			for _, t := range c.Tests {
				n.add(t)
			}
		}
	}
	return out
}

// Tests counts each test of suite over runs, by its title, which names a Go
// test's package too, and with name only the tests called that.
func Tests(runs []report.Run, suite, name string) map[string]*Count {
	out := map[string]*Count{}
	for _, r := range runs {
		for _, c := range r.Cells {
			if Suite(c) != suite {
				continue
			}
			for _, t := range c.Tests {
				if name == "" || t.Name == name {
					n := at(out, t.Title(), r.Name)
					n.Times = append(n.Times, t.Duration)
					n.add(t)
				}
			}
		}
	}
	return out
}

// Suite is the suite a cell is of: its name's first segment.
func Suite(c report.Cell) string {
	suite, _, _ := strings.Cut(c.Name, "/")
	return suite
}

// at is the count of key, counting the run once: a run is the runs that
// follow each other under one name, as a workflow run's jobs do.
func at(counts map[string]*Count, key, run string) *Count {
	n := counts[key]
	if n == nil {
		n = &Count{}
		counts[key] = n
	}
	if n.last != run {
		n.Runs, n.last = n.Runs+1, run
	}
	return n
}

func (c *Count) add(t report.Test) {
	switch Outcome(t) {
	case report.Passed:
		c.Pass++
	case report.Failed:
		c.Fail++
	case report.Skipped:
		c.Skip++
	}
	if t.Flaky {
		c.Flaky++
	}
}

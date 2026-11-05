package journaltest

import (
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/sim"
)

// NewStopwatchClock returns a *sim.TestClock whose wall time starts at the
// Unix origin and whose Elapsed starts at zero, for the tree's internal test
// files: the journal's import rule lets this package reach the shared clock
// and keeps the internal tests away from it, so they take the double from
// here, with :=, and never import pkg/sim themselves.
func NewStopwatchClock() *sim.TestClock {
	return sim.NewTestClock(time.Unix(0, 0))
}

// DrivenClock is the face a test holds of a clock it drives: a core.Clock —
// Now, Elapsed, After — plus the two methods of the test double the checks
// drive it with, Advance to move it and Pending to read how many of its
// After channels are still armed. A test takes the double from
// NewStopwatchClock with := (so no name of pkg/sim reaches an internal
// package) and types the place it keeps it — a rig's field, a helper's
// parameter — with this interface instead.
type DrivenClock interface {
	core.Clock
	Pending() int
	Advance(d time.Duration)
}

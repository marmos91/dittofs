package sim

import (
	"time"

	"github.com/marmos91/dittofs/pkg/clock"
)

// TestClock is the clock a check drives; production never builds one.
type TestClock interface {
	clock.Clock
	Advance(d time.Duration)
	SetRate(r float64) // 1.05 runs it 5 % fast against every other clock
}

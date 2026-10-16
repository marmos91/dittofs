package sim

import "time"

// Clock is the node's one source of time.
type Clock interface {
	Now() time.Time                        // wall time: record stamps, the store's Now on a store without its own
	Since(start Mono) time.Duration        // elapsed on the monotonic clock; every wait is sized on it
	Mono() Mono                            // a monotonic reading, never persisted
	After(d time.Duration) <-chan struct{} // fires after d on the monotonic clock
}

// TestClock is the clock a check drives; production never builds one.
type TestClock interface {
	Clock
	Advance(d time.Duration)
	SetRate(r float64) // 1.05 runs it 5 % fast against every other clock
}

// Mono is a reading of a clock's monotonic time: the time elapsed since an
// origin the clock chose, which means nothing outside the clock that issued
// it. A reading is never persisted and never compared across clocks.
//
// decision: Mono is a duration rather than an opaque type, because every
// clock implementation, test clocks in other packages included, has to build
// one. Revisit if readings start crossing clocks by mistake, which an opaque
// type would refuse to compile.
type Mono time.Duration

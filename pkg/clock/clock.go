package clock

import "time"

// Clock is the node's one source of time: the full set of methods a component
// may read time through.
type Clock interface {
	// Now returns the wall time: record stamps, and the store's time on a
	// store without its own.
	Now() time.Time

	// Elapsed returns the monotonic time elapsed since an origin the clock
	// chose. It is never persisted, and only differences between two readings
	// mean anything; every wait is sized on them.
	Elapsed() time.Duration

	// After returns a channel that is closed once d has elapsed on the
	// monotonic clock.
	After(d time.Duration) <-chan struct{}
}

// System is the clock of the machine the process runs on. Build it with
// NewSystem: a zero System holds no monotonic reading, so its Elapsed would
// follow the wall clock and could move backwards.
type System struct {
	start time.Time
}

// NewSystem returns the system clock, with its monotonic origin at the call.
func NewSystem() System {
	return System{start: time.Now()}
}

// Now returns time.Now().
func (s System) Now() time.Time {
	return time.Now()
}

// Elapsed returns the monotonic time since NewSystem was called.
func (s System) Elapsed() time.Duration {
	return time.Since(s.start)
}

// After returns a channel closed once d has elapsed. No goroutine waits on it:
// the runtime's timer closes it.
func (s System) After(d time.Duration) <-chan struct{} {
	ch := make(chan struct{})
	time.AfterFunc(d, func() { close(ch) })
	return ch
}

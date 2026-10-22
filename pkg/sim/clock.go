// Package sim holds the test doubles every component's checks share: a clock
// a test drives and the fault interface every storage and network seam
// consults. It imports only the standard library, so any component may use it
// in its tests.
package sim

import (
	"math"
	"slices"
	"sync"
	"time"
)

// TestClock is a clock a test drives. Time moves only when Advance is called:
// Now and Elapsed stand still between calls, and a channel from After is
// closed only by the Advance that reaches its deadline, never by real time.
// It satisfies the Clock every component declares (Now, Elapsed, After).
// Its methods are safe for concurrent use.
type TestClock struct {
	mu      sync.Mutex
	rate    float64
	wall    time.Time     // what Now returns
	mono    time.Duration // what Elapsed returns
	pending []timer
}

// timer is one After channel not yet closed.
type timer struct {
	due time.Duration // on the monotonic clock
	ch  chan struct{}
}

// NewTestClock returns a clock whose Now starts at start, whose Elapsed starts
// at zero, and which runs at rate 1.
func NewTestClock(start time.Time) *TestClock {
	return &TestClock{
		wall: start,
		rate: 1,
	}
}

// Now returns the wall time: start plus everything Advance has moved the clock.
func (c *TestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wall
}

// Elapsed returns how far Advance has moved the clock since it was made.
func (c *TestClock) Elapsed() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mono
}

// After returns a channel that is closed once the clock has moved d past its
// current Elapsed. A d of zero or less returns a channel already closed.
func (c *TestClock) After(d time.Duration) <-chan struct{} {
	ch := make(chan struct{})
	if d <= 0 {
		close(ch)
		return ch
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.pending = append(c.pending, timer{due: c.mono + d, ch: ch})
	return ch
}

// Pending returns how many channels from After are not yet closed: the
// waits a test can release with Advance.
func (c *TestClock) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pending)
}

// Advance moves the clock by d scaled by its rate, and closes every After
// channel whose deadline it reaches. A timer armed while Advance runs, by a
// goroutine one of its channels woke, waits for the next Advance. A negative
// d panics.
func (c *TestClock) Advance(d time.Duration) {
	if d < 0 {
		panic("sim: TestClock.Advance with a negative duration")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Rounded, not truncated: 3 s at 0.7 is a hair under 2.1 s in floating
	// point, and truncating would miss a timer due at exactly 2.1 s.
	step := time.Duration(math.Round(float64(d) * c.rate))
	c.mono += step
	c.wall = c.wall.Add(step)

	c.pending = slices.DeleteFunc(c.pending, func(t timer) bool {
		if t.due <= c.mono {
			close(t.ch)
			return true
		}
		return false
	})
}

// SetRate sets how fast the clock runs against the time a test advances it by:
// at 1.05, Advance(100*time.Second) moves it 105 s, as a clock 5% fast
// against every other would. It applies to later calls of Advance only. A
// rate of zero or less panics.
func (c *TestClock) SetRate(r float64) {
	if !(r > 0) {
		panic("sim: TestClock.SetRate with a rate of zero or less")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.rate = r
}

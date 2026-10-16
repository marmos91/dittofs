package backend

import (
	"fmt"
	"sync"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/kv/codec"
)

// CeilingAhead is how far ahead of the clock the ceiling is raised, once per
// CeilingPeriod. Twice the period, so Now reaches the ceiling only when a
// raise has failed or stalled for a whole period.
const (
	CeilingPeriod = time.Second
	CeilingAhead  = 2 * CeilingPeriod
)

// Clock is one node's store time: its clock clamped under a ceiling the
// store holds durably, so a clock stepped back across a restart cannot carry
// Now backward.
type Clock struct {
	clock func() time.Time

	mu      sync.Mutex
	last    time.Time // the latest Now handed out
	ceiling time.Time // Now never passes it
}

// NewClock resumes store time from the stored ceiling, zero when there is
// none. Now holds at it until the first Raise.
func NewClock(clock func() time.Time, stored time.Time) *Clock {
	if clock == nil {
		clock = time.Now
	}
	return &Clock{clock: clock, last: stored, ceiling: stored}
}

// Now is the clock clamped strictly increasing: a nanosecond past the last
// value handed out when the clock has not moved past it, as a coarse clock
// does not between commits within one of its ticks, and never past the
// ceiling. Its bound of real time is CeilingAhead ahead after a restart, and
// behind by however long a ceiling raise stalls.
//
// decision: at the ceiling Now holds rather than pass it, so two
// transactions there share one value. A version drawn from store time then
// repeats; that takes the clock a full CeilingAhead past a stalled raise,
// which stalls the store's commits too.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.clock()
	if !t.After(c.last) {
		t = c.last.Add(time.Nanosecond)
	}
	if t.After(c.ceiling) {
		t = c.ceiling
	}
	c.last = t
	return t
}

// Raise has write store the ceiling CeilingAhead past the clock, as the value
// EncodeCeiling makes, and only once it is durable lets Now use it. The
// caller writes it to a key of the node's own, so the raise conflicts with no
// other transaction's writes.
func (c *Clock) Raise(write func(value []byte) error) error {
	c.mu.Lock()
	next := c.clock().Add(CeilingAhead)
	if next.Before(c.ceiling) {
		next = c.ceiling
	}
	c.mu.Unlock()
	if err := write(EncodeCeiling(next)); err != nil {
		return err
	}
	c.mu.Lock()
	if next.After(c.ceiling) {
		c.ceiling = next
	}
	c.mu.Unlock()
	return nil
}

// Run raises the ceiling every CeilingPeriod until stop closes. A failed
// raise is retried next period; meanwhile Now holds at the old ceiling rather
// than pass it.
func (c *Clock) Run(stop <-chan struct{}, write func(value []byte) error) {
	tick := time.NewTicker(CeilingPeriod)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			_ = c.Raise(write)
		}
	}
}

// EncodeCeiling is a ceiling as stored.
func EncodeCeiling(t time.Time) []byte {
	return codec.NewWriter(1).Uint64(uint64(t.UnixNano())).Encode()
}

// DecodeCeiling reads a stored ceiling.
func DecodeCeiling(b []byte) (time.Time, error) {
	r, _, err := codec.NewReader(b, 1)
	if err != nil {
		return time.Time{}, fmt.Errorf("clock ceiling: %w", err)
	}
	ns := r.Uint64()
	if err := r.Done(); err != nil {
		return time.Time{}, fmt.Errorf("clock ceiling: %w", err)
	}
	return time.Unix(0, int64(ns)), nil
}

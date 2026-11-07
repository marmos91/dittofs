package enginetest

import (
	"sync/atomic"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/engine"
)

// Reservations counts the writes Write accepted whose Done has not run, and
// fails the test on a Done that runs twice.
type Reservations struct {
	t testing.TB
	n atomic.Int64
}

// NewReservations is a counter at zero.
func NewReservations(t testing.TB) *Reservations { return &Reservations{t: t} }

// Opts is the options of one write and the function to call with its
// answer: an accepted write counts until its Done runs.
func (r *Reservations) Opts() (engine.WriteOpts, func(err error)) {
	var calls atomic.Int32
	done := func() {
		if calls.Add(1) > 1 {
			r.t.Errorf("a write's Done ran %d times", calls.Load())
			return
		}
		r.n.Add(-1)
	}
	return engine.WriteOpts{Done: done}, func(err error) {
		if err == nil {
			r.n.Add(1)
		} else if calls.Load() > 0 {
			r.t.Error("a refused write's Done ran")
		}
	}
}

// Outstanding is the accepted writes whose Done has not run.
func (r *Reservations) Outstanding() int64 { return r.n.Load() }

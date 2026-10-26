package journalv2test

import (
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/sim"
)

// Hold is one set of calls stopped at the seam.
type Hold struct {
	seam *Seam
	op   string
	key  string

	reachedOnce sync.Once
	reached     chan struct{}
	releaseOnce sync.Once
	released    chan struct{}
	waiting     int

	mu sync.Mutex
}

// Hold makes every call whose op matches op, on a file whose name matches key,
// block before it acts until the hold is released. Both are patterns, matched
// as a fault rule's are (see sim.Match): "*" holds every op, or every key, the
// empty key of list and sync_dir included, and an exact name only itself. op
// may name any call, not only one of the seam's Op constants, so code that
// reports its own points to the seam can be held the same way. An empty op or
// key panics.
//
// A call is held after it is counted and after a call on a closed file has
// failed, and before the fault is consulted, so a held call has done nothing
// yet. A call that two holds match waits on each, in the order they were
// made. Crash does not release a hold; a call released after a Crash closed
// its file fails with fs.ErrClosed. The hold is released when t ends, if
// nothing released it before, so a test that fails before Release leaves no
// call parked for a later cleanup to wait on.
//
// decision: a hold is test choreography, not a fault. A check that stops the
// code under test at one call and acts while it waits cannot use a Delay,
// because advancing the clock to release the call also fires every timer the
// code under test armed, so a held call waits on no clock: only Release frees
// it.
func (s *Seam) Hold(t testing.TB, op, key string) *Hold {
	if op == "" || key == "" {
		panic(fmt.Sprintf(`journalv2test: Hold(%q, %q): an empty op or key; "*" holds every one`, op, key))
	}

	h := &Hold{
		seam:     s,
		op:       op,
		key:      key,
		reached:  make(chan struct{}),
		released: make(chan struct{}),
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.holds = append(s.holds, h)

	t.Cleanup(h.Release)
	return h
}

// Reached returns a channel closed once the first call reaches the hold.
func (h *Hold) Reached() <-chan struct{} {
	return h.reached
}

// Waiting returns how many calls are blocked at the hold now.
func (h *Hold) Waiting() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.waiting
}

// Release frees every call blocked at the hold and lets every later call
// pass. It may be called more than once.
func (h *Hold) Release() {
	h.releaseOnce.Do(func() {
		s := h.seam
		s.mu.Lock()
		s.holds = slices.DeleteFunc(s.holds, func(o *Hold) bool { return o == h })
		s.mu.Unlock()

		close(h.released)
	})
}

// hold blocks a call of op on key at every hold that matches it, in the order
// they were made. The caller must not hold mu.
func (s *Seam) hold(op, key string) {
	s.mu.Lock()
	var matched []*Hold
	for _, h := range s.holds {
		if sim.Match(h.op, op) && sim.Match(h.key, key) {
			matched = append(matched, h)
		}
	}
	s.mu.Unlock()

	for _, h := range matched {
		h.wait()
	}
}

func (h *Hold) wait() {
	h.mu.Lock()
	h.waiting++
	h.mu.Unlock()

	h.reachedOnce.Do(func() { close(h.reached) })
	<-h.released

	h.mu.Lock()
	h.waiting--
	h.mu.Unlock()
}

//go:build journal_conformance

package conformance

import (
	"bytes"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test"
)

// TestClock_WallJumpMovesNoTimer checks that the journal times its waits on
// the monotonic clock: with the wall clock stepped back an hour after a
// bounded write, the write is still synced within a few bounds of monotonic
// time, while the times the journal reports, such as OldestDirty, follow the
// wall clock.
//
// The wall clock is stepped back, not forward: a journal may sync before the
// bound, so a forward jump that fired a sync early would prove nothing wrong.
// One that timed its waits by the wall clock would wait the extra hour.
func TestClock_WallJumpMovesNoTimer(t *testing.T) {
	f := journalv2test.New(t)
	h := f.Share(0)
	syncs := f.Seam.Hold(t, journalv2test.OpSync, "*.seg")

	// The write runs aside, so a journal that syncs inside it parks at the
	// hold rather than stopping the test. The jump waits for the write to
	// return, so the write's time is read before it: stepped first, the clock
	// would give a journal timing by the wall clock a consistent start.
	before := pattern("before", 4096)
	var v0 journalv2.Version
	wrote := make(chan error, 1)
	go func() {
		var err error
		v0, err = tryWrite(t, h, journalv2test.File(0), 0, before)
		wrote <- err
	}()
	var werr error
	returned := false
	select {
	case werr = <-wrote:
		returned = true
	case <-syncs.Reached():
		// The journal synced inside WriteAt: no timer is left for the jump
		// to move, and the leg below proves nothing for this journal.
	}
	f.Jump(-time.Hour)

	// decision: the clock moves in tenths of the bound, up to ten bounds,
	// because nothing pins that the stream's timer is armed before WriteAt
	// returns: a timer armed after a single Advance would wait for the next.
	// Ten bounds is far below the hour a journal timing by the wall clock
	// waits. Between steps, only a real-time wait can see the sync arrive on
	// the journal's goroutine.
	reached := false
	for range 100 {
		f.Advance(journalv2.SyncBound / 10)
		select {
		case <-syncs.Reached():
			reached = true
		case <-time.After(20 * time.Millisecond):
		}
		if reached {
			break
		}
	}
	if !reached {
		t.Fatal("no sync within ten bounds of monotonic time, with the wall clock stepped back an hour")
	}
	syncs.Release()
	if !returned {
		werr = <-wrote
	}
	stub(t, "WriteAt", werr)
	if werr != nil {
		t.Fatalf("the write: %v", werr)
	}

	// The sync the hold caught is the one that made the bounded write
	// durable, not a sync of some other segment.
	seg := segmentName(heldEntry(t, f, journalv2test.File(0)).Location.Segment)
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(time.Millisecond) {
		durable, err := f.Seam.Durable(seg)
		if err == nil && bytes.Contains(durable, before) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the bounded write is not durable after the sync")
		}
	}

	after := f.Now()
	write(t, h, journalv2test.File(1), 0, pattern("after", 4096))
	mark(t, h, journalv2test.File(0), journalv2.Extent{Off: 0, Len: 4096}, v0)
	if got := f.J.Stats().OldestDirty; !got.Equal(after) {
		t.Fatalf("OldestDirty = %v for a write made after the jump, want the jumped wall time %v", got, after)
	}
}

//go:build journal_conformance

package conformance

import (
	"bytes"
	"fmt"
	"strings"
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

	// The sync the hold caught is the one that makes the bounded write
	// durable, not a sync of some other segment: the segment it is on holds
	// the write in its live bytes, which the seam makes durable when the held
	// call runs, and the index places the write there. Both are read at the
	// hold, since nothing at the seam marks a released call as done.
	var synced string
	for _, name := range segmentFiles(t, f) {
		if f.Seam.CallsFor(journalv2test.OpSync, name) != 0 && bytes.Contains(liveBytes(t, f, name), before) {
			synced = name
		}
	}
	if synced == "" {
		var counts []string
		for _, name := range segmentFiles(t, f) {
			counts = append(counts, fmt.Sprintf("%s synced %d times", name, f.Seam.CallsFor(journalv2test.OpSync, name)))
		}

		t.Fatalf("the sync within the bound is on no segment whose live bytes hold the bounded write: %s", strings.Join(counts, ", "))
	}

	syncs.Release()
	if !returned {
		werr = <-wrote
	}
	stub(t, "WriteAt", werr)
	if werr != nil {
		t.Fatalf("the write: %v", werr)
	}

	if got := segmentName(heldEntry(t, f, journalv2test.File(0)).Location.Segment); got != synced {
		t.Fatalf("the bounded write is held in %s, not in %s, the segment synced within the bound", got, synced)
	}

	after := f.Now()
	write(t, h, journalv2test.File(1), 0, pattern("after", 4096))
	mark(t, h, journalv2test.File(0), journalv2.Extent{Off: 0, Len: 4096}, v0)
	if got := f.J.Stats().OldestDirty; !got.Equal(after) {
		t.Fatalf("OldestDirty = %v for a write made after the jump, want the jumped wall time %v", got, after)
	}
}

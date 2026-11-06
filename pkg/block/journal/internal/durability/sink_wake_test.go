package durability

import (
	"context"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
)

// The synthetic records below sit in segment ids far above any the rig's real
// streams use, so a made-up Track and Synced never meets a real record.
const synthSeg = core.SegmentID(9000)

// synth is a made-up record of the file in the segment, at the offset.
func synth(f core.FileID, seg core.SegmentID, off int64) segment.Written {
	return segment.Written{File: f, Loc: segment.Location{Segment: seg, Off: off, Len: 64}}
}

// syncOf runs Sync of the files under a deadline, so a Sync nothing releases
// fails on its assertion instead of hanging the suite.
func (r *rig) syncOf(ids ...core.FileID) <-chan error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	ch := make(chan error, 1)
	go func() {
		defer cancel()
		ch <- r.d.Sync(ctx, ids...)
	}()
	return ch
}

// TestSyncedReleasesAWaiterTheWorkerDoesNotLead pins that a sync report wakes
// a parked Sync on its own: the waiter's record is in an older segment, so no
// worker sync follows, and the report is all that can release it.
func TestSyncedReleasesAWaiterTheWorkerDoesNotLead(t *testing.T) {
	prof.Watch(t, prof.Options{})
	r := newRig(t, 1)
	x, other := file(1), file(2)
	a := synth(x, synthSeg+1, 8)
	r.sinks[0].Track(a)
	r.sinks[0].Track(synth(other, synthSeg+2, 8))

	ch := r.syncOf(x)
	mustBlock(t, "Sync of a record in an older segment", ch)
	r.sinks[0].Synced(a.Loc.Segment, a.Loc.Off+a.Loc.Len)
	if err := mustReturn(t, "Sync after the segment's sync report", ch); err != nil {
		t.Fatalf("Sync = %v, want nil", err)
	}
}

// TestAWaiterOnTwoStreamsWakesOnEitherStreamsReport pins that a Sync naming
// files on two streams wakes on a report of either one, in whichever order
// the reports arrive.
func TestAWaiterOnTwoStreamsWakesOnEitherStreamsReport(t *testing.T) {
	for _, first := range []uint32{0, 1} {
		second := 1 - first
		t.Run(map[uint32]string{0: "stream0First", 1: "stream1First"}[first], func(t *testing.T) {
			prof.Watch(t, prof.Options{})
			r := newRig(t, 2)
			x, y := file(1), file(2)
			recs := map[uint32]segment.Written{0: synth(x, synthSeg+1, 8), 1: synth(y, synthSeg+11, 8)}
			for no, w := range recs {
				r.sinks[no].Track(w)
				r.sinks[no].Track(synth(file(byte(10+no)), w.Loc.Segment+1, 8))
			}

			ch := r.syncOf(x, y)
			mustBlock(t, "Sync over two streams", ch)
			r.sinks[first].Synced(recs[first].Loc.Segment, recs[first].Loc.Off+recs[first].Loc.Len)
			mustBlock(t, "Sync with one stream's record still unsynced", ch)
			r.sinks[second].Synced(recs[second].Loc.Segment, recs[second].Loc.Off+recs[second].Loc.Len)
			if err := mustReturn(t, "Sync after both streams reported", ch); err != nil {
				t.Fatalf("Sync = %v, want nil", err)
			}
		})
	}
}

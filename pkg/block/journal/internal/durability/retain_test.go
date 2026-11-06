package durability

import (
	"context"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// publish appends a write of payload at off to f and publishes it through the
// index with retain, as a writer does: both under the file's append domain.
// It returns where the record landed and its version.
func (r *rig) publish(t *testing.T, f core.FileID, off int64, payload string) (segment.Location, core.Version) {
	t.Helper()
	return r.publishGap(t, f, off, payload, nil)
}

// publishGap is publish with gap run between the append and the publish, still
// under the file's append domain.
func (r *rig) publishGap(t *testing.T, f core.FileID, off int64, payload string, gap func()) (segment.Location, core.Version) {
	t.Helper()
	v, err := r.d.counters.Draw()
	if err != nil {
		t.Fatalf("Draw: %v", err)
	}
	seq, err := r.d.counters.NextSeq()
	if err != nil {
		t.Fatalf("NextSeq: %v", err)
	}
	x := core.Extent{Off: off, Len: int64(len(payload))}
	unlock := r.domains.Lock(f)
	defer unlock()
	p, err := r.streams.For(f).Append(format.RecordHeader{
		Kind: format.KindWrite, File: f, Extent: x, Seq: seq, Version: v, Tag: 1,
	}, format.RecordBody{Payload: []byte(payload), MTime: 1})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if gap != nil {
		gap()
	}
	placed := index.Placed{Extent: x, Record: index.RecordAt{Segment: p.Loc.Segment, Offset: uint32(p.Loc.Off)}, Version: v}
	if _, err := r.ix.WriteRetain(1, f, placed, seq); err != nil {
		t.Fatalf("WriteRetain: %v", err)
	}
	return p.Loc, v
}

// retainedBy returns the retained pieces of f superseded by v.
func (r *rig) retainedBy(f core.FileID, v core.Version) []index.Retained {
	var out []index.Retained
	for _, p := range r.ix.Retained(f) {
		if p.By == v {
			out = append(out, p)
		}
	}
	return out
}

// TestSyncLiftsWhatTheSyncedRecordSuperseded pins that a superseded extent is
// retained until the record that superseded it is synced, and not after.
func TestSyncLiftsWhatTheSyncedRecordSuperseded(t *testing.T) {
	r := newRig(t, 1)
	f := file(1)
	_, v1 := r.publish(t, f, 0, "aaaa")
	if err := r.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync of v1: %v", err)
	}
	_, v2 := r.publish(t, f, 0, "bbbb")

	if got := r.retainedBy(f, v2); len(got) != 1 || got[0].Version != v1 {
		t.Fatalf("before the sync the retained pieces superseded by v2 are %+v, want v1's piece", got)
	}
	if p, ok := r.ix.Lookup(f, 0); !ok || p.Version != v2 {
		t.Fatalf("before the sync Lookup serves %+v, %v, want v2", p, ok)
	}

	if err := r.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync of v2: %v", err)
	}
	if got := r.ix.Retained(f); len(got) != 0 {
		t.Fatalf("after the sync %+v is still retained", got)
	}
	if p, ok := r.ix.Lookup(f, 0); !ok || p.Version != v2 {
		t.Fatalf("after the sync Lookup serves %+v, %v, want v2", p, ok)
	}
}

// TestSyncOfARecordThatSupersededNothingAccountsNothing pins that a lift with
// nothing to drop changes no retained piece and reaches no account.
func TestSyncOfARecordThatSupersededNothingAccountsNothing(t *testing.T) {
	r := newRig(t, 1)
	f, g := file(1), file(2)
	_, v1 := r.publish(t, f, 0, "aaaa")
	r.publish(t, g, 0, "gggg")
	if err := r.d.Sync(context.Background(), f, g); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got := r.ix.Retained(f); len(got) != 0 {
		t.Fatalf("retained %+v, want none", got)
	}
	if p, ok := r.ix.Lookup(f, 0); !ok || p.Version != v1 {
		t.Fatalf("Lookup serves %+v, %v, want v1", p, ok)
	}
	if got := r.acct.all(); len(got) != 0 {
		t.Fatalf("the account received %+v for records that superseded nothing", got)
	}
}

// TestSyncReleasesTheSegmentsRetainedBytes pins that the segment of the
// superseded record counts its bytes as retained until the superseding record
// syncs.
func TestSyncReleasesTheSegmentsRetainedBytes(t *testing.T) {
	r := newRig(t, 1)
	f := file(1)
	loc1, _ := r.publish(t, f, 0, "aaaa")
	if err := r.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync of v1: %v", err)
	}
	r.publish(t, f, 0, "bbbb")
	if got := r.ix.SegmentBytes(loc1.Segment).Retained; got != 4 {
		t.Fatalf("before the sync the segment retains %d bytes, want 4", got)
	}
	if err := r.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync of v2: %v", err)
	}
	if got := r.ix.SegmentBytes(loc1.Segment).Retained; got != 0 {
		t.Fatalf("after the sync the segment retains %d bytes, want 0", got)
	}
}

// TestSyncAccountsTheLift pins that the lift's delta reaches Config.Account,
// so the counters an account feeds see the retained bytes leave.
func TestSyncAccountsTheLift(t *testing.T) {
	r := newRig(t, 1)
	f := file(1)
	r.publish(t, f, 0, "aaaa")
	if err := r.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync of v1: %v", err)
	}
	r.publish(t, f, 0, "bbbb")
	if err := r.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync of v2: %v", err)
	}
	var retained int64
	for _, hd := range r.acct.all() {
		retained += hd.RetainedBytes
		if hd.File != f {
			t.Fatalf("a delta names %v, want %v", hd.File, f)
		}
	}
	if retained != -4 {
		t.Fatalf("the account saw %d retained bytes change, want -4", retained)
	}
}

// TestSyncInTheGapLiftsAfterThePublish pins that a sync which completes
// between a writer's append and its publish lifts only after the publish: the
// lift waits for the file's append domain, so what the writer retains is not
// left retained for good.
func TestSyncInTheGapLiftsAfterThePublish(t *testing.T) {
	r := newRig(t, 1)
	f := file(1)
	r.publish(t, f, 0, "aaaa")
	if err := r.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync of v1: %v", err)
	}

	appended := make(chan segment.Location, 1)
	release := make(chan struct{})
	done := make(chan core.Version, 1)
	go func() {
		_, v2 := r.publishGap(t, f, 0, "bbbb", func() {
			appended <- segment.Location{}
			<-release
		})
		done <- v2
	}()
	<-appended

	// The stream syncs while the writer holds f's domain: v2's record is
	// covered before it is published.
	r.domains.mu.Lock()
	taken := r.domains.taken
	r.domains.mu.Unlock()
	r.syncStream(t, f)
	// The worker has completed v2's entry up to the file's domain, which it
	// asks for and the writer holds.
	eventually(t, "the worker to ask for the file's domain", func() bool {
		r.domains.mu.Lock()
		defer r.domains.mu.Unlock()
		return r.domains.taken > taken
	})

	close(release)
	v2 := <-done
	if err := r.d.Sync(context.Background(), f); err != nil {
		t.Fatalf("Sync of v2: %v", err)
	}
	eventually(t, "v1 to leave the retained set", func() bool { return len(r.retainedBy(f, v2)) == 0 })
	if got := r.ix.Retained(f); len(got) != 0 {
		t.Fatalf("%+v is retained after the sync that covered v2", got)
	}
}

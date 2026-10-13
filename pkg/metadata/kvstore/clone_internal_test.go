package kvstore

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
)

func (f *cfix) cloneOp(src, dst entity.FileID, srcOff, dstOff, n int64, v uint64) CloneOp {
	return CloneOp{Share: f.share, File: dst, Version: entity.VersionOf(v), Time: time.Unix(int64(1_900_000_000+v), 0),
		Spec: entity.CloneSpec{SrcShare: f.share, Src: src, SrcOff: srcOff, DstOff: dstOff, Len: n}}
}

func (f *cfix) clone(op CloneOp) {
	f.t.Helper()
	if err := f.s.Existence().Clone(context.Background(), op, nil); err != nil {
		f.t.Fatal(err)
	}
}

// RFC 6 §6.6: a clone copies refs, not bytes. The destination keeps the
// source's holes, adopts its carved refs re-versioned at the clone's
// version, leaves its uncarved runs to the journal copy, and is not served
// until the last batch; the source cannot be released meanwhile.
func TestClone(t *testing.T) {
	f := newContentFix(t, nil)
	f.s.SetKeyBudget(4*batchItemKeys + 4) // one record of each kind per batch
	ctx := context.Background()
	src, dst := f.file(0), f.file(0)
	a, b := chunk(1), chunk(2)
	f.exist(src, write(1, 0, 4*mib), write(2, 6*mib, 6*mib))
	f.commit(src, carry(a, b), ref(0, 4*mib, a, 1, 1), ref(6*mib, 4*mib, b, 2, 2))
	f.want(src, "source", "carved[0,4) hole[4,6) carved[6,10) uncarved[10,12)")

	f.clone(f.cloneOp(src, dst, 0, 2*mib, 12*mib, 5))
	for _, err := range f.s.Content().Covering(ctx, f.share, dst, 3*mib, 1) {
		if !errors.Is(err, entity.ErrDelay) {
			t.Errorf("a half-done clone's extent served: %v", err)
		}
	}
	f.unlinked(src)
	if err := f.s.Existence().Remove(ctx, f.share, src, 0, release(9), nil); !errors.Is(err, ErrHeld) {
		t.Errorf("released a clone's source: %v", err)
	}

	f.resume(dst)
	f.want(dst, "after the clone", "hole[0,2) carved[2,6) hole[6,8) carved[8,12) uncarved[12,14)")
	for _, r := range f.refs(dst) {
		if r.Oldest != entity.VersionOf(5) || r.Newest != entity.VersionOf(5) {
			t.Errorf("cloned ref not re-versioned: %+v", r)
		}
	}
	for _, h := range []entity.ChunkHash{a, b} {
		if rec, _ := f.chunkRec(h); rec.Refcount != 2 {
			t.Errorf("refcount %d, want 2", rec.Refcount)
		}
	}
	if got := f.get(dst); got.Size != 14*mib || got.Charged != 10*mib {
		t.Errorf("destination size %d charged %d", got.Size, got.Charged)
	}
	if err := f.s.Existence().Remove(ctx, f.share, src, 0, release(9), nil); err != nil {
		t.Errorf("the source is still held after the clone: %v", err)
	}
	f.checkCounts()

	f.clone(f.cloneOp(src, dst, 0, 2*mib, 12*mib, 5)) // a replay changes nothing
	f.want(dst, "replay", "hole[0,2) carved[2,6) hole[6,8) carved[8,12) uncarved[12,14)")
}

// RFC 6 §6.6: the hole a clone's last batch joins raises the destination's
// MaxSpan, so a lookup starting inside it still finds its start while
// another removal is not done.
func TestCloneJoinRaisesMaxSpan(t *testing.T) {
	f := newContentFix(t, nil)
	src, dst := f.file(0), f.file(0)
	f.exist(src, write(1, mib, mib))
	f.exist(dst, write(1, 0, 4*mib), write(2, 5*mib, 2*mib))
	f.clone(f.cloneOp(src, dst, 0, 5*mib, mib, 3)) // the source's hole lands beside dst's
	f.resume(dst)
	f.want(dst, "after the clone", "uncarved[0,4) hole[4,6) uncarved[6,7)")
	if got := f.get(dst); got.MaxSpan != 2*mib {
		t.Errorf("max span %d, want the joined hole's %d", got.MaxSpan, 2*mib)
	}

	f.remove(dst, dealloc(4, 0, mib)) // phase 1 only: a removal not done
	if got := f.spans(dst, 5*mib+mib/2, mib/2); len(got) != 1 || got[0].Class != entity.Hole {
		t.Errorf("inside the joined hole: %+v", got)
	}
}

// RFC 6 §6.6 and RFC 8 §9.1 step 4: a clone's phase 1 writes O(1) keys
// however many holes the source has, recording the whole extent as existing
// and charged; the batches write the holes within the key budget and give
// their bytes back. A clone of a destination still being cloned waits.
func TestCloneHolesInPhase2(t *testing.T) {
	var w *writesKV
	f := newContentFix(t, func(db kv.KV) kv.KV { w = &writesKV{KV: db}; return w })
	ctx := context.Background()
	src, dst, third := f.file(0), f.file(0), f.file(0)
	var ops []PendingOp
	const writes = 120
	for i := range int64(writes) {
		ops = append(ops, write(uint64(i+1), 2*i*mib, mib)) // a 1 MiB hole after each write but the last
	}
	f.exist(src, ops...)

	w.max = 0
	f.clone(f.cloneOp(src, dst, 0, 0, (2*writes-1)*mib, 50))
	if w.max > 12 {
		t.Errorf("phase 1 wrote %d keys for %d source holes", w.max, writes-1)
	}
	if got := f.holeRecords(dst); got != "" {
		t.Errorf("phase 1 wrote holes %s", got)
	}
	if got := f.get(dst).Charged; got != (2*writes-1)*mib {
		t.Errorf("charged %d after phase 1, want the whole extent", got)
	}
	f.checkUsage()
	if err := f.s.Existence().Clone(ctx, f.cloneOp(dst, third, 0, 0, mib, 51), nil); !errors.Is(err, entity.ErrDelay) {
		t.Errorf("a clone of a destination being cloned: %v", err)
	}

	budget := 5*batchItemKeys + 4 // one record of each kind per batch
	f.s.SetKeyBudget(budget)
	w.max = 0
	f.resume(dst)
	if w.max > budget {
		t.Errorf("a batch wrote %d keys, budget %d", w.max, budget)
	}
	f.want(dst, "after the clone", f.layout(src))
	if got := f.get(dst); got.Charged != writes*mib || got.MaxSpan != mib {
		t.Errorf("charged %d, max span %d", got.Charged, got.MaxSpan)
	}
	if got := strings.Count(f.holeRecords(dst), "@50"); got != writes-1 {
		t.Errorf("%d holes at the clone's version, want %d", got, writes-1)
	}
	f.checkUsage()
	f.checkCounts()
}

// A clone over existing content masks and drops what was there below its
// version, and leaves alone what was written after it.
func TestCloneOverContent(t *testing.T) {
	f := newContentFix(t, nil)
	src, dst := f.file(0), f.file(0)
	a, c, d := chunk(1), chunk(3), chunk(4)
	f.exist(src, write(1, 0, 2*mib))
	f.commit(src, carry(a), ref(0, 2*mib, a, 1, 1))
	f.exist(dst, write(1, 0, 4*mib))
	f.commit(dst, carry(c), ref(0, 4*mib, c, 1, 1))

	f.clone(f.cloneOp(src, dst, 0, mib, 2*mib, 5))
	// Written and offloaded after the clone, before its batches run.
	f.exist(dst, write(6, 2*mib, mib/2))
	f.commit(dst, carry(d), ref(2*mib, mib/2, d, 6, 6))
	f.resume(dst)
	f.want(dst, "after the clone", "carved[0,1) carved[1,2) carved[2,2.5) carved[2.5,3) carved[3,4)")
	for _, r := range f.refs(dst) {
		if r.Offset == 2*mib && r.Chunk != d {
			t.Errorf("the clone overwrote a later write: %+v", r)
		}
	}
	if rec, _ := f.chunkRec(c); rec.Refcount != 2 {
		t.Errorf("C's refcount %d, want 2 pieces", rec.Refcount)
	}
	f.checkCounts()
}

// Every refusal comes before anything changes.
func TestCloneRefusals(t *testing.T) {
	f := newContentFix(t, nil)
	ctx := context.Background()
	src := f.file(0)
	f.exist(src, write(1, 0, 4*mib))
	other := entity.NewShareID()
	if err := f.s.ControlPlane().PutShare(ctx, entity.Share{ID: other, Name: "other", Path: "/other", Root: entity.NewFileID()}); err != nil {
		t.Fatal(err)
	}
	foreign := entity.NewFileID()
	if err := f.s.kv.Update(ctx, func(tx kv.Txn) error {
		return putFile(tx, entity.JournalVersion{}, entity.File{ID: foreign, Share: other, Type: entity.Regular, Number: 9, Nlink: 1, Version: entity.VersionOf(1)})
	}); err != nil {
		t.Fatal(err)
	}
	before := len(f.keysUnder(src))
	for name, c := range map[string]struct {
		op   CloneOp
		want func(error) bool
	}{
		"overlapping itself":    {f.cloneOp(src, src, 0, mib, 2*mib, 5), isInvalid},
		"past the source's end": {f.cloneOp(src, src, 3*mib, 8*mib, 2*mib, 5), isInvalid},
		"across namespaces": {CloneOp{Share: other, File: foreign, Version: entity.VersionOf(5),
			Spec: entity.CloneSpec{SrcShare: f.share, Src: src, Len: mib}}, func(err error) bool { return errors.Is(err, ErrCrossNamespace) }},
	} {
		if err := f.s.Existence().Clone(ctx, c.op, nil); !c.want(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if after := len(f.keysUnder(src)); after != before {
		t.Errorf("a refused clone changed the source's keys: %d -> %d", before, after)
	}
	if rs := f.removals(src); len(rs) != 0 {
		t.Errorf("a refused clone left a removal: %+v", rs)
	}
}

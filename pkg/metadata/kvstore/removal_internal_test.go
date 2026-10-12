package kvstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
)

func truncate(v uint64, size int64) RemoveOp {
	return RemoveOp{Version: entity.VersionOf(v), Kind: entity.RemoveTruncate, Extent: entity.Extent{Offset: size},
		Time: time.Unix(int64(1_900_000_000+v), 0)}
}

func dealloc(v uint64, off, n int64) RemoveOp {
	return RemoveOp{Version: entity.VersionOf(v), Kind: entity.RemoveDeallocate, Extent: entity.Extent{Offset: off, Length: n},
		Time: time.Unix(int64(1_900_000_000+v), 0)}
}

func (f *cfix) remove(file entity.FileID, op RemoveOp) {
	f.t.Helper()
	if err := f.s.Existence().Remove(context.Background(), f.share, file, 0, op, nil); err != nil {
		f.t.Fatal(err)
	}
}

func (f *cfix) resume(file entity.FileID) {
	f.t.Helper()
	if err := f.s.Content().Resume(context.Background(), f.share, file); err != nil {
		f.t.Fatal(err)
	}
}

func (f *cfix) removals(file entity.FileID) []entity.Removal {
	f.t.Helper()
	var out []entity.Removal
	if err := f.s.kv.View(context.Background(), func(r kv.Reader) (err error) {
		out, err = removals(r, f.share, file)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return out
}

func (f *cfix) want(file entity.FileID, step, layout string) {
	f.t.Helper()
	if got := f.layout(file); got != layout {
		f.t.Fatalf("%s: %s, want %s", step, got, layout)
	}
}

// RFC 6 §6.2: a truncate masks what it removes from phase 1 on, a truncate
// up after it reads as a hole however the masked records overlap it, and
// phase 2 drops only what is older than the removal.
func TestTruncate(t *testing.T) {
	f := newContentFix(t, nil)
	file := f.file(0)
	a, b := chunk(1), chunk(2)
	f.exist(file, write(1, 0, 4*mib), write(2, 10*mib, mib))
	f.commit(file, carry(a, b), ref(0, 4*mib, a, 1, 1), ref(10*mib, mib, b, 2, 2))

	f.remove(file, truncate(4, 3*mib))
	f.want(file, "phase 1", "carved[0,3)")
	if got := f.get(file); got.Size != 3*mib || got.Charged != 3*mib || got.Applied != entity.VersionOf(4) {
		t.Errorf("size %d charged %d applied %d", got.Size, got.Charged, got.Applied.Counter())
	}
	f.exist(file, grow(5, 12*mib))
	f.want(file, "truncate up over masked records", "carved[0,3) hole[3,12)")
	// A lookup starting inside the masked hole, which sorts nearer than the
	// new one covering it, still finds the new one.
	if s := f.spans(file, 5*mib, mib); len(s) != 1 || s[0].Class != entity.Hole {
		t.Errorf("lookup inside the masked hole: %+v", s)
	}
	if rs := f.removals(file); len(rs) != 1 || rs[0].Done {
		t.Fatalf("removals %+v", rs)
	}
	// A write over the masked hole fills only the new one.
	f.exist(file, write(6, 5*mib, mib))
	f.want(file, "a write over the masked hole", "carved[0,3) hole[3,5) uncarved[5,6) hole[6,12)")
	if got := f.get(file).Charged; got != 4*mib {
		t.Errorf("charged %d after filling 1 MiB", got)
	}

	f.resume(file)
	f.want(file, "phase 2", "carved[0,3) hole[3,5) uncarved[5,6) hole[6,12)")
	if rs := f.removals(file); len(rs) != 1 || !rs[0].Done {
		t.Errorf("removals after phase 2 %+v", rs)
	}
	if refs := f.refs(file); len(refs) != 1 || refs[0].Length != 3*mib {
		t.Errorf("refs %+v", refs)
	}
	if c, ok := f.chunkRec(b); ok && c.Refcount != 0 {
		t.Errorf("B's refcount %d", c.Refcount)
	}
	f.exist(file, write(7, 3*mib, mib))
	f.want(file, "a write into the regrown extent", "carved[0,3) uncarved[3,4) hole[4,5) uncarved[5,6) hole[6,12)")

	// A replay changes nothing; a truncate up is not a removal.
	f.remove(file, truncate(4, 3*mib))
	f.resume(file)
	if err := f.s.Existence().Remove(context.Background(), f.share, file, 0, truncate(8, 20*mib), nil); !isInvalid(err) {
		t.Errorf("truncate up through Remove: %v", err)
	}
	f.want(file, "replay", "carved[0,3) uncarved[3,4) hole[4,5) uncarved[5,6) hole[6,12)")
	f.checkCounts()
}

func isInvalid(err error) bool {
	var se *merrs.StoreError
	return errors.As(err, &se) && se.Code == merrs.ErrInvalidArgument
}

// RFC 6 §6.2: a deallocate reads as one hole at once; a write after it is
// kept by phase 2, which splits the ref spanning the extent in two.
func TestDeallocate(t *testing.T) {
	f := newContentFix(t, nil)
	f.s.SetKeyBudget(3*batchItemKeys + 4) // one record of each kind per batch
	file := f.file(0)
	c := chunk(3)
	f.exist(file, write(1, 0, 8*mib))
	f.commit(file, carry(c), ref(0, 8*mib, c, 1, 1))

	f.remove(file, dealloc(2, 2*mib, 2*mib))
	f.want(file, "phase 1", "carved[0,2) hole[2,4) carved[4,8)")
	if got := f.get(file); got.Charged != 6*mib {
		t.Errorf("charged %d", got.Charged)
	}
	f.exist(file, write(3, 3*mib, mib/2))
	f.want(file, "a write after it", "carved[0,2) hole[2,3) uncarved[3,3.5) hole[3.5,4) carved[4,8)")

	f.resume(file)
	f.want(file, "phase 2", "carved[0,2) hole[2,3) uncarved[3,3.5) hole[3.5,4) carved[4,8)")
	if refs := f.refs(file); len(refs) != 2 {
		t.Errorf("refs %+v", refs)
	}
	if rec, _ := f.chunkRec(c); rec.Refcount != 2 {
		t.Errorf("C's refcount %d, want 2 after the split", rec.Refcount)
	}
	f.checkCounts()
}

// RFC 6 §6.2's transfer row: a pass offered before a truncate commits after
// it, and a truncate up must not turn its refs back into readable content.
func TestOffloadAfterTruncate(t *testing.T) {
	f := newContentFix(t, nil)
	file := f.file(0)
	d := chunk(4)
	f.exist(file, write(1, 0, 4*mib))
	f.remove(file, truncate(2, mib))
	_, res := f.commit(file, carry(d), ref(0, 4*mib, d, 1, 1))
	if got := res[file]; len(got) != 1 || got[0] != entity.ExtentOf(0, mib) {
		t.Errorf("reported offloaded %v", got)
	}
	f.exist(file, grow(3, 4*mib))
	f.want(file, "regrown", "carved[0,1) hole[1,4)")
	f.resume(file)
	f.want(file, "phase 2", "carved[0,1) hole[1,4)")
	f.checkCounts()
}

// RFC 6 §11.1's batching row: with K forced low, every sub-transaction of a
// truncate of many refs leaves the extent reading as existence says, and a
// write into it survives phase 2.
func TestRemovalBatches(t *testing.T) {
	f := newContentFix(t, nil)
	f.s.SetKeyBudget(3*batchItemKeys + 4)
	file := f.file(0)
	const n = 20
	f.exist(file, write(1, 0, n*mib))
	for i := range int64(n) {
		h := chunk(byte(10 + i))
		f.commit(file, carry(h), ref(i*mib, mib, h, 1, 1))
	}
	f.remove(file, truncate(2, 0))
	f.exist(file, grow(3, n*mib), write(4, 5*mib, mib))
	const want = "hole[0,5) uncarved[5,6) hole[6,20)"
	f.want(file, "phase 1", want)
	batches := 0
	for {
		var more bool
		if err := f.s.kv.Update(context.Background(), func(tx kv.Txn) (err error) {
			more, err = f.s.resumeBatch(tx, f.share, file)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if !more {
			break
		}
		batches++
		f.want(file, "between batches", want)
		f.checkCounts()
	}
	if batches < n {
		t.Errorf("%d batches for %d refs at one ref a batch", batches, n)
	}
	if refs := f.refs(file); len(refs) != 0 {
		t.Errorf("refs left %+v", refs)
	}
}

// Done removals at or below the in-flight floor are pruned; others stay.
func TestPruneRemovals(t *testing.T) {
	f := newContentFix(t, nil)
	file := f.file(0)
	f.exist(file, write(1, 0, 4*mib))
	f.remove(file, truncate(2, 3*mib))
	f.remove(file, truncate(3, 2*mib))
	c := f.s.Content()
	ctx := context.Background()
	if err := c.PruneRemovals(ctx, f.share, file, entity.VersionOf(9)); err != nil {
		t.Fatal(err)
	}
	if rs := f.removals(file); len(rs) != 2 {
		t.Errorf("pruned a removal not done: %+v", rs)
	}
	f.resume(file)
	if err := c.PruneRemovals(ctx, f.share, file, entity.VersionOf(2)); err != nil {
		t.Fatal(err)
	}
	if rs := f.removals(file); len(rs) != 1 || rs[0].Version != entity.VersionOf(3) {
		t.Errorf("after pruning at 2: %+v", rs)
	}
}

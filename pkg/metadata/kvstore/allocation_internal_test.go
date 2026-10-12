package kvstore

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
)

// RFC 6 §3.5: a zero ref reads as a hole to every allocation answer, except
// under a newer overwrite record, where its current bytes are data.
func TestAllocation(t *testing.T) {
	f := newContentFix(t, nil)
	ctx := context.Background()
	file := f.file(0)
	a := chunk(1)
	f.exist(file, write(1, 0, 4*mib), write(2, 10*mib, mib))
	f.commit(file, carry(a), ref(0, mib, entity.ChunkHash{}, 1, 1), ref(mib, 3*mib, a, 1, 1))
	type run struct {
		start, end int64
		hole       bool
	}
	check := func(step string, want []run) {
		t.Helper()
		for _, w := range want {
			got, hole, err := f.s.Existence().Allocation(ctx, f.share, file, w.start)
			if err != nil || got != entity.ExtentOf(w.start, w.end) || hole != w.hole {
				t.Errorf("%s at %d: %v hole %v %v, want [%d,%d) hole %v", step, w.start, got, hole, err, w.start, w.end, w.hole)
			}
		}
	}
	check("carved", []run{{0, mib, true}, {mib, 4 * mib, false}, {4 * mib, 10 * mib, true}, {10 * mib, 11 * mib, false}, {11 * mib, 11 * mib, false}})
	f.exist(file, write(3, 0, mib/2))
	check("zero ref under an overwrite", []run{{0, mib / 2, false}, {mib / 2, mib, true}})

	big := f.file(0)
	f.exist(big, write(1, 0, 300*mib))
	check2, hole, err := f.s.Existence().Allocation(ctx, f.share, big, 5)
	if err != nil || hole || check2 != entity.ExtentOf(5, 300*mib) {
		t.Errorf("a run past several windows: %v hole %v %v", check2, hole, err)
	}
}

// RFC 6 §3.3: pruning deletes an overwrite record only once refs at or
// above its version cover every part outside the holes, never one with a
// part no ref covers.
func TestPruneOverwrites(t *testing.T) {
	f := newContentFix(t, nil)
	ctx := context.Background()
	file := f.file(0)
	a, b := chunk(1), chunk(2)
	f.exist(file, write(1, 0, 4*mib))
	f.commit(file, carry(a), ref(0, 4*mib, a, 1, 1))
	f.exist(file, write(2, mib, 2*mib))
	prune := func() {
		t.Helper()
		if _, err := f.s.Existence().PruneOverwrites(ctx, f.share, file); err != nil {
			t.Fatal(err)
		}
	}
	prune() // the old ref is below the record: nothing goes
	f.want(file, "under the old ref", "carved[0,1) uncarved[1,3)@2 carved[3,4)")
	f.commit(file, carry(b), ref(mib, mib, b, 2, 2))
	prune() // half the record still has no ref at its version
	f.want(file, "half covered", "carved[0,1) carved[1,2) uncarved[2,3)@2 carved[3,4)")
	f.commit(file, carry(b), ref(2*mib, mib, b, 2, 2))
	prune()
	_ = f.s.kv.View(ctx, func(r kv.Reader) error {
		for _, err := range r.Scan(overwritesPrefix(f.share, file), nil) {
			if err == nil {
				t.Error("a covered record survived the prune")
			}
		}
		return nil
	})
	f.checkCounts()
}

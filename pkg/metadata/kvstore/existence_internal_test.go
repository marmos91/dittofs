package kvstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
)

func write(v uint64, off, n int64) PendingOp {
	return PendingOp{Version: entity.VersionOf(v), Kind: OpWrite, Extent: entity.Extent{Offset: off, Length: n},
		Time: time.Unix(int64(1_900_000_000+v), 0)}
}

func grow(v uint64, size int64) PendingOp {
	return PendingOp{Version: entity.VersionOf(v), Kind: OpGrow, Size: size, Time: time.Unix(int64(1_900_000_000+v), 0)}
}

func (f *cfix) exist(file entity.FileID, ops ...PendingOp) {
	f.t.Helper()
	failed, err := f.s.Existence().Commit(context.Background(), []PendingExistence{{Share: f.share, File: file, Ops: ops}})
	if err != nil || len(failed) != 0 {
		f.t.Fatalf("existence commit: %v %v", failed, err)
	}
}

func (f *cfix) get(file entity.FileID) entity.File {
	f.t.Helper()
	var out entity.File
	if err := f.s.kv.View(context.Background(), func(r kv.Reader) (err error) {
		out, err = fileIn(r, f.share, file)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return out
}

// layout is a file's covering answer in MiB, one "class[start,end)" per
// span, and an overwrite version after an uncarved run an overwrite made.
func (f *cfix) layout(file entity.FileID) string {
	f.t.Helper()
	var b strings.Builder
	for _, s := range f.spans(file, 0, entity.EndOfFile) {
		fmt.Fprintf(&b, "%s[%g,%g)", s.Class, float64(s.Offset)/mib, float64(s.End())/mib)
		if !s.Overwrite.IsZero() {
			fmt.Fprintf(&b, "@%d", s.Overwrite.Counter())
		}
		b.WriteByte(' ')
	}
	return strings.TrimSpace(b.String())
}

// RFC 6 §3.3 and §3.5: what each kind of write does to existence.
func TestExistenceCommit(t *testing.T) {
	f := newContentFix(t, nil)
	file := f.file(0)
	steps := []struct {
		name   string
		ops    []PendingOp
		layout string
	}{
		{"first write", []PendingOp{write(1, 0, 4*mib)}, "uncarved[0,4)"},
		{"write past size leaves a hole", []PendingOp{write(2, 10*mib, mib)}, "uncarved[0,4) hole[4,10) uncarved[10,11)"},
		{"hole fill writes no overwrite", []PendingOp{write(3, 5*mib, mib)}, "uncarved[0,4) hole[4,5) uncarved[5,6) hole[6,10) uncarved[10,11)"},
		{"truncate up joins the hole it touches", []PendingOp{write(4, 6*mib, 4*mib), grow(5, 14*mib)},
			"uncarved[0,4) hole[4,5) uncarved[5,11) hole[11,14)"},
	}
	for _, s := range steps {
		f.exist(file, s.ops...)
		if got := f.layout(file); got != s.layout {
			t.Fatalf("%s: %s, want %s", s.name, got, s.layout)
		}
	}
	got := f.get(file)
	if got.Size != 14*mib || got.Applied != entity.VersionOf(5) || got.Charged != 10*mib {
		t.Errorf("size %d applied %d charged %d", got.Size, got.Applied.Counter(), got.Charged)
	}
	if !got.Modify.Equal(time.Unix(1_900_000_005, 0)) || got.MaxSpan != 6*mib {
		t.Errorf("modify %v, max span %d", got.Modify, got.MaxSpan)
	}

	// Overwrites of committed content, once it is carved: records are split
	// by version, never merged, and a ref at or above a record's version
	// carves the extent again.
	a := chunk(1)
	f.commit(file, carry(a), ref(0, 4*mib, a, 1, 1))
	f.exist(file, write(6, mib, mib))
	f.exist(file, write(7, 3*mib/2, 3*mib/2))
	if got, want := f.layout(file), "carved[0,1) uncarved[1,1.5)@6 uncarved[1.5,3)@7 carved[3,4) hole[4,5) uncarved[5,11) hole[11,14)"; got != want {
		t.Errorf("after overwrites: %s, want %s", got, want)
	}
	// The two bounds are kept apart: the 6 MiB hole does not widen the
	// overwrite window.
	if got := f.get(file); got.MaxOverwrite != 3*mib/2 || got.MaxSpan != 6*mib {
		t.Errorf("max overwrite %d, max span %d", got.MaxOverwrite, got.MaxSpan)
	}
	b := chunk(2)
	f.commit(file, carry(b), ref(mib, 2*mib, b, 6, 7))
	if got, want := f.layout(file), "carved[0,1) carved[1,3) carved[3,4) hole[4,5) uncarved[5,11) hole[11,14)"; got != want {
		t.Errorf("after the overwrite's offload: %s, want %s", got, want)
	}

	// A replay of committed operations changes nothing, the File included.
	before := f.get(file)
	f.exist(file, write(6, mib, mib), write(7, 3*mib/2, 3*mib/2))
	if after := f.get(file); after.Version != before.Version {
		t.Errorf("a replay rewrote the file: version %d -> %d", before.Version.Counter(), after.Version.Counter())
	}
	f.checkCounts()
}

// Within one commit, the overwrite test is against what was committed
// before it: a fill and a rewrite of the filled bytes write no overwrite
// record, since no ref can cover bytes that were a hole.
func TestExistenceCommitSeesItsStart(t *testing.T) {
	f := newContentFix(t, nil)
	file := f.file(0)
	f.exist(file, write(1, 4*mib, mib))
	f.exist(file, write(2, mib, mib), write(3, mib, mib), write(4, 4*mib, mib))
	if got, want := f.layout(file), "hole[0,1) uncarved[1,2) hole[2,4) uncarved[4,5)@4"; got != want {
		t.Errorf("%s, want %s", got, want)
	}
}

// A file that fails alone fails no other in its batch.
func TestExistenceCommitPerFileFailure(t *testing.T) {
	f := newContentFix(t, nil)
	good, shrink, gone := f.file(0), f.file(2*mib), entity.NewFileID()
	failed, err := f.s.Existence().Commit(context.Background(), []PendingExistence{
		{Share: f.share, File: gone, Ops: []PendingOp{write(1, 0, mib)}},
		{Share: f.share, File: shrink, Ops: []PendingOp{grow(1, mib)}},
		{Share: f.share, File: good, Ops: []PendingOp{write(2, 0, mib)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(failed[gone], entity.ErrStale) || !errors.As(failed[shrink], new(*merrs.StoreError)) || failed[shrink].(*merrs.StoreError).Code != merrs.ErrInvalidArgument || failed[good] != nil {
		t.Errorf("failures %v", failed)
	}
	if f.get(good).Size != mib || f.get(shrink).Size != 2*mib {
		t.Error("a failure changed another file, or the failing one")
	}
	_ = f.s.kv.View(context.Background(), func(r kv.Reader) error {
		for row, err := range r.Scan(fileKey(f.share, gone), nil) {
			if err == nil {
				t.Errorf("a released file's commit wrote %x", row.Key)
			}
		}
		return nil
	})
}

// RFC 8 §5.2: a file whose pending existence needs more than K keys commits
// over several transactions in version order, and a batch over K splits.
func TestExistenceCommitSplitsAtTheBudget(t *testing.T) {
	cnt := &countingTxns{}
	f := newContentFix(t, func(db kv.KV) kv.KV { cnt.KV = db; return cnt })
	f.s.SetKeyBudget(8)
	file, other := f.file(0), f.file(0)
	var ops []PendingOp
	for i := range 20 {
		ops = append(ops, write(uint64(i+1), int64(2*i)*mib, mib)) // each but the first leaves a hole
	}
	cnt.n = 0
	failed, err := f.s.Existence().Commit(context.Background(), []PendingExistence{
		{Share: f.share, File: file, Ops: ops},
		{Share: f.share, File: other, Ops: []PendingOp{write(1, 0, mib)}},
	})
	if err != nil || len(failed) != 0 {
		t.Fatal(failed, err)
	}
	if cnt.n < 3 {
		t.Errorf("%d transactions for 21 keys at K=8", cnt.n)
	}
	if got := f.get(file); got.Size != 39*mib || got.Applied != entity.VersionOf(20) {
		t.Errorf("size %d applied %d", got.Size, got.Applied.Counter())
	}
	if got := f.layout(file); strings.Count(got, "hole") != 19 {
		t.Errorf("layout %s", got)
	}
	if f.get(other).Size != mib {
		t.Error("the second file was not committed")
	}
}

// countingTxns counts the update transactions that commit.
type countingTxns struct {
	kv.KV
	n int
}

func (c *countingTxns) Update(ctx context.Context, fn func(kv.Txn) error) error {
	err := c.KV.Update(ctx, fn)
	if err == nil {
		c.n++
	}
	return err
}

// RFC 8 §5.1: a stability point waits only for writes over committed
// content.
func TestOverwrites(t *testing.T) {
	f := newContentFix(t, nil)
	file := f.file(0)
	f.exist(file, write(1, 0, mib), write(2, 4*mib, mib))
	x := f.s.Existence()
	for _, c := range []struct {
		runs []entity.Extent
		want bool
	}{
		{[]entity.Extent{{Offset: mib, Length: mib}}, false},     // inside the hole
		{[]entity.Extent{{Offset: 5 * mib, Length: mib}}, false}, // past size
		{[]entity.Extent{{Offset: 2 * mib, Length: mib}, {Offset: mib / 2, Length: 1}}, true},
		{[]entity.Extent{{Offset: 4*mib - 1, Length: 2}}, true}, // straddles the hole's end
	} {
		if got, err := x.Overwrites(context.Background(), f.share, file, c.runs); err != nil || got != c.want {
			t.Errorf("%v: %v %v, want %v", c.runs, got, err, c.want)
		}
	}
	if v, err := x.Applied(context.Background(), f.share, file); err != nil || v != entity.VersionOf(2) {
		t.Errorf("applied %d %v", v.Counter(), err)
	}
}

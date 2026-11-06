package kvstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
)

func write(v uint64, off, n int64) PendingOp {
	return PendingOp{Version: entity.VersionOf(v), Kind: OpWrite, Extent: entity.Extent{Offset: off, Length: n},
		Mtime: time.Unix(int64(1_900_000_000+v), 0), Ctime: time.Unix(int64(1_900_000_000+v), 0)}
}

func grow(v uint64, size int64) PendingOp {
	return PendingOp{Version: entity.VersionOf(v), Kind: OpGrow, Size: size, Mtime: time.Unix(int64(1_900_000_000+v), 0), Ctime: time.Unix(int64(1_900_000_000+v), 0)}
}

func (f *cfix) exist(file entity.FileID, ops ...PendingOp) {
	f.t.Helper()
	failed, err := f.s.Existence().Commit(context.Background(), []PendingExistence{{Share: f.share, File: file, Ops: ops}}, nil)
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
	}, nil)
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

// RFC 8 §5.2: a file whose record does not decode fails alone; the others
// in its batch commit.
func TestExistenceCommitUndecodableRecordFailsAlone(t *testing.T) {
	f := newContentFix(t, nil)
	good, bad := f.file(0), f.file(0)
	if err := f.s.kv.Update(context.Background(), func(tx kv.Txn) error {
		return tx.Set(fileKey(f.share, bad), []byte{0xff})
	}); err != nil {
		t.Fatal(err)
	}
	failed, err := f.s.Existence().Commit(context.Background(), []PendingExistence{
		{Share: f.share, File: bad, Ops: []PendingOp{write(1, 0, mib)}},
		{Share: f.share, File: good, Ops: []PendingOp{write(2, 0, mib)}},
	}, nil)
	if err != nil || failed[bad] == nil || failed[good] != nil {
		t.Fatalf("Commit: %v %v, want only the undecodable file failed", failed, err)
	}
	if f.get(good).Size != mib {
		t.Error("the good file was not committed")
	}
}

// failGets fails a transaction's Get of key with err, the next n times, or
// every time when n is negative.
type failGets struct {
	kv.KV
	key []byte
	err error
	n   int
}

func (f *failGets) Update(ctx context.Context, fn func(kv.Txn) error) error {
	return f.KV.Update(ctx, func(tx kv.Txn) error { return fn(failGetsTxn{tx, f}) })
}

type failGetsTxn struct {
	kv.Txn
	f *failGets
}

func (t failGetsTxn) Get(key []byte) ([]byte, error) {
	if t.f.n != 0 && bytes.Equal(key, t.f.key) {
		t.f.n--
		return nil, t.f.err
	}
	return t.Txn.Get(key)
}

// A conflict reading a file's record retries the transaction rather than
// failing the file, and the retry commits the whole batch.
func TestExistenceCommitRetriesAConflictOnTheRecord(t *testing.T) {
	fg := &failGets{err: kv.ErrConflict}
	f := newContentFix(t, func(db kv.KV) kv.KV { fg.KV = db; return fg })
	a, b := f.file(0), f.file(0)
	fg.key, fg.n = fileKey(f.share, b), 1
	failed, err := f.s.Existence().Commit(context.Background(), []PendingExistence{
		{Share: f.share, File: a, Ops: []PendingOp{write(1, 0, mib)}},
		{Share: f.share, File: b, Ops: []PendingOp{write(2, 0, mib)}},
	}, nil)
	if err != nil || len(failed) != 0 {
		t.Fatalf("Commit: %v %v, want no failure", failed, err)
	}
	if fg.n != 0 {
		t.Fatal("the conflict was never injected")
	}
	if f.get(a).Size != mib || f.get(b).Size != mib {
		t.Error("the retry did not commit both files")
	}
}

// Any other store error reading a file's record fails the whole call, not
// that file alone, and commits nothing. A context error is a store stall,
// never one file's own failure.
func TestExistenceCommitFailsWholeOnAStoreError(t *testing.T) {
	for _, storeErr := range []error{kv.ErrClosed, fmt.Errorf("fdb: %w", context.DeadlineExceeded), context.Canceled} {
		t.Run(storeErr.Error(), func(t *testing.T) {
			fg := &failGets{err: storeErr}
			f := newContentFix(t, func(db kv.KV) kv.KV { fg.KV = db; return fg })
			a, b := f.file(0), f.file(0)
			fg.key, fg.n = fileKey(f.share, b), -1
			failed, err := f.s.Existence().Commit(context.Background(), []PendingExistence{
				{Share: f.share, File: a, Ops: []PendingOp{write(1, 0, mib)}},
				{Share: f.share, File: b, Ops: []PendingOp{write(2, 0, mib)}},
			}, nil)
			if !errors.Is(err, storeErr) || failed != nil {
				t.Fatalf("Commit: %v %v, want nil and the store error", failed, err)
			}
			fg.n = 0
			if f.get(a).Size != 0 {
				t.Error("a failed call committed the other file")
			}
		})
	}
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
	}, nil)
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

// RFC 6 §2.4: a hole carries the cut it was born at, and every piece cut
// from it, by a write landing inside it or a truncate across it, keeps it.
func TestHoleBornSurvivesCuts(t *testing.T) {
	f := newContentFix(t, nil)
	file := f.file(0)
	f.exist(file, write(1, 0, mib), write(2, 9*mib, mib)) // a hole over [1, 9) MiB
	if err := f.s.kv.Update(context.Background(), func(tx kv.Txn) error {
		return tx.Set(holeKey(f.share, file, mib), encodeHole(hole{Extent: entity.ExtentOf(mib, 9*mib), Version: entity.VersionOf(2), Born: 7}))
	}); err != nil {
		t.Fatal(err)
	}
	f.exist(file, write(3, 4*mib, mib)) // splits it in two
	f.remove(file, truncate(4, 7*mib))  // cuts the second piece at the new size
	var n int
	if err := f.s.kv.View(context.Background(), func(r kv.Reader) error {
		pre := holesPrefix(f.share, file)
		for row, err := range r.Scan(pre, nil) {
			if err != nil {
				return err
			}
			start, err := startOf(pre, row.Key)
			if err != nil {
				return err
			}
			h, err := decodeHole(start, row.Value)
			if err != nil {
				return err
			}
			n++
			if h.Born != 7 {
				t.Errorf("hole %v born at %d, want 7", h.Extent, h.Born)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("%d hole records, want the two split pieces, the second cut at the new size", n)
	}
}

// A write whose open suspended the modification time moves the change time
// alone: the File keeps the modification time an earlier write set.
func TestExistenceCommitSuspendedModify(t *testing.T) {
	f := newContentFix(t, nil)
	file := f.file(0)
	first := write(1, 0, mib)
	suspended := write(2, mib, mib)
	suspended.Mtime = time.Time{}
	f.exist(file, first, suspended)
	got := f.get(file)
	if !got.Modify.Equal(first.Mtime) || !got.Change.Equal(suspended.Ctime) {
		t.Errorf("modify %v, change %v; want modify %v from the first write, change %v from the suspended one", got.Modify, got.Change, first.Mtime, suspended.Ctime)
	}
}

// A lost write exists as a write does, uncarved past the old size, and
// charges nothing.
func TestExistenceCommitLost(t *testing.T) {
	f := newContentFix(t, nil)
	file := f.file(0)
	lost := write(2, mib, mib)
	lost.Kind = OpLost
	f.exist(file, write(1, 0, mib), lost)
	got := f.get(file)
	if got.Size != 2*mib || got.Charged != mib {
		t.Errorf("size %d, charged %d; want size %d, charged %d for the first write alone", got.Size, got.Charged, 2*mib, mib)
	}
	if l := f.layout(file); l != "uncarved[0,2)" {
		t.Errorf("layout %s, want uncarved[0,2)", l)
	}
}

// marksOf reads journal j's every stored sync mark, by the stream its key
// names.
func (f *cfix) marksOf(j [16]byte) map[uint32]entity.SyncMark {
	f.t.Helper()
	out := map[uint32]entity.SyncMark{}
	pre := syncMarksPrefix(j)
	if err := f.s.kv.View(context.Background(), func(r kv.Reader) error {
		for row, err := range r.Scan(pre, nil) {
			if err != nil {
				return err
			}
			s, _, err := keys.ReadUint64(row.Key[len(pre):])
			if err != nil {
				return err
			}
			m, err := decodeSyncMark(row.Value)
			if err != nil {
				return err
			}
			m.Stream = uint32(s)
			out[m.Stream] = m
		}
		return nil
	}); err != nil {
		f.t.Fatal(err)
	}
	return out
}

// byStream is marks keyed by the stream each names.
func byStream(marks ...entity.SyncMark) map[uint32]entity.SyncMark {
	out := map[uint32]entity.SyncMark{}
	for _, m := range marks {
		out[m.Stream] = m
	}
	return out
}

// RFC 6 §10.1: a commit's marks are written beside its files, or alone,
// each under the stream it names, a stream the write leaves out keeping its
// stored mark; and only while the journal's open generation is the one they
// carry, failing the files beside them too when it moved.
func TestExistenceCommitRecordsSyncMarks(t *testing.T) {
	f := newContentFix(t, nil)
	file, j := f.file(0), [16]byte{9}
	first := []entity.SyncMark{{Stream: 0, Segment: 1, Through: 100, Offset: 90, Sequence: 7}, {Stream: 1, Segment: 2, Through: 50, Offset: 40, Sequence: 3}}
	if failed, err := f.s.Existence().Commit(context.Background(), []PendingExistence{{Share: f.share, File: file, Ops: []PendingOp{write(1, 0, mib)}}},
		&MarksWrite{Journal: j, Marks: first}); err != nil || len(failed) != 0 {
		t.Fatal(failed, err)
	}
	if got := f.marksOf(j); !maps.Equal(got, byStream(first...)) {
		t.Errorf("marks beside a file: %v, want %v", got, first)
	}
	alone := entity.SyncMark{Stream: 0, Segment: 1, Through: 200, Offset: 190, Sequence: 9}
	if _, err := f.s.Existence().Commit(context.Background(), nil, &MarksWrite{Journal: j, Marks: []entity.SyncMark{alone}}); err != nil {
		t.Fatal(err)
	}
	if got, want := f.marksOf(j), byStream(alone, first[1]); !maps.Equal(got, want) {
		t.Errorf("marks alone: %v, want %v: stream 0 replaced and stream 1 kept", got, want)
	}

	if err := f.s.kv.Update(context.Background(), func(tx kv.Txn) error { return tx.Set(openGenKey(j), encodeCounter(1)) }); err != nil {
		t.Fatal(err)
	}
	stale := &MarksWrite{Journal: j, Gen: 0, Marks: []entity.SyncMark{{Segment: 1, Through: 300, Offset: 290, Sequence: 11}}}
	_, err := f.s.Existence().Commit(context.Background(), []PendingExistence{{Share: f.share, File: file, Ops: []PendingOp{write(2, 0, mib)}}}, stale)
	if !errors.Is(err, ErrOpenGenMoved) {
		t.Fatalf("a marks write of an earlier open: %v, want ErrOpenGenMoved", err)
	}
	if got, want := f.marksOf(j), byStream(alone, first[1]); !maps.Equal(got, want) {
		t.Errorf("the refused write left %v, want %v", got, want)
	}
	if got := f.get(file).Applied; got != entity.VersionOf(1) {
		t.Errorf("the file beside the refused marks is applied at %d, want 1", got.Counter())
	}
	stale.Gen = 1
	if _, err := f.s.Existence().Commit(context.Background(), nil, stale); err != nil {
		t.Errorf("a marks write of the current open: %v", err)
	}
}

// A mark is keyed by the stream it names, not its place in the set: two
// writes whose stream lists differ each land under their own streams, a
// stream the second leaves out keeps its earlier mark (RFC 16 §4.2: a marks
// write deletes nothing), and the same streams in another order land where
// they did.
func TestSyncMarksAreKeyedByStream(t *testing.T) {
	f := newContentFix(t, nil)
	j := [16]byte{7}
	put := func(marks ...entity.SyncMark) {
		t.Helper()
		if _, err := f.s.Existence().Commit(context.Background(), nil, &MarksWrite{Journal: j, Marks: marks}); err != nil {
			t.Fatal(err)
		}
	}
	a := entity.SyncMark{Stream: 2, Segment: 20, Through: 100, Offset: 90, Sequence: 5}
	b := entity.SyncMark{Stream: 5, Segment: 50, Through: 200, Offset: 190, Sequence: 6}
	c := entity.SyncMark{Stream: 9, Segment: 90, Through: 300, Offset: 290, Sequence: 7}
	put(a, b)
	b2 := entity.SyncMark{Stream: 5, Segment: 51, Through: 10, Offset: 5, Sequence: 8}
	put(c, b2)
	if got, want := f.marksOf(j), byStream(a, c, b2); !maps.Equal(got, want) {
		t.Errorf("after a second write of other streams: %v, want %v", got, want)
	}
	put(b, c)
	if got, want := f.marksOf(j), byStream(a, b, c); !maps.Equal(got, want) {
		t.Errorf("the same streams in another order: %v, want %v", got, want)
	}
}

// RFC 8 §5.2: a marks write is charged against K for the keys it sets
// alone, so streams it leaves out, whose marks it keeps, take none of the
// budget of the files beside it.
func TestAMarksWriteChargesOnlyTheKeysItSets(t *testing.T) {
	cnt := &countingTxns{}
	f := newContentFix(t, func(db kv.KV) kv.KV { cnt.KV = db; return cnt })
	f.s.SetKeyBudget(7)
	file, j := f.file(0), [16]byte{3}
	var kept []entity.SyncMark
	for i := range 6 {
		kept = append(kept, entity.SyncMark{Stream: uint32(i), Segment: uint64(i + 1)})
	}
	if _, err := f.s.Existence().Commit(context.Background(), nil, &MarksWrite{Journal: j, Marks: kept}); err != nil {
		t.Fatal(err)
	}
	cnt.n = 0
	mark := entity.SyncMark{Stream: 6, Segment: 7}
	ops := []PendingOp{write(1, 0, mib), write(2, mib, mib)}
	if failed, err := f.s.Existence().Commit(context.Background(), []PendingExistence{{Share: f.share, File: file, Ops: ops}}, &MarksWrite{Journal: j, Marks: []entity.SyncMark{mark}}); err != nil || len(failed) != 0 {
		t.Fatal(failed, err)
	}
	if cnt.n != 1 {
		t.Errorf("%d transactions for one mark and a file's two writes at K=7, want 1", cnt.n)
	}
}

// Marks that take a transaction's whole key budget still leave its first
// file the progress a fresh transaction makes.
func TestMarksLeaveTheFirstFileItsProgress(t *testing.T) {
	f := newContentFix(t, nil)
	f.s.SetKeyBudget(8)
	file := f.file(0)
	mw := &MarksWrite{}
	for i := range 8 {
		mw.Marks = append(mw.Marks, entity.SyncMark{Stream: uint32(i)})
	}
	if failed, err := f.s.Existence().Commit(context.Background(), []PendingExistence{{Share: f.share, File: file, Ops: []PendingOp{write(1, 0, mib)}}}, mw); err != nil || len(failed) != 0 {
		t.Fatal(failed, err)
	}
	if got := f.get(file).Applied; got != entity.VersionOf(1) {
		t.Errorf("applied %d, want 1", got.Counter())
	}
}

// conflictingKV answers every update, once on, a conflict its context ended
// on, as the store does once a conflict outlives the call's deadline.
type conflictingKV struct {
	kv.KV
	on *bool
}

func (c conflictingKV) Update(ctx context.Context, fn func(kv.Txn) error) error {
	if *c.on {
		return errors.Join(kv.ErrConflict, context.DeadlineExceeded)
	}
	return c.KV.Update(ctx, fn)
}

// A conflict that outlives a call of marks alone is ErrSyncMarks; beside a
// file it is the conflict, which the engine splits.
func TestAConflictOnMarksAloneIsErrSyncMarks(t *testing.T) {
	var on bool
	f := newContentFix(t, func(db kv.KV) kv.KV { return conflictingKV{db, &on} })
	file := f.file(0)
	on = true
	mw := &MarksWrite{Marks: []entity.SyncMark{{Segment: 1}}}
	if _, err := f.s.Existence().Commit(context.Background(), nil, mw); !errors.Is(err, ErrSyncMarks) || !errors.Is(err, ErrConflict) {
		t.Errorf("marks alone, conflicting: %v, want ErrSyncMarks", err)
	}
	_, err := f.s.Existence().Commit(context.Background(), []PendingExistence{{Share: f.share, File: file, Ops: []PendingOp{write(1, 0, mib)}}}, mw)
	if errors.Is(err, ErrSyncMarks) || !errors.Is(err, ErrConflict) {
		t.Errorf("marks beside a file, conflicting: %v, want the conflict alone", err)
	}
}

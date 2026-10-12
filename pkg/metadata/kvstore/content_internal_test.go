package kvstore

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/keys"
)

const mib = 1 << 20

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// cfix is one share with a namespace, on a store whose clock the test moves.
type cfix struct {
	t     *testing.T
	s     *Store
	clk   *clock
	share entity.ShareID
	ns    entity.NamespaceID
	seq   byte
}

func newContentFix(t *testing.T, wrap func(kv.KV) kv.KV) *cfix {
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	db, err := badgerkv.Open(badgerkv.Options{Clock: clk.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var store kv.KV = db
	if wrap != nil {
		store = wrap(db)
	}
	ctx := context.Background()
	if err := Init(ctx, store, uuid.New()); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	f := &cfix{t: t, s: s, clk: clk, share: newShare(t, s)}
	if err := s.kv.View(ctx, func(r kv.Reader) error {
		b, err := r.Get(namespacesKey(f.share))
		if err != nil {
			return err
		}
		n, err := decodeNamespaces(b)
		f.ns = n.Namespaces[0].Namespace
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

// file writes a regular file of the given size directly: the existence
// commit that would set it is the write path's.
func (f *cfix) file(size int64) entity.FileID {
	id := entity.NewFileID()
	rows(f.t, f.s, fileKey(f.share, id), encodeFile(entity.File{ID: id, Share: f.share, Type: entity.Regular, Number: 99, Size: size, Nlink: 1}))
	return id
}

func (f *cfix) name() entity.BlockName {
	f.seq++
	return entity.BlockName{0: 0xB1, 31: f.seq}
}

// refs reads every ref of a file, in offset order, and fails the test if
// any two overlap: refs of one file tile what is carved.
func (f *cfix) refs(file entity.FileID) []entity.ChunkRef {
	f.t.Helper()
	var out []entity.ChunkRef
	_ = f.s.kv.View(context.Background(), func(r kv.Reader) error {
		pre := refsPrefix(f.share, file)
		for row, err := range r.Scan(pre, nil) {
			if err != nil {
				return err
			}
			off, _ := startOf(pre, row.Key)
			c, err := decodeRef(file, off, row.Value)
			if err != nil {
				return err
			}
			if n := len(out); n > 0 && out[n-1].End() > c.Offset {
				f.t.Errorf("refs overlap: %+v and %+v", out[n-1], c)
			}
			out = append(out, c)
		}
		return nil
	})
	return out
}

// due moves a retired block's trash time to now.
func (f *cfix) due(name entity.BlockName) {
	f.t.Helper()
	blk, _ := f.block(name)
	old := blk.NotBefore
	blk.NotBefore = f.clk.now().Add(-time.Second)
	rows(f.t, f.s, blockKey(f.ns, name), encodeBlock(blk), retiredKey(f.ns, blk.NotBefore, name), encodeEmpty())
	if err := f.s.kv.Update(context.Background(), func(tx kv.Txn) error { return tx.Delete(retiredKey(f.ns, old, name)) }); err != nil {
		f.t.Fatal(err)
	}
}

func chunk(b byte) entity.ChunkHash { return entity.ChunkHash{0: 0xC0, 31: b} }

func ref(off, length int64, h entity.ChunkHash, oldest, newest uint64) entity.ChunkRef {
	return entity.ChunkRef{Offset: off, Length: length, Chunk: h, Oldest: entity.VersionOf(oldest), Newest: entity.VersionOf(newest)}
}

// commit records a put intent and commits a block carrying chunks, with
// refs for one file.
func (f *cfix) commit(file entity.FileID, chunks []ChunkAt, refs ...entity.ChunkRef) (entity.BlockName, CommitResult) {
	f.t.Helper()
	name := f.name()
	res, err := f.tryCommit(name, file, chunks, refs...)
	if err != nil {
		f.t.Fatal(err)
	}
	return name, res
}

func (f *cfix) tryCommit(name entity.BlockName, file entity.FileID, chunks []ChunkAt, refs ...entity.ChunkRef) (CommitResult, error) {
	ctx := context.Background()
	c := f.s.Content()
	if err := c.Intend(ctx, f.ns, name, Intent{Domain: DomainShard}); err != nil && !errors.Is(err, ErrInconsistent) {
		return nil, err
	}
	return c.Commit(ctx, BlockCommit{Namespace: f.ns, Name: name, Size: 4 * mib, Chunks: chunks,
		Files: []FileCommit{{Share: f.share, File: file, Refs: refs}}})
}

func carry(hs ...entity.ChunkHash) []ChunkAt {
	out := make([]ChunkAt, len(hs))
	for i, h := range hs {
		out[i] = ChunkAt{Hash: h, Position: int64(i) * mib, Length: mib}
	}
	return out
}

func (f *cfix) spans(file entity.FileID, off, n int64) []entity.Span {
	f.t.Helper()
	var out []entity.Span
	for s, err := range f.s.Content().Covering(context.Background(), f.share, file, off, n) {
		if err != nil {
			f.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func (f *cfix) chunkRec(h entity.ChunkHash) (chunkRecord, bool) {
	var rec chunkRecord
	var ok bool
	_ = f.s.kv.View(context.Background(), func(r kv.Reader) error {
		b, found, err := get(r, chunkKey(f.ns, h))
		if err != nil || !found {
			return err
		}
		ok = true
		rec, err = decodeChunk(h, b)
		return err
	})
	return rec, ok
}

func (f *cfix) block(name entity.BlockName) (entity.Block, bool) {
	var blk entity.Block
	var ok bool
	_ = f.s.kv.View(context.Background(), func(r kv.Reader) error {
		b, found, err := get(r, blockKey(f.ns, name))
		if err != nil || !found {
			return err
		}
		ok = true
		blk, err = decodeBlock(name, b)
		return err
	})
	return blk, ok
}

// checkCounts asserts RFC 6 §6.1 over the whole namespace: every refcount
// equals its reverse keys and the refs naming it, and every block's live is
// the chunk records naming it with a nonzero count, retired exactly at zero.
func (f *cfix) checkCounts() {
	f.t.Helper()
	ctx := context.Background()
	refsPer := map[entity.ChunkHash]int64{}
	if err := f.s.kv.View(ctx, func(r kv.Reader) error {
		pre := filesPrefix(f.share)
		for row, err := range r.Scan(pre, nil) {
			if err != nil {
				return err
			}
			rest := row.Key[len(pre)+keys.IDLen:]
			if len(rest) == 9 && rest[0] == keys.FRef {
				c, err := decodeRef(entity.FileID{}, 0, row.Value)
				if err != nil {
					return err
				}
				if !c.IsZeroRef() {
					refsPer[c.Chunk]++
				}
			}
		}
		live := map[entity.BlockName]int64{}
		cpre := []byte{keys.KindC}
		cpre = keys.ID(cpre, f.ns[:])
		for row, err := range r.Scan(cpre, nil) {
			if err != nil {
				return err
			}
			h := entity.ChunkHash(row.Key[len(cpre):])
			rec, err := decodeChunk(h, row.Value)
			if err != nil {
				return err
			}
			n, err := countReverse(r, f.ns, h)
			if err != nil {
				return err
			}
			if rec.Refcount != n || rec.Refcount != refsPer[h] {
				f.t.Errorf("chunk %x: refcount %d, reverse keys %d, refs %d", h[31], rec.Refcount, n, refsPer[h])
			}
			if rec.Refcount > 0 {
				live[rec.Block]++
			}
		}
		bpre := keys.ID([]byte{keys.KindB}, f.ns[:])
		for row, err := range r.Scan(bpre, nil) {
			if err != nil {
				return err
			}
			name := entity.BlockName(row.Key[len(bpre):])
			blk, err := decodeBlock(name, row.Value)
			if err != nil {
				return err
			}
			if blk.Live != live[name] {
				f.t.Errorf("block %x: live %d, referenced chunk records %d", name[31], blk.Live, live[name])
			}
			if (blk.Live == 0) != (blk.State != entity.BlockLive) {
				f.t.Errorf("block %x: live %d in state %v", name[31], blk.Live, blk.State)
			}
		}
		return nil
	}); err != nil {
		f.t.Fatal(err)
	}
}

func TestCoveringClasses(t *testing.T) {
	f := newContentFix(t, nil)
	file := f.file(16 * mib)
	a, b := chunk(1), chunk(2)
	f.commit(file, carry(a, b), ref(0, 4*mib, a, 1, 2), ref(10*mib, mib, b, 1, 2))
	rows(t, f.s,
		holeKey(f.share, file, 4*mib), encodeHole(hole{entity.ExtentOf(4*mib, 10*mib), entity.VersionOf(2)}),
		overwriteKey(f.share, file, mib), encodeOverwrite(overwrite{Extent: entity.ExtentOf(mib, 2*mib), Version: entity.VersionOf(3)}))
	got := f.spans(file, 0, 32*mib) // past the size: clamped to it
	want := []struct {
		e     entity.Extent
		class entity.Class
		skip  int64
	}{
		{entity.ExtentOf(0, mib), entity.Carved, 0},
		{entity.ExtentOf(mib, 2*mib), entity.Uncarved, 0},
		{entity.ExtentOf(2*mib, 4*mib), entity.Carved, 2 * mib},
		{entity.ExtentOf(4*mib, 10*mib), entity.Hole, 0},
		{entity.ExtentOf(10*mib, 11*mib), entity.Carved, 0},
		{entity.ExtentOf(11*mib, 16*mib), entity.Uncarved, 0},
	}
	if len(got) != len(want) {
		t.Fatalf("%d spans: %+v", len(got), got)
	}
	for i, w := range want {
		s := got[i]
		if s.Extent != w.e || s.Class != w.class || (s.Class == entity.Carved && (s.Ref.Skip != w.skip || s.Ref.Offset != w.e.Offset)) {
			t.Errorf("span %d: %+v, want %v %v skip %d", i, s, w.e, w.class, w.skip)
		}
	}
	if got[1].Overwrite != entity.VersionOf(3) || got[5].Overwrite != (entity.JournalVersion{}) {
		t.Error("an uncarved run made by an overwrite record does not carry its version")
	}
	// A ref at or above the overwrite's version makes the run carved again.
	c := chunk(3)
	f.commit(file, carry(c), ref(mib, mib, c, 3, 3))
	if s := f.spans(file, mib, mib); len(s) != 1 || s[0].Class != entity.Carved || s[0].Ref.Chunk != c {
		t.Errorf("after the overwrite's offload: %+v", s)
	}
	// The lookup seeks, whatever the offset: one Before per record kind.
	cnt := &kv.Counting{KV: f.s.kv}
	(&Store{kv: cnt}).Content()
	var spans []entity.Span
	_ = cnt.View(context.Background(), func(r kv.Reader) (err error) {
		spans, err = covering(r, f.share, file, entity.ExtentOf(10*mib+5, 10*mib+6))
		return err
	})
	if n := cnt.Take(); n.Befores != 3 || n.Scanned > 3 || len(spans) != 1 {
		t.Errorf("a one-byte lookup read %+v and returned %+v", n, spans)
	}
	f.checkCounts()
}

// RFC 6 §11.1's commit order row.
func TestCommitOrder(t *testing.T) {
	f := newContentFix(t, nil)
	file := f.file(4 * mib)
	a, b := chunk(1), chunk(2)
	f.commit(file, carry(b), ref(0, 4*mib, b, 4, 5))
	// An older pass's commit lands after: its ref is refused, B survives.
	_, res := f.commit(file, carry(a), ref(0, 4*mib, a, 1, 3))
	if len(res[file]) != 0 {
		t.Errorf("an older ref was reported offloaded: %v", res[file])
	}
	if s := f.spans(file, 0, 4*mib); len(s) != 1 || s[0].Ref.Chunk != b {
		t.Fatalf("after the older commit: %+v", s)
	}
	// A commit whose newest falls in B's range is already committed:
	// nothing changes, and the extent is reported offloaded.
	c := chunk(3)
	_, res = f.commit(file, carry(c), ref(0, 4*mib, c, 4, 4))
	if len(res[file]) != 1 || res[file][0] != entity.ExtentOf(0, 4*mib) {
		t.Errorf("already committed: reported %v", res[file])
	}
	if s := f.spans(file, 0, 4*mib); len(s) != 1 || s[0].Ref.Chunk != b {
		t.Fatalf("after the in-range commit: %+v", s)
	}
	f.checkCounts()
}

func TestCommitSplitsAndCounts(t *testing.T) {
	f := newContentFix(t, nil)
	file := f.file(4 * mib)
	a, c := chunk(1), chunk(2)
	k1, _ := f.commit(file, carry(a), ref(0, 4*mib, a, 1, 2))
	// An overwrite of the middle splits A's ref in two pieces.
	f.commit(file, carry(c), ref(mib, mib, c, 3, 3))
	if rec, _ := f.chunkRec(a); rec.Refcount != 2 {
		t.Errorf("A refcount %d after a split, want 2", rec.Refcount)
	}
	if blk, _ := f.block(k1); blk.Live != 1 || blk.State != entity.BlockLive {
		t.Errorf("K1 %+v", blk)
	}
	f.checkCounts()
	// Replacing all of A's bytes retires K1, into the trash.
	d := chunk(4)
	f.commit(file, carry(d), ref(0, 4*mib, d, 4, 4))
	if rec, _ := f.chunkRec(a); rec.Refcount != 0 || rec.Block != k1 {
		t.Errorf("A after replacement: %+v", rec)
	}
	blk, _ := f.block(k1)
	if blk.State != entity.BlockRetired || !blk.NotBefore.Equal(f.clk.now().Add(trashRetention)) {
		t.Errorf("K1 after its last ref: %+v", blk)
	}
	f.checkCounts()
}

// RFC 6 §11.1's two writers, one chunk list row.
func TestSecondCarrierBornDead(t *testing.T) {
	f := newContentFix(t, nil)
	f1, f2 := f.file(mib), f.file(mib)
	x := chunk(9)
	k1, _ := f.commit(f1, carry(x), ref(0, mib, x, 1, 1))
	k2, res := f.commit(f2, carry(x), ref(0, mib, x, 1, 1))
	if len(res[f2]) != 1 {
		t.Errorf("the second file's ref was not applied: %v", res)
	}
	if rec, _ := f.chunkRec(x); rec.Block != k1 || rec.Refcount != 2 {
		t.Errorf("X %+v: want owned by K1, counted twice", rec)
	}
	blk, _ := f.block(k2)
	if blk.Live != 0 || blk.State != entity.BlockRetired || !blk.NotBefore.Equal(f.clk.now()) {
		t.Errorf("K2 %+v: want born retired, due at once", blk)
	}
	var due []entity.BlockName
	for n, err := range f.s.Blocks(f.ns).Due(context.Background(), f.clk.now()) {
		if err != nil {
			t.Fatal(err)
		}
		due = append(due, n)
	}
	if len(due) != 1 || due[0] != k2 {
		t.Errorf("due %v, want K2", due)
	}
	f.checkCounts()
}

func TestCommitRefusals(t *testing.T) {
	f := newContentFix(t, nil)
	file := f.file(mib)
	a := chunk(1)
	ctx := context.Background()
	if _, err := f.s.Content().Commit(ctx, BlockCommit{Namespace: f.ns, Name: f.name(), Chunks: carry(a),
		Files: []FileCommit{{Share: f.share, File: file, Refs: []entity.ChunkRef{ref(0, mib, a, 1, 1)}}}}); !errors.Is(err, ErrNoIntent) {
		t.Errorf("commit with no intent: %v", err)
	}
	name, _ := f.commit(file, carry(a), ref(0, mib, a, 1, 1))
	if _, err := f.tryCommit(name, file, carry(a), ref(0, mib, a, 2, 2)); !errors.Is(err, ErrInconsistent) {
		t.Errorf("a second commit of one name: %v", err)
	}
	// A released file's refs are dropped, and not reported.
	gone := entity.NewFileID()
	_, res := f.commit(gone, carry(chunk(2)), ref(0, mib, chunk(2), 1, 1))
	if len(res) != 0 {
		t.Errorf("a released file's refs were applied: %v", res)
	}
	_ = f.s.kv.View(ctx, func(r kv.Reader) error {
		for row, err := range r.Scan(fileKey(f.share, gone), nil) {
			if err == nil {
				t.Errorf("a released file's commit wrote %x", row.Key)
			}
		}
		return nil
	})
	other := entity.NamespaceID{1}
	if err := f.s.Content().Intend(ctx, other, f.name(), Intent{}); err != nil {
		t.Fatal(err)
	}
	f.checkCounts()
}

// RFC 6 §3.5: an all-zero chunk is a zero ref, never stored or counted.
func TestZeroRef(t *testing.T) {
	f := newContentFix(t, nil)
	file := f.file(mib)
	f.commit(file, nil, ref(0, mib, entity.ChunkHash{}, 1, 1))
	s := f.spans(file, 0, mib)
	if len(s) != 1 || s[0].Class != entity.Carved || !s[0].Ref.IsZeroRef() {
		t.Fatalf("zero region: %+v", s)
	}
	if _, ok := f.chunkRec(entity.ChunkHash{}); ok {
		t.Error("a zero chunk has a record")
	}
	f.checkCounts()
}

// RFC 6 §11.1's verified delete and offload-never-resurrects rows, and the
// deleter's path to prune.
func TestDeleterLifecycle(t *testing.T) {
	f := newContentFix(t, nil)
	ctx := context.Background()
	f1, f2 := f.file(mib), f.file(mib)
	a, z := chunk(1), chunk(2)
	k1, _ := f.commit(f1, carry(a), ref(0, mib, a, 1, 1))
	f.commit(f1, carry(z), ref(0, mib, z, 2, 2)) // A's last ref goes: K1 retires
	blocks := f.s.Blocks(f.ns)
	if errs := blocks.MarkDeleted(ctx, []entity.BlockName{k1}); errs[0] == nil {
		t.Fatal("deleted before the trash time")
	}
	// Offload of another file carrying A repoints A to the new block and
	// leaves K1 retired.
	k3, _ := f.commit(f2, carry(a), ref(0, mib, a, 1, 1))
	if rec, _ := f.chunkRec(a); rec.Block != k3 || rec.Refcount != 1 {
		t.Errorf("A after a carrying commit: %+v", rec)
	}
	if blk, _ := f.block(k1); blk.State != entity.BlockRetired {
		t.Errorf("K1 resurrected by an offload: %+v", blk)
	}
	f.checkCounts()
	// Its trash time passes. Store time is held under the backend's clock
	// ceiling, so the test moves the block's due time rather than the clock.
	f.due(k1)
	if errs := blocks.MarkDeleted(ctx, []entity.BlockName{k1}); errs[0] != nil {
		t.Fatalf("MarkDeleted(K1): %v", errs[0])
	}
	if err := blocks.Prune(ctx, []entity.BlockName{k1}); err == nil {
		t.Error("pruned before the object's delete was recorded")
	}
	if err := blocks.Deleted(ctx, []entity.BlockName{k1}); err != nil {
		t.Fatal(err)
	}
	if err := blocks.Prune(ctx, []entity.BlockName{k1}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.block(k1); ok {
		t.Error("K1's record survived its prune")
	}
	// Pruning only its own: A's record names K3 and stays.
	if rec, ok := f.chunkRec(a); !ok || rec.Block != k3 {
		t.Errorf("A's record after K1's prune: %+v, %v", rec, ok)
	}
	f.checkCounts()
}

// RFC 6 §11.1's verified delete: a count wrongly at zero leaves the block
// retired, and the deleter's check of the reverse index refuses it.
func TestVerifiedDelete(t *testing.T) {
	f := newContentFix(t, nil)
	ctx := context.Background()
	file := f.file(mib)
	a := chunk(1)
	k1, _ := f.commit(file, carry(a), ref(0, mib, a, 1, 1))
	rec, _ := f.chunkRec(a)
	rec.Refcount = 0
	blk, _ := f.block(k1)
	blk.Live, blk.State, blk.NotBefore = 0, entity.BlockRetired, f.clk.now().Add(-time.Second)
	rows(t, f.s, chunkKey(f.ns, a), encodeChunk(rec), blockKey(f.ns, k1), encodeBlock(blk),
		retiredKey(f.ns, blk.NotBefore, k1), encodeEmpty())
	if errs := f.s.Blocks(f.ns).MarkDeleted(ctx, []entity.BlockName{k1}); !errors.Is(errs[0], ErrReferenced) {
		t.Fatalf("MarkDeleted of a referenced block: %v", errs[0])
	}
	if blk, _ := f.block(k1); blk.State != entity.BlockLive || blk.Live != 1 {
		t.Errorf("K1 after the refused delete: %+v", blk)
	}
	if rec, _ := f.chunkRec(a); rec.Refcount != 1 {
		t.Errorf("A's count after the refused delete: %d", rec.Refcount)
	}
	f.checkCounts()
}

// RFC 6 §11.1's put intent and single-node restart rows.
func TestIntents(t *testing.T) {
	f := newContentFix(t, nil)
	ctx := context.Background()
	file := f.file(mib)
	a := chunk(1)
	name := f.name()
	if err := f.s.Content().Intend(ctx, f.ns, name, Intent{Domain: DomainShard, Epoch: 41}); err != nil {
		t.Fatal(err)
	}
	gc := f.name()
	if err := f.s.Content().Intend(ctx, f.ns, gc, Intent{Domain: DomainGC, Epoch: 50}); err != nil {
		t.Fatal(err)
	}
	// A restart abandons the node's own shard intents, and no other.
	if n, err := f.s.Blocks(f.ns).AbandonShardIntents(ctx); err != nil || n != 1 {
		t.Fatalf("abandoned %d, %v", n, err)
	}
	if _, err := f.s.Content().Commit(ctx, BlockCommit{Namespace: f.ns, Name: name, Chunks: carry(a),
		Files: []FileCommit{{Share: f.share, File: file, Refs: []entity.ChunkRef{ref(0, mib, a, 1, 1)}}}}); !errors.Is(err, ErrNoIntent) {
		t.Errorf("commit after abandonment: %v", err)
	}
	blk, ok := f.block(name)
	if !ok || blk.State != entity.BlockRetired || !blk.NotBefore.Equal(f.clk.now().Add(putBound)) || len(blk.Carried) != 0 {
		t.Errorf("abandoned name's record: %+v, %v", blk, ok)
	}
	var left []entity.BlockName
	_ = f.s.kv.View(ctx, func(r kv.Reader) error {
		for row := range r.Scan(intentsPrefix(f.ns), nil) {
			left = append(left, entity.BlockName(row.Key[len(intentsPrefix(f.ns)):]))
		}
		return nil
	})
	if len(left) != 1 || left[0] != gc {
		t.Errorf("intents left %v, want the GC partition's", left)
	}
}

func TestRecountRaises(t *testing.T) {
	f := newContentFix(t, nil)
	ctx := context.Background()
	file := f.file(mib)
	a := chunk(1)
	f.commit(file, carry(a), ref(0, mib, a, 1, 1))
	rec, _ := f.chunkRec(a)
	rec.Refcount = 5 // high: a recount never lowers
	rows(t, f.s, chunkKey(f.ns, a), encodeChunk(rec))
	if err := f.s.Blocks(f.ns).Recount(ctx, a); err != nil {
		t.Fatal(err)
	}
	if rec, _ := f.chunkRec(a); rec.Refcount != 5 {
		t.Errorf("a recount lowered a count to %d", rec.Refcount)
	}
	var hs []Holder
	for h, err := range f.s.Blocks(f.ns).Holders(ctx, a) {
		if err != nil {
			t.Fatal(err)
		}
		hs = append(hs, h)
	}
	if len(hs) != 1 || hs[0].File != file || hs[0].Offset != 0 || hs[0].Share != f.share {
		t.Errorf("holders %+v", hs)
	}
}

// holdKV runs a hook once, inside the next Update, after its function and
// before its commit, so the hook's own Update commits between them.
type holdKV struct {
	kv.KV
	mu   sync.Mutex
	hook func()
}

func (h *holdKV) Update(ctx context.Context, fn func(kv.Txn) error) error {
	h.mu.Lock()
	hook := h.hook
	h.hook = nil
	h.mu.Unlock()
	return h.KV.Update(ctx, func(tx kv.Txn) error {
		err := fn(tx)
		if hook != nil {
			hook()
			hook = nil
		}
		return err
	})
}

// RFC 6 §11.1's commits-serialise-without-the-guard row: two offload
// commits of one file over overlapping extents, the older committing last,
// share no key but the file's offload fence. The fence makes the older one
// retry, find the newer ref and refuse its own; without the fence write
// both commit and their refs overlap.
func TestCommitsSerialiseOnTheFence(t *testing.T) {
	var h *holdKV
	f := newContentFix(t, func(db kv.KV) kv.KV { h = &holdKV{KV: db}; return h })
	file := f.file(4 * mib)
	older, newer := chunk(1), chunk(2)
	oldName, newName := f.name(), f.name()
	ctx := context.Background()
	for _, n := range []entity.BlockName{oldName, newName} {
		if err := f.s.Content().Intend(ctx, f.ns, n, Intent{Domain: DomainShard}); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(name entity.BlockName, h entity.ChunkHash, r entity.ChunkRef) error {
		_, err := f.s.Content().Commit(ctx, BlockCommit{Namespace: f.ns, Name: name, Chunks: carry(h),
			Files: []FileCommit{{Share: f.share, File: file, Refs: []entity.ChunkRef{r}}}})
		return err
	}
	var innerErr error
	h.mu.Lock()
	h.hook = func() { innerErr = commit(newName, newer, ref(0, 4*mib, newer, 5, 5)) }
	h.mu.Unlock()
	if err := commit(oldName, older, ref(mib, mib, older, 2, 2)); err != nil {
		t.Fatal(err)
	}
	if innerErr != nil {
		t.Fatal(innerErr)
	}
	refs := f.refs(file)
	if len(refs) != 1 || refs[0].Chunk != newer || refs[0].Extent() != entity.ExtentOf(0, 4*mib) {
		t.Fatalf("refs after both commits: %+v; want only the newer one", refs)
	}
	f.checkCounts()
}

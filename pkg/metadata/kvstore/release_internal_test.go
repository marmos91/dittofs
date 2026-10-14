package kvstore

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
)

func release(v uint64) RemoveOp {
	return RemoveOp{Version: entity.VersionOf(v), Kind: entity.RemoveRelease}
}

// unlinked turns a file into one that has lost its last entry: Nlink zero
// and its pending release recorded, as the final unlink leaves it.
func (f *cfix) unlinked(file entity.FileID) {
	f.t.Helper()
	x := f.get(file)
	x.Nlink = 0
	if err := f.s.kv.Update(context.Background(), func(tx kv.Txn) error {
		if err := tx.Set(fileKey(f.share, file), encodeFile(x)); err != nil {
			return err
		}
		return putRelease(tx, f.share, x.Shard, file)
	}); err != nil {
		f.t.Fatal(err)
	}
}

// pendingReleases is what recovery finds for the fixture's share.
func (f *cfix) pendingReleases() []entity.FileID {
	f.t.Helper()
	var ids []entity.FileID
	for id, err := range f.s.Namespace().PendingReleases(context.Background(), f.share) {
		if err != nil {
			f.t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

// keysUnder lists every key left under a file's prefix.
func (f *cfix) keysUnder(file entity.FileID) [][]byte {
	f.t.Helper()
	var out [][]byte
	_ = f.s.kv.View(context.Background(), func(r kv.Reader) error {
		for row, err := range r.Scan(fileKey(f.share, file), nil) {
			if err != nil {
				return err
			}
			out = append(out, row.Key)
		}
		return nil
	})
	return out
}

// RFC 7 §4.3 and RFC 6 §6.4: a release deletes the file's namespace
// records and hands its streams their own pending release in phase 1, drops
// every ref in phase 2, and once its removal is pruned leaves no key of the
// file behind.
func TestRelease(t *testing.T) {
	f := newContentFix(t, nil)
	ctx := context.Background()
	file, stream := f.file(0), f.file(0)
	a := chunk(1)
	f.exist(file, write(1, 0, 4*mib))
	f.commit(file, carry(a), ref(0, 4*mib, a, 1, 1))
	rows(t, f.s,
		aclKey(f.share, file), encodeACL(entity.ACL{}),
		xattrKey(f.share, file, []byte("user.k")), encodeXattr([]byte("v")),
		append(streamsPrefix(f.share, file), stream[:]...), encodeEmpty())
	f.unlinked(file)

	f.remove(file, release(2))
	for _, err := range f.s.Content().Covering(ctx, f.share, file, 0, mib) {
		if !errors.Is(err, entity.ErrStale) {
			t.Errorf("covering a released file: %v", err)
		}
	}
	if failed, err := f.s.Existence().Commit(ctx, []PendingExistence{{Share: f.share, File: file, Ops: []PendingOp{write(3, 0, 1)}}}); err != nil || !errors.Is(failed[file], entity.ErrStale) {
		t.Errorf("existence commit after release: %v %v", failed, err)
	}
	_ = f.s.kv.View(ctx, func(r kv.Reader) error {
		for _, k := range [][]byte{fileKey(f.share, file), aclKey(f.share, file), releaseKey(f.share, file),
			xattrKey(f.share, file, []byte("user.k")), append(streamsPrefix(f.share, file), stream[:]...)} {
			if _, ok, _ := get(r, k); ok {
				t.Errorf("phase 1 left %x", k)
			}
		}
		if _, ok, _ := get(r, releaseKey(f.share, stream)); !ok {
			t.Error("the stream has no pending release")
		}
		return nil
	})

	if got := f.pendingReleases(); !slices.Equal(got, []entity.FileID{stream}) {
		t.Errorf("pending releases after the release %v, want the stream's", got)
	}

	f.remove(file, release(2)) // a replay finds the file gone
	f.resume(file)
	if refs := f.refs(file); len(refs) != 0 {
		t.Errorf("refs left %+v", refs)
	}
	if rec, ok := f.chunkRec(a); ok && rec.Refcount != 0 {
		t.Errorf("refcount %d", rec.Refcount)
	}
	f.checkCounts()
	if err := f.s.Content().PruneRemovals(ctx, f.share, file, entity.VersionOf(9)); err != nil {
		t.Fatal(err)
	}
	if left := f.keysUnder(file); len(left) != 0 {
		t.Errorf("keys left after pruning: %x", left)
	}
}

// RFC 6 §6.4: a released file's leftover keys, here a directory's unfolded
// time deltas with their version-index entries, are pruned in batches
// within the key budget, and the last batch takes the removal, the fences
// and the File's own entry, at the Version its removal carries.
func TestReleasePruneBatches(t *testing.T) {
	var w *writesKV
	f := newContentFix(t, func(db kv.KV) kv.KV { w = &writesKV{KV: db}; return w })
	ctx := context.Background()
	file := f.file(0)
	f.exist(file, write(1, 0, mib))
	var kvs [][]byte
	for i := range 20 {
		unique := [32]byte{0: byte(i + 1)}
		d := delta{Modify: f.clk.now(), Change: f.clk.now(), Version: entity.VersionOf(uint64(i + 2))}
		kvs = append(kvs, append(deltasPrefix(f.share, file), unique[:]...), encodeDelta(d),
			deltaFloorKey(f.share, file, d.Version, unique), encodeEmpty())
	}
	rows(t, f.s, kvs...)
	last := f.get(file).Version
	f.unlinked(file)
	f.remove(file, release(30))
	if rs := f.removals(file); len(rs) != 1 || rs[0].FileVersion != last {
		t.Fatalf("release removal %+v, want FileVersion %d", rs, last.Counter())
	}
	f.checkFloor()
	f.resume(file)

	f.s.SetKeyBudget(10)
	w.max = 0
	if err := f.s.Content().PruneRemovals(ctx, f.share, file, entity.VersionOf(99)); err != nil {
		t.Fatal(err)
	}
	if w.max > 10 {
		t.Errorf("a prune batch wrote %d keys, budget 10", w.max)
	}
	if left := f.keysUnder(file); len(left) != 0 {
		t.Errorf("keys left after pruning: %x", left)
	}
	f.checkFloor()
}

// writesKV records the most keys one Update set or deleted.
type writesKV struct {
	kv.KV
	max int
}

func (w *writesKV) Update(ctx context.Context, fn func(kv.Txn) error) error {
	return w.KV.Update(ctx, func(tx kv.Txn) error {
		c := &writesTxn{Txn: tx}
		err := fn(c)
		w.max = max(w.max, c.n)
		return err
	})
}

type writesTxn struct {
	kv.Txn
	n int
}

func (t *writesTxn) Set(k, v []byte) error { t.n++; return t.Txn.Set(k, v) }
func (t *writesTxn) Delete(k []byte) error { t.n++; return t.Txn.Delete(k) }

// RFC 7 §4.5: a release re-checks every holder and changes nothing when
// one remains.
func TestReleaseRefusesHeld(t *testing.T) {
	f := newContentFix(t, nil)
	ctx := context.Background()
	open := func(id entity.FileID) []byte { return append(opensPrefix(f.share, id), make([]byte, 16)...) }
	for _, c := range []struct {
		name  string
		setup func(file entity.FileID)
	}{
		{"an entry", func(file entity.FileID) {
			f.unlinked(file)
			x := f.get(file)
			x.Nlink = 1
			rows(t, f.s, fileKey(f.share, file), encodeFile(x))
		}},
		{"no pending release", func(file entity.FileID) {
			f.unlinked(file)
			_ = f.s.kv.Update(ctx, func(tx kv.Txn) error { return tx.Delete(releaseKey(f.share, file)) })
		}},
		{"a durable open", func(file entity.FileID) {
			f.unlinked(file)
			rows(t, f.s, open(file), encodeEmpty())
		}},
		{"a durable open of a stream", func(file entity.FileID) {
			f.unlinked(file)
			s := entity.NewFileID()
			rows(t, f.s, append(streamsPrefix(f.share, file), s[:]...), encodeEmpty(), open(s), encodeEmpty())
		}},
	} {
		file := f.file(mib)
		c.setup(file)
		before := len(f.keysUnder(file))
		if err := f.s.Existence().Remove(ctx, f.share, file, 0, release(2), nil); !errors.Is(err, ErrHeld) {
			t.Errorf("%s: %v", c.name, err)
		}
		if after := len(f.keysUnder(file)); after != before {
			t.Errorf("%s: a refused release changed %d keys to %d", c.name, before, after)
		}
	}
}

// RFC 7 §4.5's race: an open of the unlinked file and its release, in both
// commit orders. The open guards the File record; the release range-guards
// the open records. Whichever commits second sees the other.
func TestReleaseRacesOpen(t *testing.T) {
	var h *holdKV
	f := newContentFix(t, func(db kv.KV) kv.KV { h = &holdKV{KV: db}; return h })
	ctx := context.Background()
	openKey := func(file entity.FileID) []byte { return append(opensPrefix(f.share, file), make([]byte, 16)...) }
	openFile := func(file entity.FileID) error {
		return f.s.kv.Update(ctx, func(tx kv.Txn) error {
			if _, err := fileIn(tx, f.share, file); err != nil {
				return err
			}
			return tx.Set(openKey(file), encodeEmpty())
		})
	}
	releaseFile := func(file entity.FileID) error {
		return f.s.Existence().Remove(ctx, f.share, file, 0, release(2), nil)
	}
	run := func(outer, inner func() error) (error, error) {
		var innerErr error
		h.mu.Lock()
		h.hook = func() { innerErr = inner() }
		h.mu.Unlock()
		return outer(), innerErr
	}

	file := f.file(0)
	f.unlinked(file)
	rel, op := run(func() error { return releaseFile(file) }, func() error { return openFile(file) })
	if op != nil || !errors.Is(rel, ErrHeld) {
		t.Errorf("open first: release %v, open %v", rel, op)
	}

	file = f.file(0)
	f.unlinked(file)
	op, rel = run(func() error { return openFile(file) }, func() error { return releaseFile(file) })
	if rel != nil || !errors.Is(op, entity.ErrStale) {
		t.Errorf("release first: open %v, release %v", op, rel)
	}
}

// RFC 7 §4.3's recovery reads the share's pending-release index: one row
// per pending release, however many files the share holds.
func TestPendingReleasesIndex(t *testing.T) {
	var c *kv.Counting
	f := newContentFix(t, func(db kv.KV) kv.KV { c = &kv.Counting{KV: db}; return c })
	var files []entity.FileID
	for range 200 {
		files = append(files, f.file(0))
	}
	f.unlinked(files[7])
	f.unlinked(files[150])
	c.Take()
	got := f.pendingReleases()
	if len(got) != 2 || !slices.Contains(got, files[7]) || !slices.Contains(got, files[150]) {
		t.Errorf("pending releases %v", got)
	}
	if n := c.Take().Scanned; n != 2 {
		t.Errorf("recovery scanned %d rows for 2 pending releases among 200 files", n)
	}
}

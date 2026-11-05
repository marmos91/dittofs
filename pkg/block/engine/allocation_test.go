package engine_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

const kib = 1 << 10

// committed creates a file in the unit's share and commits its existence
// straight to the store: data over [0, data), then, past it, a hole up to
// size. The journal holds none of it.
func (u *unit) committed(t *testing.T, name string, data, size int64) entity.FileID {
	t.Helper()
	_, made := u.fs.Create(t, u.fs.Admin, u.fs.Root, name, kvstore.Regular(0o644))
	v := made.Version.Counter()
	ops := []kvstore.PendingOp{{Version: entity.VersionOf(v + 1), Kind: kvstore.OpWrite, Extent: entity.Extent{Length: data}, Mtime: time.Now(), Ctime: time.Now()}}
	if size > data {
		ops = append(ops, kvstore.PendingOp{Version: entity.VersionOf(v + 2), Kind: kvstore.OpGrow, Size: size, Mtime: time.Now(), Ctime: time.Now()})
	}
	failed, err := u.fs.Store.Existence().Commit(t.Context(), []kvstore.PendingExistence{{Share: u.share.ID, File: made.ID, Ops: ops}}, nil)
	if err != nil || failed[made.ID] != nil {
		t.Fatalf("commit: %v, %v", err, failed[made.ID])
	}
	return made.ID
}

type run struct {
	off, n int64
	hole   bool
}

// allocations checks each run Allocation answers from its offset.
func (u *unit) allocations(t *testing.T, f entity.FileID, want ...run) {
	t.Helper()
	for _, w := range want {
		sp, err := u.e.Allocation(t.Context(), u.share.ID, f, w.off)
		if err != nil {
			t.Fatalf("Allocation(%d): %v", w.off, err)
		}
		hole := sp.Class == entity.Hole
		if sp.Offset != w.off || sp.Length != w.n || (w.n > 0 && hole != w.hole) {
			t.Errorf("Allocation(%d) = [%d, +%d) hole %v; want +%d hole %v", w.off, sp.Offset, sp.Length, hole, w.n, w.hole)
		}
	}
}

// noJournal is a journal handle any call of which panics.
type noJournal struct{ engine.Handle }

// Allocation answers from block metadata, never the journal: a committed
// file the journal holds nothing of is still data, and no journal call is
// made.
func TestAllocationAnswersFromMetadata(t *testing.T) {
	u := newUnitWith(t, unitConfig{
		wrap: func(engine.Handle) engine.Handle {
			return noJournal{}
		},
	})
	f := u.committed(t, "f", 8*kib, 16*kib)
	u.allocations(t, f, run{0, 8 * kib, false}, run{8 * kib, 8 * kib, true}, run{16 * kib, 0, false})
}

// A pending write over a committed hole is data, with the hole either side
// of it; a pending write past the end of file leaves a hole before it.
func TestAllocationAppliesPendingWrites(t *testing.T) {
	u := newUnit(t)
	f := u.committed(t, "f", 4*kib, 16*kib)
	if _, err := u.e.Write(t.Context(), u.share.ID, f, 6*kib, make([]byte, 2*kib), engine.WriteOpts{Done: func() {}}); err != nil {
		t.Fatal(err)
	}
	if _, err := u.e.Write(t.Context(), u.share.ID, f, 20*kib, make([]byte, kib), engine.WriteOpts{Done: func() {}}); err != nil {
		t.Fatal(err)
	}
	u.allocations(t, f,
		run{0, 4 * kib, false},
		run{4 * kib, 2 * kib, true},
		run{6 * kib, 2 * kib, false},
		run{8 * kib, 12 * kib, true},
		run{20 * kib, kib, false},
		run{21 * kib, 0, false},
	)
}

// A pending deallocate inside committed data splits its run; a pending
// truncate ends the file.
func TestAllocationAppliesDeallocateAndTruncate(t *testing.T) {
	u := newUnit(t)
	f := u.committed(t, "f", 8*kib, 8*kib)
	u.e.AddOp(f, 1, false, 2*kib, 2*kib)
	u.allocations(t, f, run{0, 2 * kib, false}, run{2 * kib, 2 * kib, true}, run{4 * kib, 4 * kib, false}, run{8 * kib, 0, false})
	u.e.AddOp(f, 2, true, 3*kib, 0)
	u.allocations(t, f, run{0, 2 * kib, false}, run{2 * kib, kib, true}, run{3 * kib, 0, false}, run{5 * kib, 0, false})
}

// A pending deallocate reaching past the end of file makes a hole that ends
// at the end of file, past which the span is empty; a truncate up fills its
// extension with a hole.
func TestAllocationEndsAtTheEndOfFile(t *testing.T) {
	u := newUnit(t)
	f := u.committed(t, "f", 16*kib, 16*kib)
	u.e.AddOp(f, 1, false, 12*kib, 18*kib)
	u.allocations(t, f, run{12 * kib, 4 * kib, true}, run{16 * kib, 0, false})

	g := u.committed(t, "g", 4*kib, 16*kib)
	u.e.AddOp(g, 1, false, 10*kib, 20*kib)
	u.allocations(t, g, run{4 * kib, 12 * kib, true}, run{16 * kib, 0, false})

	h := u.committed(t, "h", 16*kib, 16*kib)
	if _, err := u.e.Write(t.Context(), u.share.ID, h, 16*kib, make([]byte, 4*kib), engine.WriteOpts{Done: func() {}}); err != nil {
		t.Fatal(err)
	}
	u.e.AddOp(h, unitLimit, false, 10*kib, 30*kib)
	u.allocations(t, h, run{10 * kib, 10 * kib, true}, run{20 * kib, 0, false})

	k := u.committed(t, "k", 4*kib, 4*kib)
	u.e.AddOp(k, 1, true, 12*kib, 0)
	u.allocations(t, k, run{0, 4 * kib, false}, run{4 * kib, 8 * kib, true}, run{12 * kib, 0, false})
}

// failingExistence is block metadata whose Allocation answers err.
type failingExistence struct {
	engine.Existence
	err error
}

func (f failingExistence) Allocation(ctx context.Context, share entity.ShareID, file entity.FileID, off int64) (entity.Extent, bool, error) {
	return entity.Extent{}, false, f.err
}

// A store refusal crosses the content boundary as the filesystem's own: its
// ErrStale as ErrStale, its ErrDelay as ErrDelay with DelayClone, the
// context's error as itself, and anything else as ErrUnavailable.
func TestAllocationMapsTheStoresRefusals(t *testing.T) {
	for _, c := range []struct {
		err  error
		want func(error) bool
	}{
		{entity.ErrStale, func(err error) bool { return errors.Is(err, vfserr.ErrStale) }},
		{entity.ErrDelay, func(err error) bool {
			var d *vfserr.Delay
			return errors.As(err, &d) && d.Cause == vfserr.DelayClone
		}},
		{context.Canceled, func(err error) bool { return err == context.Canceled }},
		{errors.New("disk on fire"), func(err error) bool { return errors.Is(err, vfserr.ErrUnavailable) }},
	} {
		t.Run(c.err.Error(), func(t *testing.T) {
			u := newUnit(t, func(d *engine.Deps) { d.Existence = failingExistence{err: c.err} })
			if _, err := u.e.Allocation(t.Context(), u.share.ID, entity.NewFileID(), 0); !c.want(err) {
				t.Errorf("the store's %v answered %v", c.err, err)
			}
		})
	}
}

// committingExistence is block metadata whose first Allocation call first
// runs between: a commit landing between a caller's entry read and its store
// read.
type committingExistence struct {
	engine.Existence
	between func() // runs once, before the store is read
	after   func() // runs once, after the store answers
}

func (c *committingExistence) Allocation(ctx context.Context, share entity.ShareID, file entity.FileID, off int64) (entity.Extent, bool, error) {
	if b := c.between; b != nil {
		c.between = nil
		b()
	}
	run, hole, err := c.Existence.Allocation(ctx, share, file, off)
	if a := c.after; a != nil {
		c.after = nil
		a()
	}
	return run, hole, err
}

// An operation staged and committed while Allocation runs is never replayed
// over a store that already holds a later one: committed data [0, 4k) and a
// pending truncate up to 8k, then a write [2k, 10k) commits with the
// truncate before the store is read, so the file is data to 10k, a state the
// stale truncate replayed over it would cut to 8k.
func TestAllocationReadsTheEntryAfterTheStore(t *testing.T) {
	ce := &committingExistence{}
	u := newUnit(t, func(d *engine.Deps) { ce.Existence = d.Existence; d.Existence = ce })
	_, made := u.fs.Create(t, u.fs.Admin, u.fs.Root, "f", kvstore.Regular(0o644))
	f, v := made.ID, made.Version.Counter()
	if failed, err := u.fs.Store.Existence().Commit(t.Context(), []kvstore.PendingExistence{{Share: u.share.ID, File: f, Ops: []kvstore.PendingOp{
		{Version: entity.VersionOf(v + 1), Kind: kvstore.OpWrite, Extent: entity.Extent{Length: 4 * kib}, Mtime: time.Now(), Ctime: time.Now()},
	}}}, nil); err != nil || failed[f] != nil {
		t.Fatalf("commit: %v, %v", err, failed[f])
	}
	u.e.AddOp(f, v+2, true, 8*kib, 0)
	ce.between = func() {
		ops := []kvstore.PendingOp{
			{Version: entity.VersionOf(v + 2), Kind: kvstore.OpGrow, Size: 8 * kib, Mtime: time.Now(), Ctime: time.Now()},
			{Version: entity.VersionOf(v + 3), Kind: kvstore.OpWrite, Extent: entity.Extent{Offset: 2 * kib, Length: 8 * kib}, Mtime: time.Now(), Ctime: time.Now()},
		}
		if failed, err := u.fs.Store.Existence().Commit(context.Background(), []kvstore.PendingExistence{{Share: u.share.ID, File: f, Ops: ops}}, nil); err != nil || failed[f] != nil {
			t.Errorf("commit: %v, %v", err, failed[f])
		}
		u.e.ClearOps(f)
	}
	u.allocations(t, f, run{0, 10 * kib, false})
}

// An operation committing after the store answers and before the entry is
// read is in neither read, so the pass is read again: committed data
// [0, 4k), a pending write [4k, 12k); between the two reads it commits and
// leaves the entry, and a write [16k, 18k) is staged. Read as one pass,
// Allocation(4k) would answer hole [4k, 16k), a state the file never had.
func TestAllocationReadsAgainAfterACommitBetweenItsReads(t *testing.T) {
	ce := &committingExistence{}
	u := newUnit(t, func(d *engine.Deps) { ce.Existence = d.Existence; d.Existence = ce })
	_, made := u.fs.Create(t, u.fs.Admin, u.fs.Root, "f", kvstore.Regular(0o644))
	f, v := made.ID, made.Version.Counter()
	if failed, err := u.fs.Store.Existence().Commit(t.Context(), []kvstore.PendingExistence{{Share: u.share.ID, File: f, Ops: []kvstore.PendingOp{
		{Version: entity.VersionOf(v + 1), Kind: kvstore.OpWrite, Extent: entity.Extent{Length: 4 * kib}, Mtime: time.Now(), Ctime: time.Now()},
	}}}, nil); err != nil || failed[f] != nil {
		t.Fatalf("commit: %v, %v", err, failed[f])
	}
	u.e.AddWriteOp(f, v+2, 4*kib, 8*kib)
	ce.after = func() {
		ops := []kvstore.PendingOp{{Version: entity.VersionOf(v + 2), Kind: kvstore.OpWrite, Extent: entity.Extent{Offset: 4 * kib, Length: 8 * kib}, Mtime: time.Now(), Ctime: time.Now()}}
		if failed, err := u.fs.Store.Existence().Commit(context.Background(), []kvstore.PendingExistence{{Share: u.share.ID, File: f, Ops: ops}}, nil); err != nil || failed[f] != nil {
			t.Errorf("commit: %v, %v", err, failed[f])
		}
		u.e.ClearOps(f)
		u.e.AddWriteOp(f, v+3, 16*kib, 2*kib)
	}
	u.allocations(t, f, run{4 * kib, 8 * kib, false})
}

// AddPending's drop leaves the entry as a commit does, so an Allocation pass
// it cuts across is read again: committed data [0, 4k), a pending write
// [4k, 12k) committed and dropped between the two reads. Read as one pass,
// Allocation(4k) would answer the end of file at 4k.
func TestAllocationReadsAgainAfterAPendingDrop(t *testing.T) {
	ce := &committingExistence{}
	u := newUnit(t, func(d *engine.Deps) { ce.Existence = d.Existence; d.Existence = ce })
	_, made := u.fs.Create(t, u.fs.Admin, u.fs.Root, "f", kvstore.Regular(0o644))
	f, v := made.ID, made.Version.Counter()
	if failed, err := u.fs.Store.Existence().Commit(t.Context(), []kvstore.PendingExistence{{Share: u.share.ID, File: f, Ops: []kvstore.PendingOp{
		{Version: entity.VersionOf(v + 1), Kind: kvstore.OpWrite, Extent: entity.Extent{Length: 4 * kib}, Mtime: time.Now(), Ctime: time.Now()},
	}}}, nil); err != nil || failed[f] != nil {
		t.Fatalf("commit: %v, %v", err, failed[f])
	}
	drop := u.e.AddPending(f, 4*kib, 8*kib)
	ce.after = func() {
		ops := []kvstore.PendingOp{{Version: entity.VersionOf(v + 2), Kind: kvstore.OpWrite, Extent: entity.Extent{Offset: 4 * kib, Length: 8 * kib}, Mtime: time.Now(), Ctime: time.Now()}}
		if failed, err := u.fs.Store.Existence().Commit(context.Background(), []kvstore.PendingExistence{{Share: u.share.ID, File: f, Ops: ops}}, nil); err != nil || failed[f] != nil {
			t.Errorf("commit: %v, %v", err, failed[f])
		}
		drop()
	}
	u.allocations(t, f, run{4 * kib, 8 * kib, false})
}

// A group commit settling after the store answers and before the entry is
// read makes the pass read again: committed data [0, 4k), a pending write
// [4k, 12k) the group commit commits between the two reads, and a write
// [16k, 18k) staged after. Read as one pass, Allocation(4k) would answer
// hole [4k, 16k).
func TestAllocationReadsAgainAfterAGroupCommitBetweenItsReads(t *testing.T) {
	ce := &committingExistence{}
	u := newUnit(t, func(d *engine.Deps) { ce.Existence = d.Existence; d.Existence = ce })
	f := u.create(t, "f")
	u.write(t, f, 0, make([]byte, 4*kib))
	if err := u.e.CommitNow(t.Context(), u.share.ID, f); err != nil {
		t.Fatal(err)
	}
	u.write(t, f, 4*kib, make([]byte, 8*kib))
	ce.after = func() {
		if err := u.e.CommitNow(context.Background(), u.share.ID, f); err != nil {
			t.Errorf("commit: %v", err)
		}
		u.write(t, f, 16*kib, make([]byte, 2*kib))
	}
	u.allocations(t, f, run{4 * kib, 8 * kib, false})
}

// A truncate's phase 1 committing after the store answers and before the
// entry is read makes the pass read again: committed data [0, 4k), a pending
// write [4k, 12k), then a truncate to 10k commits it in its phase 1 between
// the two reads, and a write [16k, 18k) is staged after. Read as one pass,
// Allocation(4k) would answer hole [4k, 16k).
func TestAllocationReadsAgainAfterAPhase1BetweenItsReads(t *testing.T) {
	ce := &committingExistence{}
	u := newUnit(t, func(d *engine.Deps) { ce.Existence = d.Existence; d.Existence = ce })
	f := u.create(t, "f")
	u.write(t, f, 0, make([]byte, 4*kib))
	if err := u.e.CommitNow(t.Context(), u.share.ID, f); err != nil {
		t.Fatal(err)
	}
	u.write(t, f, 4*kib, make([]byte, 8*kib))
	ce.after = func() {
		if err := u.e.Truncate(context.Background(), u.share.ID, f, 10*kib); err != nil {
			t.Errorf("truncate: %v", err)
		}
		u.write(t, f, 16*kib, make([]byte, 2*kib))
	}
	u.allocations(t, f, run{4 * kib, 6 * kib, false})
}

// A write staged after a truncate applies after it, and only extends the end
// of file: a truncate to 1k then a write [3k, 8k) leave a hole [1k, 3k); a
// write [18k, 22k) then [12k, 19k) leave the file ending at 22k.
func TestAllocationAppliesAWriteAfterATruncate(t *testing.T) {
	u := newUnit(t)
	f := u.committed(t, "f", 0, 17*kib)
	u.e.AddOp(f, 1, true, kib, 0)
	u.e.AddWriteOp(f, 2, 3*kib, 5*kib)
	u.allocations(t, f, run{0, 3 * kib, true}, run{3 * kib, 5 * kib, false}, run{8 * kib, 0, false})

	g := u.committed(t, "g", 18*kib, 18*kib)
	u.e.AddWriteOp(g, 1, 18*kib, 4*kib)
	u.e.AddWriteOp(g, 2, 12*kib, 7*kib)
	u.allocations(t, g, run{12 * kib, 10 * kib, false}, run{22 * kib, 0, false})
}

// A deallocate to the end of file makes a hole to the end of file rather than
// wrapping past it.
func TestAllocationDeallocatesToTheEndOfFile(t *testing.T) {
	u := newUnit(t)
	f := u.committed(t, "f", 8*kib, 8*kib)
	u.e.AddOp(f, 1, false, 2*kib, entity.EndOfFile)
	u.allocations(t, f, run{0, 2 * kib, false}, run{2 * kib, 6 * kib, true}, run{8 * kib, 0, false})
}

// countingExistence is block metadata that counts its Allocation calls and
// ignores the context.
type countingExistence struct {
	engine.Existence
	calls int
}

func (c *countingExistence) Allocation(ctx context.Context, share entity.ShareID, file entity.FileID, off int64) (entity.Extent, bool, error) {
	c.calls++
	return c.Existence.Allocation(context.Background(), share, file, off)
}

// A cancelled call asks the store nothing and answers the context's error.
func TestAllocationStopsOnItsContext(t *testing.T) {
	ce := &countingExistence{}
	u := newUnit(t, func(d *engine.Deps) { ce.Existence = d.Existence; d.Existence = ce })
	f := u.committed(t, "f", 4*kib, 4*kib)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := u.e.Allocation(ctx, u.share.ID, f, 0); !errors.Is(err, context.Canceled) || ce.calls != 0 {
		t.Errorf("a cancelled Allocation answered %v after %d store calls; want context.Canceled and none", err, ce.calls)
	}
}

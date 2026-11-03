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
	ops := []kvstore.PendingOp{{Version: entity.VersionOf(v + 1), Kind: kvstore.OpWrite, Extent: entity.Extent{Length: data}, Time: time.Now()}}
	if size > data {
		ops = append(ops, kvstore.PendingOp{Version: entity.VersionOf(v + 2), Kind: kvstore.OpGrow, Size: size, Time: time.Now()})
	}
	failed, err := u.fs.Store.Existence().Commit(t.Context(), []kvstore.PendingExistence{{Share: u.share.ID, File: made.ID, Ops: ops}})
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
	u := newUnit(t)
	u.e.BindForTest(u.share, u.j, noJournal{})
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
	u.e.AddOp(h, 1<<20, false, 10*kib, 30*kib)
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

package openstate

import (
	"errors"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
)

// cloned runs a clone's first phase of src's first 4 KiB onto dst, as the
// engine does, and answers the clone it holds src for.
func (f *fixture) cloned(t *testing.T, src, dst FileKey) CloneSource {
	t.Helper()
	ex := f.fs.Store.Existence()
	failed, err := ex.Commit(ctx, []kvstore.PendingExistence{{Share: src.Share, File: src.ID,
		Ops: []kvstore.PendingOp{{Kind: kvstore.OpWrite, Version: entity.VersionOf(1), Extent: entity.ExtentOf(0, 4096)}}}})
	if err == nil {
		err = failed[src.ID]
	}
	if err == nil {
		err = ex.Clone(ctx, kvstore.CloneOp{Share: dst.Share, File: dst.ID, Version: entity.VersionOf(5),
			Spec: entity.CloneSpec{SrcShare: src.Share, Src: src.ID, Len: 4096}}, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return CloneSource{Dst: dst.ID, Version: entity.VersionOf(5)}
}

// lastBatch runs the clone onto dst to its end, whose last batch deletes its
// hold on the source.
func (f *fixture) lastBatch(t *testing.T, dst FileKey) {
	t.Helper()
	if err := f.fs.Store.Content().Resume(ctx, dst.Share, dst.ID); err != nil {
		t.Fatal(err)
	}
}

// A clone's hold on its source is an open of no client, held from CloneOpen
// to CloneClose: no client uses or closes it, it defers no delete, it keeps
// an unlinked source alive past its last client's close, and its close
// reports the source an orphan.
func TestCloneHoldsItsSource(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, smb("pc-a"))
	src, r := f.fileIn(t, "src")
	dst, _ := f.fileIn(t, "dst")
	c := f.cloned(t, src, dst)
	if _, err := f.Opens().CloneOpen(ctx, FileKey{Share: src.Share, Shard: 3, ID: src.ID}, c); delayedFor(err) != DelayFrozen {
		t.Errorf("CloneOpen in a shard not begun: %v, want ErrDelay (DelayFrozen)", err)
	}
	if _, err := f.Opens().CloneOpen(ctx, src, CloneSource{Dst: dst.ID, Version: entity.VersionOf(4)}); !errors.Is(err, ErrBadState) {
		t.Errorf("CloneOpen of a clone with no hold: %v, want ErrBadState", err)
	}
	id, err := f.Opens().CloneOpen(ctx, src, c)
	if got := f.stored(t, src); err != nil || len(got) != 1 || got[0].ID != id {
		t.Fatalf("CloneOpen %d, %v; stored %+v", id, err, got)
	}
	if again, err := f.Opens().CloneOpen(ctx, src, c); err != nil || again != id {
		t.Errorf("CloneOpen again %d, %v; want the same hold", again, err)
	}
	if _, err := f.Opens().Close(ctx, Caller{}, OpenRef{Kind: RefOpened, Open: id}, Seqs{}); !errors.Is(err, ErrBadState) {
		t.Errorf("a Close naming the hold: %v, want ErrBadState", err)
	}

	r.Access, r.DeleteOnClose = rd|del, true
	oa := f.mustOpen(t, a, src, r)
	if _, err := f.Opens().CloneClose(ctx, oa.ID); !errors.Is(err, ErrBadState) {
		t.Errorf("CloneClose of a client's open: %v, want ErrBadState", err)
	}
	if res := f.close(t, a, oa); res.Unlink == nil {
		t.Fatalf("the last client close of a delete-on-close source: %+v, want its unlink", res)
	}
	f.unlink(t, src, "src")
	oa = f.mustOpen(t, a, src, OpenRequest{Access: rd})
	if res := f.close(t, a, oa); res.Orphan {
		t.Error("the last client close of an unlinked source the clone holds reported an orphan")
	}
	f.lastBatch(t, dst)
	if got := f.stored(t, src); len(got) != 0 {
		t.Errorf("stored after the last batch %+v, want the hold gone", got)
	}
	if res, err := f.Opens().CloneClose(ctx, id); err != nil || !res.Orphan {
		t.Errorf("CloneClose %+v, %v; want the unlinked source reported an orphan", res, err)
	}
	if _, err := f.Opens().CloneClose(ctx, id); !errors.Is(err, ErrBadState) {
		t.Errorf("CloneClose again: %v, want ErrBadState", err)
	}
}

// A clone's hold is installed with its shard and kept through grace's end,
// held or not yet held here; only CloneClose ends it.
func TestCloneHoldOutlivesGrace(t *testing.T) {
	f := newFixture(t)
	src, _ := f.fileIn(t, "src")
	dst, _ := f.fileIn(t, "dst")
	unheld, _ := f.fileIn(t, "unheld")
	udst, _ := f.fileIn(t, "udst")
	id, err := f.Opens().CloneOpen(ctx, src, f.cloned(t, src, dst))
	if err != nil {
		t.Fatal(err)
	}
	g := f.restart(t)
	f.cloned(t, unheld, udst)
	g.clock.mu.Lock()
	g.clock.now += leasePeriod + time.Second
	g.clock.mu.Unlock()
	g.closeUnreclaimed(ctx, g.sink)
	if len(g.stored(t, src)) != 1 || len(g.stored(t, unheld)) != 1 {
		t.Fatalf("grace's end closed a clone's hold: %+v, %+v", g.stored(t, src), g.stored(t, unheld))
	}
	g.lastBatch(t, dst)
	if _, err := g.Opens().CloneClose(ctx, id); err != nil {
		t.Errorf("CloneClose after a restart: %v", err)
	}
}

// An unlink of the source between the clone's last batch and CloneClose does
// not write the hold's record back: the hold's record is the clone's, and
// nothing holds the unlinked source after CloneClose, nor after a restart.
func TestUnlinkAfterLastBatchLeavesNoHold(t *testing.T) {
	f := newFixture(t)
	src, _ := f.fileIn(t, "src")
	dst, _ := f.fileIn(t, "dst")
	id, err := f.Opens().CloneOpen(ctx, src, f.cloned(t, src, dst))
	if err != nil {
		t.Fatal(err)
	}
	f.lastBatch(t, dst)
	f.unlink(t, src, "src")
	if _, err := f.Opens().CloneClose(ctx, id); err != nil {
		t.Fatal(err)
	}
	if got := f.stored(t, src); len(got) != 0 {
		t.Errorf("the unlinked source's records after CloneClose: %+v, want none", got)
	}
	g := f.restart(t)
	g.clock.mu.Lock()
	g.clock.now += leasePeriod + time.Second
	g.clock.mu.Unlock()
	g.closeUnreclaimed(ctx, g.sink)
	if got := g.stored(t, src); len(got) != 0 {
		t.Errorf("after a restart and grace's end: %d records still hold the unlinked source", len(got))
	}
}

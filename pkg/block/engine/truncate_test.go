package engine_test

import (
	"context"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// A truncate whose phase 1 sync drops, by a failed window, the write it
// decided a removal over lands above the committed size: the store sets the
// size, phase 1 commits, and the guard is released, no write counted.
func TestATruncateOverAWriteItsSyncDropsCommits(t *testing.T) {
	u := newUnit(t)
	f := u.create(t, "a")
	if _, err := u.e.Write(t.Context(), u.share.ID, f, 0, fill(10, 'a'), engine.WriteOpts{Done: func() {}}); err != nil {
		t.Fatal(err)
	}
	u.failWindow(f, 0, 10)
	applied := u.e.PendingApplied()
	if err := u.e.Truncate(t.Context(), u.share.ID, f, 5); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if n := u.size(t, f); n != 5 {
		t.Errorf("size %d, want 5", n)
	}
	if u.e.GuardHeld(f) || len(u.e.Pending(f)) != 0 {
		t.Errorf("after it: guard %v, pending %+v; want neither", u.e.GuardHeld(f), u.e.Pending(f))
	}
	if n := u.e.PendingApplied() - applied; n != 0 {
		t.Errorf("%d operations counted applied, want none: the dropped write committed nothing", n)
	}
}

// heldWrite is the unit's handle holding each write, once the journal has
// staged it, until released: the write is admitted and not yet in its
// file's entry.
type heldWrite struct {
	engine.Handle
	staged  chan struct{}
	release chan struct{}
}

// newHeldUnit is newUnit with its share's handle a heldWrite, holding no
// write until lateRemoval sets it to.
func newHeldUnit(t *testing.T) (*unit, *heldWrite) {
	t.Helper()
	h := &heldWrite{}
	u := newUnitWith(t, unitConfig{wrap: func(x engine.Handle) engine.Handle {
		h.Handle = x
		return h
	}})
	return u, h
}

func (h *heldWrite) WriteAt(ctx context.Context, id journal.FileID, off int64, p []byte, mtime int64) (journal.Version, error) {
	v, err := h.Handle.WriteAt(ctx, id, off, p, mtime)
	if h.staged != nil {
		close(h.staged)
		<-h.release
	}
	return v, err
}

// lateRemoval runs remove while a write of p at off is staged in the
// journal and not yet in f's entry, releasing the write only once remove
// has started, and returns remove's error.
func (u *unit) lateRemoval(t *testing.T, h *heldWrite, f entity.FileID, off int64, p []byte, remove func(context.Context) error) error {
	t.Helper()
	h.staged, h.release = make(chan struct{}), make(chan struct{})
	wrote := make(chan error, 1)
	go func() {
		_, err := u.e.Write(context.Background(), u.share.ID, f, off, p, engine.WriteOpts{Done: func() {}})
		wrote <- err
	}()
	<-h.staged
	removed := make(chan error, 1)
	go func() { removed <- remove(context.Background()) }()
	time.Sleep(20 * time.Millisecond) // the removal starts and meets the admitted write
	close(h.release)
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}
	h.staged = nil
	return <-removed
}

// A truncate down meeting a write staged and not yet in the entry waits it
// out: afterwards the file reads the write's bytes below the new size, never
// lost.
func TestATruncateDownWaitsOutAnAdmittedWrite(t *testing.T) {
	u, h := newHeldUnit(t)
	f := u.create(t, "a")
	u.write(t, f, 0, fill(16, 'a'))
	u.commit(t, f)
	if err := u.lateRemoval(t, h, f, 0, fill(10, 'w'), func(ctx context.Context) error {
		return u.e.Truncate(ctx, u.share.ID, f, 3)
	}); err != nil {
		t.Fatal(err)
	}
	if err := u.e.CommitNow(t.Context(), u.share.ID, f); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 8)
	n, err := u.e.Read(t.Context(), u.share.ID, f, 0, got)
	if err != nil || string(got[:n]) != "www" {
		t.Errorf("read: %q, %v; want the write's first three bytes", got[:n], err)
	}
}

// A truncate meeting a write staged and not yet in the entry decides grow or
// removal over it: here a removal, so the commit after it completes and the
// file holds the truncate's size.
func TestATruncateDecidesOverAnAdmittedWrite(t *testing.T) {
	u, h := newHeldUnit(t)
	f := u.create(t, "a")
	if err := u.lateRemoval(t, h, f, 0, fill(10, 'w'), func(ctx context.Context) error {
		return u.e.Truncate(ctx, u.share.ID, f, 5)
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := u.e.CommitNow(ctx, u.share.ID, f); err != nil {
		t.Fatalf("the commit after the truncate: %v", err)
	}
	if n := u.size(t, f); n != 5 {
		t.Errorf("size %d, want 5", n)
	}
}

// Between a truncate down's journal step and its phase 1, the file's
// overlay already shows the new size, set.
func TestATruncatesOverlayBeforeItsPhase1(t *testing.T) {
	u := newUnit(t)
	f := u.create(t, "a")
	u.write(t, f, 0, fill(16, 'a'))
	u.commit(t, f)
	var o engine.Overlay
	var pending bool
	u.e.OnStaged(func() { o, pending, _ = u.e.Overlay(context.Background(), u.share.ID, f) })
	if err := u.e.Truncate(t.Context(), u.share.ID, f, 4); err != nil {
		t.Fatal(err)
	}
	if !pending || !o.SizeSet || o.Size != 4 {
		t.Errorf("overlay before phase 1: %+v pending %v; want size 4, set", o, pending)
	}
}

// readInTruncate is a journal handle that runs read, once set, when its
// Truncate has stopped holding the extent, before the truncate's operation
// is inserted.
type readInTruncate struct {
	engine.Handle
	read *armed
}

func (h readInTruncate) Truncate(ctx context.Context, id journal.FileID, size int64) (journal.Version, error) {
	v, err := h.Handle.Truncate(ctx, id, size)
	h.read.run()
	return v, err
}

// A read started between a truncate's journal step and the insert of its
// operation waits for the insert: it reads the truncated file, never the
// committed run past the new size as lost.
func TestAReadInATruncatesJournalStepIsNeverLost(t *testing.T) {
	read := &armed{}
	u := newUnitWith(t, unitConfig{wrap: func(h engine.Handle) engine.Handle { return readInTruncate{h, read} }})
	f := u.create(t, "a")
	u.write(t, f, 0, fill(8*kib, 'a'))
	u.commit(t, f)
	u.e.ClearOps(f)
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	read.set(func() {
		go func() {
			n, err := u.e.Read(context.Background(), u.share.ID, f, 0, make([]byte, 8*kib))
			done <- result{n, err}
		}()
		deadline := time.Now().Add(time.Second)
		for u.e.FileStateWaiting() == 0 && len(done) == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
	})
	if err := u.e.Truncate(t.Context(), u.share.ID, f, 4*kib); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.n != 4*kib || r.err != nil {
		t.Errorf("a read in the truncate's journal step: %d, %v; want a short read of 4 KiB", r.n, r.err)
	}
}

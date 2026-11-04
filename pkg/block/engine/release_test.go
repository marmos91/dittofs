package engine_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// stalling is the store's existence view whose removals, while on, hold
// until their context ends, as a store that commits nothing does.
type stalling struct {
	engine.Existence
	on atomic.Bool
}

func (x *stalling) Remove(ctx context.Context, share entity.ShareID, file entity.FileID, epoch uint64, op kvstore.RemoveOp, pending []kvstore.PendingOp) error {
	if x.on.Load() {
		<-ctx.Done()
		return ctx.Err()
	}
	return x.Existence.Remove(ctx, share, file, epoch, op, pending)
}

// unlinked creates a file with a pending write and unlinks its only name, so
// the store records its pending release.
func (u *unit) unlinked(t *testing.T, res *enginetest.Reservations) entity.FileID {
	t.Helper()
	f := u.create(t, "a")
	o, answered := res.Opts()
	_, err := u.e.Write(t.Context(), u.share.ID, f, 0, fill(8, 'a'), o)
	answered(err)
	if _, err := u.fs.Store.Namespace().Unlink(t.Context(), u.fs.Admin, u.fs.Root, []byte("a")); err != nil {
		t.Fatal(err)
	}
	return f
}

// A release whose phase 1 commits within its call answers a nil channel and
// nil; the file is gone, its writes' Dones ran, and the journal is settled.
func TestAReleaseCommitsWithinItsCall(t *testing.T) {
	u, h := newHookedUnit(t)
	res := enginetest.NewReservations(t)
	f := u.unlinked(t, res)
	ch, err := u.e.Delete(t.Context(), u.share.ID, f)
	if ch != nil || err != nil {
		t.Fatalf("Delete: %v, %v; want a nil channel and nil", ch, err)
	}
	if _, err := u.fs.Store.Existence().Size(t.Context(), u.share.ID, f); !errors.Is(err, entity.ErrStale) {
		t.Errorf("the released file's record: %v, want it gone", err)
	}
	if n := res.Outstanding(); n != 0 {
		t.Errorf("%d writes without their Done", n)
	}
	if len(h.settleLog()) != 1 {
		t.Errorf("settles %+v, want one", h.settleLog())
	}
}

// A release a holder refuses, here the file's remaining name, answers
// ErrFileOpen: the file stays, its loss sequence is raised, and its writes'
// Dones ran.
func TestAnAbortedReleaseAnswersErrFileOpen(t *testing.T) {
	u := newUnit(t)
	res := enginetest.NewReservations(t)
	f := u.create(t, "a")
	o, answered := res.Opts()
	_, err := u.e.Write(t.Context(), u.share.ID, f, 0, fill(8, 'a'), o)
	answered(err)
	seq := u.e.LossSeq(u.share.ID, f)
	ch, err := u.e.Delete(t.Context(), u.share.ID, f)
	if ch != nil || !errors.Is(err, vfserr.ErrFileOpen) {
		t.Fatalf("Delete: %v, %v; want ErrFileOpen", ch, err)
	}
	if _, err := u.fs.Store.Existence().Size(t.Context(), u.share.ID, f); err != nil {
		t.Errorf("the file after the abort: %v, want it kept", err)
	}
	if got := u.e.LossSeq(u.share.ID, f); got <= seq {
		t.Errorf("loss sequence %d after the abort, %d before: the loss is not reported", got, seq)
	}
	if n := res.Outstanding(); n != 0 {
		t.Errorf("%d writes without their Done", n)
	}
}

// A release whose store stalls past its call's deadline is pending: Delete
// answers a channel and nil, and the channel yields nil once, after the
// store recovers and the loop's retry commits, the journal settled.
func TestAStalledReleaseIsPending(t *testing.T) {
	x := &stalling{}
	u, h := newHookedUnit(t, func(d *engine.Deps) { x.Existence = d.Existence; d.Existence = x })
	res := enginetest.NewReservations(t)
	f := u.unlinked(t, res)
	x.on.Store(true)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	ch, err := u.e.Delete(ctx, u.share.ID, f)
	if ch == nil || err != nil {
		t.Fatalf("Delete: %v, %v; want a channel and nil", ch, err)
	}
	x.on.Store(false)
	var got error
	u.until(t, "the channel yields", func() bool {
		select {
		case got = <-ch:
			return true
		default:
			return false
		}
	})
	if got != nil {
		t.Errorf("the channel yields %v, want nil", got)
	}
	if len(h.settleLog()) != 1 {
		t.Errorf("settles %+v, want one", h.settleLog())
	}
	if n := res.Outstanding(); n != 0 {
		t.Errorf("%d writes without their Done", n)
	}
}

// A pending release stalled past five retries is removal_stalled; the abort
// that ends it clears it and its channel yields ErrFileOpen, not
// ErrUnavailable.
func TestAStalledReleaseEndedByAnAbort(t *testing.T) {
	x := &stalling{}
	u := newUnit(t, func(d *engine.Deps) { x.Existence = d.Existence; d.Existence = x })
	engine.SetConflictBound(10 * time.Millisecond) // each retry stalls this long, not a second
	t.Cleanup(func() { engine.SetConflictBound(time.Second) })
	f := u.create(t, "a") // linked: its name holds it, so phase 1 aborts once it runs
	u.write(t, f, 0, fill(8, 'a'))
	x.on.Store(true)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	ch, err := u.e.Delete(ctx, u.share.ID, f)
	if ch == nil || err != nil {
		t.Fatalf("Delete: %v, %v; want a channel and nil", ch, err)
	}
	u.until(t, "the release is removal_stalled", func() bool { return u.e.RemovalStalled(f) })
	x.on.Store(false)
	var got error
	u.until(t, "the channel yields", func() bool {
		select {
		case got = <-ch:
			return true
		default:
			return false
		}
	})
	if !errors.Is(got, vfserr.ErrFileOpen) {
		t.Errorf("the channel yields %v, want ErrFileOpen", got)
	}
	if u.e.RemovalStalled(f) {
		t.Error("still removal_stalled after the abort")
	}
}

// An aborted release drops only the pending writes below it: a write staged
// after the journal's Delete stays pending, its Done not run.
func TestAnAbortKeepsTheWritesAboveIt(t *testing.T) {
	u := newUnit(t)
	f := u.create(t, "a") // linked: its name aborts the release
	u.write(t, f, 0, fill(8, 'a'))
	var later atomic.Bool
	u.e.OnStaged(func() {
		_, _ = u.e.Write(context.Background(), u.share.ID, f, 8, fill(4, 'b'), engine.WriteOpts{Done: func() { later.Store(true) }})
	})
	if _, err := u.e.Delete(t.Context(), u.share.ID, f); !errors.Is(err, vfserr.ErrFileOpen) {
		t.Fatalf("Delete: %v, want ErrFileOpen", err)
	}
	if p := u.e.Pending(f); len(p) != 1 || p[0].Off != 8 {
		t.Errorf("pending after the abort %+v, want the later write alone", p)
	}
	if later.Load() {
		t.Error("the later write's Done ran")
	}
}

// A phase 1 still retried when the loop ends leaves the guard released and
// the truncate's operation out of the entry.
func TestALoopEndingDropsARetriedPhase1(t *testing.T) {
	x := &stalling{}
	u := newUnit(t, func(d *engine.Deps) { x.Existence = d.Existence; d.Existence = x })
	f := u.create(t, "a")
	u.write(t, f, 0, fill(16, 'a'))
	if err := u.e.CommitNow(t.Context(), u.share.ID, f); err != nil {
		t.Fatal(err)
	}
	x.on.Store(true)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := u.e.Truncate(ctx, u.share.ID, f, 4); err == nil {
		t.Fatal("a stalled truncate answered nil")
	}
	u.e.CloseGroupCommits()
	if u.e.GuardHeld(f) || len(u.e.Pending(f)) != 0 {
		t.Errorf("after the loop ended: guard %v, pending %+v; want neither", u.e.GuardHeld(f), u.e.Pending(f))
	}
}

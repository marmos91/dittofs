package engine_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// hookedHandle is the unit's handle with seams inside its removals and a
// log of its Settles, each with whether the file's guard was held then.
type hookedHandle struct {
	engine.Handle
	e        *engine.Engine
	truncate func() // run inside Truncate, after the journal's removal
	mu       sync.Mutex
	settles  []settled
}

type settled struct {
	v       journal.Version
	guarded bool
	pending int // the file's pending operations then
}

func (h *hookedHandle) Truncate(ctx context.Context, id journal.FileID, size int64) (journal.Version, error) {
	v, err := h.Handle.Truncate(ctx, id, size)
	if h.truncate != nil {
		h.truncate()
	}
	return v, err
}

func (h *hookedHandle) Settle(id journal.FileID, v journal.Version) {
	h.mu.Lock()
	f := entity.FileID(id.Bytes())
	h.settles = append(h.settles, settled{v, h.e.GuardHeld(f), len(h.e.Pending(f))})
	h.mu.Unlock()
	h.Handle.Settle(id, v)
}

func (h *hookedHandle) settleLog() []settled {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]settled(nil), h.settles...)
}

// hooked is c with the unit's share's handle a hookedHandle, which it
// returns: its e is set once the unit is built.
func hooked(c unitConfig) (unitConfig, *hookedHandle) {
	h := &hookedHandle{}
	c.wrap = func(x engine.Handle) engine.Handle {
		if h.Handle != nil {
			return x // another share's
		}
		h.Handle = x
		return h
	}
	return c, h
}

// newHookedUnit is newUnit with its share's handle a hookedHandle.
func newHookedUnit(t *testing.T, deps ...func(*engine.Deps)) (*unit, *hookedHandle) {
	t.Helper()
	c, h := hooked(unitConfig{deps: deps})
	u := newUnitWith(t, c)
	h.e = u.e
	return u, h
}

// removing is the store's existence view counting its removals.
type removing struct {
	engine.Existence
	removes atomic.Int32
	on      func() // run before each removal reaches the store
}

func (x *removing) Remove(ctx context.Context, share entity.ShareID, file entity.FileID, epoch uint64, op kvstore.RemoveOp, pending []kvstore.PendingOp) error {
	x.removes.Add(1)
	if x.on != nil {
		x.on()
	}
	return x.Existence.Remove(ctx, share, file, epoch, op, pending)
}

func (u *unit) size(t *testing.T, f entity.FileID) int64 {
	t.Helper()
	n, err := u.fs.Store.Existence().Size(t.Context(), u.share.ID, f)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// A truncate answers once its phase 1 committed the new size and the writes
// below it, each Done once; the journal is settled once, at the truncate's
// version, after the guard is released; and the entry keeps neither the
// truncate's operation nor the writes it committed.
func TestATruncateCommitsItsPhase1(t *testing.T) {
	u, h := newHookedUnit(t)
	res := enginetest.NewReservations(t)
	f := u.create(t, "a")
	o, answered := res.Opts()
	_, err := u.e.Write(t.Context(), u.share.ID, f, 0, fill(16, 'a'), o)
	answered(err)
	if err := u.e.Truncate(t.Context(), u.share.ID, f, 3); err != nil {
		t.Fatal(err)
	}
	if n := u.size(t, f); n != 3 {
		t.Errorf("committed size %d, want 3", n)
	}
	if n := res.Outstanding(); n != 0 {
		t.Errorf("%d writes without their Done", n)
	}
	s := h.settleLog()
	if len(s) != 1 || s[0].guarded || s[0].pending != 0 {
		t.Errorf("settles %+v, want one, after the guard's release, the entry emptied before it", s)
	}
	if p := u.e.Pending(f); len(p) != 0 {
		t.Errorf("after phase 1 the entry holds %+v, want nothing", p)
	}
}

// A write staged between the journal's truncate and phase 1 is above the
// truncate and stays pending: phase 1 commits only below it, and the write
// commits after, so the file ends the truncate's size grown by the write.
// Claiming it with the rest raises applied past the truncate, which the store
// then skips, leaving the size from before.
func TestAWriteAboveATruncateCommitsAfterIt(t *testing.T) {
	u := newUnit(t)
	f := u.create(t, "a")
	u.write(t, f, 0, fill(16, 'a'))
	u.e.OnStaged(func() { u.write(t, f, 8, fill(4, 'b')) })
	if err := u.e.Truncate(t.Context(), u.share.ID, f, 3); err != nil {
		t.Fatal(err)
	}
	u.e.OnStaged(func() {})
	if n := u.size(t, f); n != 3 {
		t.Errorf("after phase 1 the size is %d, want 3", n)
	}
	if err := u.e.CommitNow(t.Context(), u.share.ID, f); err != nil {
		t.Fatal(err)
	}
	if n := u.size(t, f); n != 12 {
		t.Errorf("after the write's commit the size is %d, want 12", n)
	}
}

// A group commit while a truncate's journal call is blocked commits the
// journal's other files and skips the truncated one, without waiting on the
// entry lock the truncate holds.
func TestAGroupCommitSkipsAGuardedFile(t *testing.T) {
	u, h := newHookedUnit(t)
	f, g := u.create(t, "f"), u.create(t, "g")
	u.write(t, f, 0, fill(8, 'f'))
	u.write(t, g, 0, fill(8, 'g'))
	if err := u.e.Apply(engine.Settings{ExistenceAge: time.Nanosecond, GroupCommitMaxFiles: 64, CloneMaxLen: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	blocked, unblock := make(chan struct{}), make(chan struct{})
	h.truncate = func() { close(blocked); <-unblock }
	done := make(chan error, 1)
	go func() { done <- u.e.Truncate(context.Background(), u.share.ID, f, 2) }()
	<-blocked
	u.clock.Advance(time.Second)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := u.e.CommitNow(ctx, u.share.ID, g); err != nil {
		t.Errorf("g's commit while f's truncate holds its entry: %v", err)
	}
	close(unblock)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := u.size(t, f); n != 2 {
		t.Errorf("f's size %d, want 2", n)
	}
}

// A truncate waits out a batch in flight on its file: its phase 1 starts
// only once the batch returned, and the batch's writes are committed once.
func TestATruncateWaitsOutABatchInFlight(t *testing.T) {
	u := newUnit(t)
	res := enginetest.NewReservations(t)
	f := u.create(t, "a")
	o, answered := res.Opts()
	_, err := u.e.Write(t.Context(), u.share.ID, f, 0, fill(16, 'a'), o)
	answered(err)
	held, release := make(chan struct{}), make(chan struct{})
	var once, freed sync.Once
	free := func() { freed.Do(func() { close(release) }) }
	defer free() // a failure below still lets the batch, and the unit's cleanup, finish
	u.e.OnCapture(func() { once.Do(func() { close(held); <-release }) })
	commit := make(chan error, 1)
	go func() { commit <- u.e.CommitNow(context.Background(), u.share.ID, f) }()
	<-held
	truncated := make(chan error, 1)
	go func() { truncated <- u.e.Truncate(context.Background(), u.share.ID, f, 4) }()
	select {
	case err := <-truncated:
		t.Fatalf("the truncate answered %v while a batch held its file", err)
	case <-time.After(100 * time.Millisecond):
	}
	free()
	for _, c := range []chan error{commit, truncated} {
		select {
		case err := <-c:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the commit or the truncate never returned once the batch was released")
		}
	}
	if n := u.size(t, f); n != 4 {
		t.Errorf("size %d, want 4", n)
	}
	if n := res.Outstanding(); n != 0 {
		t.Errorf("%d writes without their Done", n)
	}
}

// A clone registering the extent of a truncate in flight waits until the
// truncate's phase 1 committed.
func TestACloneWaitsOutATruncateInFlight(t *testing.T) {
	x := &removing{}
	u := cloneUnit(t, func(d *engine.Deps) { x.Existence = d.Existence; d.Existence = x })
	f, dst := u.create(t, "src"), u.create(t, "dst")
	u.write(t, f, 0, fill(16, 'a'))
	var phase1 atomic.Bool
	u.e.SetAfterRegister(func() {
		if !phase1.Load() {
			t.Error("the clone registered before the truncate's phase 1")
		}
	})
	cloned := make(chan error, 1)
	u.e.OnStaged(func() {
		go func() {
			cloned <- u.e.Clone(context.Background(), u.share.ID, f, u.share.ID, dst, vfserr.Extent{SrcOff: 0, DstOff: 0, Len: 8})
		}()
		time.Sleep(50 * time.Millisecond)
	})
	x.on = func() { phase1.Store(true) }
	if err := u.e.Truncate(t.Context(), u.share.ID, f, 4); err != nil {
		t.Fatal(err)
	}
	if err := <-cloned; err != engine.ErrUnbuilt {
		t.Errorf("clone: %v, want the unbuilt answer", err)
	}
}

// A truncate into an extent a clone registered, its source's or anywhere in
// its destination, answers DelayClone with nothing removed.
func TestATruncateIntoARegistrationIsDelayClone(t *testing.T) {
	for _, onDst := range []bool{false, true} {
		t.Run(map[bool]string{false: "source", true: "destination"}[onDst], func(t *testing.T) {
			u := cloneUnit(t)
			src, dst := u.create(t, "src"), u.create(t, "dst")
			in, out := make(chan struct{}), make(chan struct{})
			u.e.SetAfterRegister(func() { close(in); <-out })
			go func() {
				_ = u.e.Clone(context.Background(), u.share.ID, src, u.share.ID, dst, vfserr.Extent{SrcOff: 0, DstOff: 1 << 20, Len: 8})
			}()
			<-in
			f := src
			if onDst {
				f = dst
			}
			if err := u.e.Truncate(t.Context(), u.share.ID, f, 4); !isDelay(err, vfserr.DelayClone) {
				t.Errorf("truncate: %v, want ErrDelay(DelayClone)", err)
			}
			close(out)
		})
	}
}

// A truncate at or above the size the file's operations leave is a grow: no
// phase 1, and the group commit commits the new size.
func TestATruncateUpIsAGrow(t *testing.T) {
	x := &removing{}
	u := newUnit(t, func(d *engine.Deps) { x.Existence = d.Existence; d.Existence = x })
	f := u.create(t, "a")
	u.write(t, f, 0, fill(4, 'a'))
	if err := u.e.Truncate(t.Context(), u.share.ID, f, 100); err != nil {
		t.Fatal(err)
	}
	if n := x.removes.Load(); n != 0 {
		t.Errorf("a truncate up made %d removals", n)
	}
	if err := u.e.CommitNow(t.Context(), u.share.ID, f); err != nil {
		t.Fatal(err)
	}
	if n := u.size(t, f); n != 100 {
		t.Errorf("size %d, want 100", n)
	}
}

// A Commit on a file whose truncate is in flight waits for the truncate
// rather than asking batch after batch, each of which skips the file.
func TestACommitWaitsOutATruncateInFlight(t *testing.T) {
	u := newUnit(t)
	f := u.create(t, "a")
	u.write(t, f, 0, fill(8, 'a'))
	var takes atomic.Int32
	u.e.OnTake(func() { takes.Add(1) })
	blocked, unblock := make(chan struct{}), make(chan struct{})
	// Held between the journal's truncate and phase 1: the guard is held and
	// the entry's lock is free.
	u.e.OnStaged(func() { close(blocked); <-unblock })
	done := make(chan error, 1)
	go func() { done <- u.e.Truncate(context.Background(), u.share.ID, f, 2) }()
	<-blocked
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_ = u.e.CommitNow(ctx, u.share.ID, f)
	close(unblock)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := takes.Load(); n > 2 {
		t.Errorf("%d batches took the guarded file while its Commit waited, want at most 2", n)
	}
}

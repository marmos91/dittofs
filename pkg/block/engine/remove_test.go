package engine_test

import (
	"context"
	"errors"
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
	writes   atomic.Int32
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

func (h *hookedHandle) WriteAt(ctx context.Context, id journal.FileID, off int64, p []byte, mtime int64) (journal.Version, error) {
	h.writes.Add(1)
	return h.Handle.WriteAt(ctx, id, off, p, mtime)
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

// A deallocate's phase 1 records the extent as one hole at once, stages no
// zeros through the journal's write, and a write into the extent after it
// reads back.
func TestADeallocateRecordsItsHoleAtOnce(t *testing.T) {
	u, h := newHookedUnit(t)
	f := u.create(t, "a")
	u.write(t, f, 0, fill(16, 'a'))
	if err := u.e.CommitNow(t.Context(), u.share.ID, f); err != nil {
		t.Fatal(err)
	}
	h.writes.Store(0)
	if err := u.e.Deallocate(t.Context(), u.share.ID, f, 4, 8); err != nil {
		t.Fatal(err)
	}
	if n := h.writes.Load(); n != 0 {
		t.Errorf("a deallocate made %d journal writes", n)
	}
	run, hole, err := u.fs.Store.Existence().Allocation(t.Context(), u.share.ID, f, 4)
	if err != nil || !hole || run != (entity.Extent{Offset: 4, Length: 8}) {
		t.Errorf("at 4 after phase 1: %+v hole %v, %v; want the hole [4, 12)", run, hole, err)
	}
	u.write(t, f, 6, []byte("bb"))
	if err := u.e.CommitNow(t.Context(), u.share.ID, f); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := u.e.Read(t.Context(), u.share.ID, f, 4, got); err != nil || string(got) != "\x00\x00bb" {
		t.Errorf("read [4, 8): %q, %v; want two zeros then the write", got, err)
	}
}

// conflictingRemove is the store's existence view answering every removal a
// conflict while on, counting the removals it answered.
type conflictingRemove struct {
	engine.Existence
	on    atomic.Bool
	calls atomic.Int32
}

func (x *conflictingRemove) Remove(ctx context.Context, share entity.ShareID, file entity.FileID, epoch uint64, op kvstore.RemoveOp, pending []kvstore.PendingOp) error {
	x.calls.Add(1)
	if x.on.Load() {
		return kvstore.ErrConflict
	}
	return x.Existence.Remove(ctx, share, file, epoch, op, pending)
}

// retriedTruncate truncates f to size with its phase 1 conflictingRemove, staging
// a write of [off, off+n) above it before phase 1, and returns once the loop
// retried phase 1 at least once, with the truncate's version.
func (u *unit) retriedTruncate(t *testing.T, x *conflictingRemove, f entity.FileID, size, off int64, n int) journal.Version {
	t.Helper()
	x.on.Store(true)
	u.e.OnStaged(func() { u.write(t, f, off, fill(n, 'b')) })
	var d *vfserr.Delay
	if err := u.e.Truncate(t.Context(), u.share.ID, f, size); !errors.As(err, &d) || d.Cause != vfserr.DelayConflict {
		t.Fatalf("a conflictingRemove truncate: %v, want ErrDelay(DelayConflict)", err)
	}
	u.e.OnStaged(func() {})
	u.until(t, "the loop retrying phase 1", func() bool { return x.calls.Load() >= 2 })
	p := u.e.Pending(f)
	if len(p) < 2 || p[len(p)-2].Len != 0 {
		t.Fatalf("pending %+v, want the truncate and the write above it last", p)
	}
	return p[len(p)-2].Version
}

// A write staged above a truncate whose phase 1 conflicts and retries is
// committed by no group commit, the guard's skip removed: the capture takes
// the write below the truncate and stops at the truncate, so the file's
// applied and every Settle of it stay below the truncate's version until
// phase 1 commits, and the write above commits after. A capture claiming
// past the truncate commits the write above first, and the store then skips
// the truncate as applied.
func TestAGroupCommitStopsAtARetriedRemoval(t *testing.T) {
	x := &conflictingRemove{}
	u, h := newHookedUnit(t, func(d *engine.Deps) { x.Existence = d.Existence; d.Existence = x })
	u.e.NoGuardSkip()
	f := u.create(t, "a")
	u.write(t, f, 0, fill(16, 'a')) // due, and below the truncate
	tv := u.retriedTruncate(t, x, f, 3, 8, 4)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	_ = u.e.CommitAll(ctx, u.rec)
	cancel()
	if n := u.size(t, f); n != 16 {
		t.Errorf("the group commit left the size %d, want the write below the truncate's 16", n)
	}
	applied, err := u.fs.Store.Existence().Applied(t.Context(), u.share.ID, f)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Compare(entity.JournalVersion(tv.Bytes())) >= 0 {
		t.Errorf("applied %v while the truncate at %v retries phase 1", applied, tv)
	}
	for _, s := range h.settleLog() {
		if s.v.Compare(tv) >= 0 {
			t.Errorf("settled at %v while the truncate at %v retries phase 1", s.v, tv)
		}
	}
	x.on.Store(false)
	u.until(t, "phase 1 committing", func() bool { return !u.e.GuardHeld(f) })
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

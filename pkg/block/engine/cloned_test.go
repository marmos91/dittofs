package engine_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// clonedUnit is a cloneUnit whose clones answer once their phase 1 commits,
// a test's stand-in for the hold, phase 2 and done signal not built.
func clonedUnit(t *testing.T, opts ...func(*engine.Deps)) *unit {
	t.Helper()
	return clonedUnitWith(t, unitConfig{deps: opts})
}

// clonedUnitWith is clonedUnit as c asks.
func clonedUnitWith(t *testing.T, c unitConfig) *unit {
	t.Helper()
	u := cloneUnitWith(t, c)
	u.e.FakeCloneDone()
	return u
}

// committedFile creates a file holding p at 0, its existence committed.
func (u *unit) committedFile(t *testing.T, name string, p []byte) entity.FileID {
	t.Helper()
	f := u.create(t, name)
	u.write(t, f, 0, p)
	if err := u.e.CommitNow(t.Context(), u.share.ID, f); err != nil {
		t.Fatal(err)
	}
	return f
}

func (u *unit) read(t *testing.T, f entity.FileID, off int64, n int) []byte {
	t.Helper()
	p := make([]byte, n)
	if _, err := u.e.Read(t.Context(), u.share.ID, f, off, p); err != nil {
		t.Fatalf("read [%d, %d): %v", off, off+int64(n), err)
	}
	return p
}

// clone runs a clone of x and fails the test on its error.
func (u *unit) clone(t *testing.T, src, dst entity.FileID, x vfserr.Extent) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := u.e.Clone(ctx, u.share.ID, src, u.share.ID, dst, x); err != nil {
		t.Fatalf("Clone: %v", err)
	}
}

// heldBeforePhase1 starts a clone of x and returns once it is held between
// its copies and its phase 1, with the function that lets it go on and the
// channel of its answer.
func (u *unit) heldBeforePhase1(t *testing.T, src, dst entity.FileID, x vfserr.Extent) (release func(), answer <-chan error) {
	t.Helper()
	held, hold := make(chan struct{}), make(chan struct{})
	var once atomic.Bool
	u.e.OnStaged(func() {
		if once.CompareAndSwap(false, true) {
			close(held)
			<-hold
		}
	})
	errc := make(chan error, 1)
	go func() { errc <- u.e.Clone(context.Background(), u.share.ID, src, u.share.ID, dst, x) }()
	select {
	case <-held:
	case err := <-errc:
		t.Fatalf("the clone answered %v before its phase 1", err)
	}
	var released sync.Once
	t.Cleanup(func() { released.Do(func() { close(hold) }) })
	return func() { released.Do(func() { close(hold) }) }, errc
}

// A reservation the journal refuses fails the clone with the mapped error
// before its journal step: both registrations are gone, the destination
// reads its prior content, and the journal saw no CloneTarget.
func TestARefusedReservationChangesNothing(t *testing.T) {
	u := clonedUnit(t)
	src := u.committedFile(t, "src", fill(2*mib, 's'))
	dst := u.committedFile(t, "dst", fill(2*mib, 'd'))
	u.j.RefuseReserve(&journal.NoSpaceError{Limit: journal.LimitShare, Missing: 1, Unit: journal.UnitBytes})
	err := u.e.Clone(t.Context(), u.share.ID, src, u.share.ID, dst, vfserr.Extent{Len: 2 * mib})
	if !isDelay(err, vfserr.DelayCapacity) {
		t.Fatalf("a refused reservation: %v, want DelayCapacity", err)
	}
	if n := len(u.j.CloneTargets()); n != 0 {
		t.Errorf("%d CloneTarget calls after a refused reservation, want none", n)
	}
	if s, d := u.e.Registrations(src); s+d != 0 {
		t.Errorf("registrations left on the source: %d, %d", s, d)
	}
	if _, err := u.e.Write(t.Context(), u.share.ID, src, 0, []byte("x"), engine.WriteOpts{Done: func() {}}); err != nil {
		t.Errorf("a write into the source extent after the refusal: %v, want served", err)
	}
	if got := u.read(t, dst, 0, 2*mib); !bytes.Equal(got, fill(2*mib, 'd')) {
		t.Error("the destination lost its prior content to a refused clone")
	}
}

// A source extent past the source's end is refused ErrInvalid before the
// journal step: no CloneTarget, the destination unchanged.
func TestACloneOfASourcePastItsEndIsRefused(t *testing.T) {
	u := clonedUnit(t)
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.committedFile(t, "dst", fill(2*mib, 'd'))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	err := u.e.Clone(ctx, u.share.ID, src, u.share.ID, dst, vfserr.Extent{Len: 2 * mib})
	if !errors.Is(err, vfserr.ErrInvalid) {
		t.Fatalf("a clone past the source's end: %v, want ErrInvalid", err)
	}
	if n := len(u.j.CloneTargets()); n != 0 {
		t.Errorf("%d CloneTarget calls, want none", n)
	}
	if got := u.read(t, dst, 0, 2*mib); !bytes.Equal(got, fill(2*mib, 'd')) {
		t.Error("the destination lost its prior content to a refused clone")
	}
}

// The reservation is sized from the source's held runs at asOf, and every
// copy goes through it above the clone's version: three held runs and two
// holes reserve the runs' bytes and three writes. Phase 1 commits the copies
// through Copied: the destination's applied is at or above the newest copy,
// and the copied runs read the source.
func TestACloneReservesItsCopiesAndWritesThemAboveItsVersion(t *testing.T) {
	u := clonedUnit(t)
	src := u.create(t, "src")
	u.write(t, src, 0, fill(mib, 'a'))
	u.write(t, src, 2*mib, fill(mib, 'b'))
	u.write(t, src, 4*mib, fill(mib/2, 'c'))
	dst := u.create(t, "dst")
	u.clone(t, src, dst, vfserr.Extent{Len: 4*mib + mib/2})

	if r := u.j.Reserves(); len(r) != 1 || r[0].N != 2*mib+mib/2 || r[0].Writes != 3 {
		t.Errorf("reserved %+v, want one of %d bytes and 3 writes", r, 2*mib+mib/2)
	}
	ct := u.j.CloneTargets()
	if len(ct) != 1 {
		t.Fatalf("%d CloneTarget calls, want one", len(ct))
	}
	copies := u.j.ReservedWrites()
	if len(copies) != 3 {
		t.Fatalf("%d copies through the reservation, want 3", len(copies))
	}
	newest := copies[0]
	for _, v := range copies {
		if v.Compare(ct[0].Version) <= 0 {
			t.Errorf("a copy at %v, at or below the clone's %v", v, ct[0].Version)
		}
		if newest.Compare(v) < 0 {
			newest = v
		}
	}
	applied, err := u.fs.Store.Existence().Applied(t.Context(), u.share.ID, dst)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Less(entity.JournalVersion(newest.Bytes())) {
		t.Errorf("the destination's applied %v is below the newest copy %v", applied, newest)
	}
	if got := u.read(t, dst, 2*mib, mib); !bytes.Equal(got, fill(mib, 'b')) {
		t.Error("a copied run of the destination does not read the source")
	}
	if len(u.e.Pending(dst)) != 0 {
		t.Errorf("the destination's entry kept %+v after phase 1, want nothing", u.e.Pending(dst))
	}
	for c, err := range u.h.Since(t.Context(), journal.NewFileID(dst), journal.Version{}) {
		if err != nil {
			t.Fatal(err)
		}
		if c.Kind == journal.ChangeCloneTarget {
			t.Errorf("the clone's journal marker at %v is still unsettled after phase 1", c.Version)
		}
	}
}

// While the clone is held between its copies and its phase 1, the
// destination's guard keeps its operations out of every batch: a group
// commit asked for then commits none of them, and the clone then commits.
func TestTheGuardKeepsACloneOutOfEveryBatch(t *testing.T) {
	u := clonedUnit(t)
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	u.write(t, dst, 4*mib, fill(10, 'w')) // below the clone, outside its extent
	release, answer := u.heldBeforePhase1(t, src, dst, vfserr.Extent{Len: mib})
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	_ = u.e.CommitAll(ctx, u.rec)
	cancel()
	if applied, _ := u.fs.Store.Existence().Applied(t.Context(), u.share.ID, dst); !applied.IsZero() {
		t.Errorf("a batch committed the destination's write at %v while the clone held its guard", applied)
	}
	release()
	if err := <-answer; err != nil {
		t.Fatalf("the clone: %v", err)
	}
	if got := u.read(t, dst, 4*mib, 10); !bytes.Equal(got, fill(10, 'w')) {
		t.Error("the destination's earlier write did not survive the clone")
	}
}

// The clone's journal step raises the destination's overlay to the extent's
// end. A write to the destination outside the extent, staged after the
// journal step and before phase 1, growing the file past a hole, reads back after
// the clone, and the store records its size and the hole: phase 1 applies
// it after the clone, so no later commit skips it.
func TestAWriteOutsideTheExtentDuringACloneSurvives(t *testing.T) {
	u := clonedUnit(t)
	src := u.committedFile(t, "src", fill(2*mib, 's'))
	if err := u.e.Deallocate(t.Context(), u.share.ID, src, mib, mib); err != nil {
		t.Fatal(err)
	}
	dst := u.create(t, "dst")
	release, answer := u.heldBeforePhase1(t, src, dst, vfserr.Extent{Len: 2 * mib})
	if o, _, err := u.e.Overlay(t.Context(), u.share.ID, dst); err != nil || o.Size != 2*mib {
		t.Errorf("the empty destination's overlay during the clone: %+v, %v; want the extent's end", o, err)
	}
	if _, err := u.e.Write(t.Context(), u.share.ID, dst, 3*mib, fill(10, 'w'), engine.WriteOpts{Done: func() {}}); err != nil {
		t.Fatalf("a write outside the extent during the clone: %v", err)
	}
	release()
	if err := <-answer; err != nil {
		t.Fatalf("the clone: %v", err)
	}
	if got := u.read(t, dst, 3*mib, 10); !bytes.Equal(got, fill(10, 'w')) {
		t.Error("the write outside the extent did not read back after the clone")
	}
	size, err := u.fs.Store.Existence().Size(t.Context(), u.share.ID, dst)
	if err != nil || size != 3*mib+10 {
		t.Errorf("the destination's committed size: %d, %v; want %d", size, err, 3*mib+10)
	}
	if err := u.e.CommitNow(t.Context(), u.share.ID, dst); err != nil {
		t.Fatal(err)
	}
	if again, _ := u.fs.Store.Existence().Size(t.Context(), u.share.ID, dst); again != size {
		t.Errorf("a later commit changed the size to %d", again)
	}
}

// After phase 1 over the real store, the source holds an open record of the
// clone kind naming the destination and the clone's version, and the
// destination's covering lookup over the extent waits for the clone to be
// done.
func TestPhase1HoldsTheSourceAndHoldsTheDestinationExtent(t *testing.T) {
	u := clonedUnit(t)
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	u.clone(t, src, dst, vfserr.Extent{Len: mib})
	v := u.j.CloneTargets()[0].Version
	opens, err := u.fs.Store.Opens().Of(t.Context(), u.share.ID, src)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, o := range opens {
		if o.Kind == entity.OpenCloneSource && o.CloneDst == dst && o.CloneVersion == entity.JournalVersion(v.Bytes()) {
			found = true
		}
	}
	if !found {
		t.Errorf("the source's opens after phase 1: %+v; want one of the clone kind naming the destination at %v", opens, v)
	}
	for _, err := range u.fs.Store.Content().Covering(t.Context(), u.share.ID, dst, 0, mib) {
		if !errors.Is(err, entity.ErrDelay) {
			t.Errorf("the destination extent served before the clone is done: %v", err)
		}
		break
	}
}

// A source run the journal lost after its existence committed is copied as
// far as the journal holds it: the destination reads the source around it,
// and lost there, never zeros, once phase 2 adopts the source.
func TestAnUncarvedSourceRunReadsLostInTheDestination(t *testing.T) {
	u := clonedUnit(t)
	src := u.committedFile(t, "src", fill(3*mib, 's'))
	u.lose(src, mib, mib)
	dst := u.create(t, "dst")
	u.clone(t, src, dst, vfserr.Extent{Len: 3 * mib})
	if err := u.fs.Store.Content().Resume(t.Context(), u.share.ID, dst); err != nil {
		t.Fatal(err)
	}
	if got := u.read(t, dst, 0, mib); !bytes.Equal(got, fill(mib, 's')) {
		t.Error("the destination does not read the source before the lost run")
	}
	if got := u.read(t, dst, 2*mib, mib); !bytes.Equal(got, fill(mib, 's')) {
		t.Error("the destination does not read the source after the lost run")
	}
	_, err := u.e.Read(t.Context(), u.share.ID, dst, mib, make([]byte, mib))
	if !errors.Is(err, vfserr.ErrLost) {
		t.Errorf("the lost run in the destination: %v, want ErrLost", err)
	}
}

// deletes counts the journal's Delete calls.
type deletes struct {
	engine.Handle
	n atomic.Int64
}

func (h *deletes) Delete(ctx context.Context, id journal.FileID) (journal.Version, error) {
	h.n.Add(1)
	return h.Handle.Delete(ctx, id)
}

// A release of the source while a clone holds it is pending before the
// journal's Delete, which it never reaches; its channel yields DelayClone
// once the clone is done, and nothing after.
func TestAReleaseOfARegisteredSourceIsPending(t *testing.T) {
	d := &deletes{}
	u := clonedUnitWith(t, unitConfig{wrap: func(h engine.Handle) engine.Handle {
		d.Handle = h
		return d
	}})
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	release, answer := u.heldBeforePhase1(t, src, dst, vfserr.Extent{Len: mib})
	ch, err := u.e.Delete(t.Context(), u.share.ID, src)
	if err != nil || ch == nil {
		t.Fatalf("a release of a registered source: %v, %v; want a pending channel", ch, err)
	}
	ran := u.e.LoopSteps(u.share.ID)
	u.until(t, "the pending release's item running once more", func() bool { return u.e.LoopSteps(u.share.ID) > ran })
	select {
	case early := <-ch:
		t.Fatalf("the pending release answered %v while the clone still held the source", early)
	default:
	}
	release()
	if err := <-answer; err != nil {
		t.Fatalf("the clone: %v", err)
	}
	var got error
	u.until(t, "the pending release's outcome", func() bool {
		select {
		case got = <-ch:
			return true
		default:
			return false
		}
	})
	if !isDelay(got, vfserr.DelayClone) {
		t.Errorf("the pending release's outcome: %v, want DelayClone", got)
	}
	if n := d.n.Load(); n != 0 {
		t.Errorf("the journal saw %d Delete calls for a release behind a clone, want none", n)
	}
	u.clock.Advance(10 * time.Second)
	time.Sleep(10 * time.Millisecond)
	select {
	case again := <-ch:
		t.Errorf("the channel answered twice: %v", again)
	default:
	}
}

// refusingClone is the store's existence view refusing its clones' phase 1
// n times, then passing them on.
type refusingClone struct {
	engine.Existence
	left  atomic.Int64
	calls atomic.Int64
	err   error // the failure; a store refusal when nil
	// entered and hold, when set, hold the second call until hold closes,
	// closing entered once it is held.
	entered, hold chan struct{}
}

func (x *refusingClone) Clone(ctx context.Context, op kvstore.CloneOp, pending []kvstore.PendingOp) error {
	if x.calls.Add(1) == 2 && x.hold != nil {
		close(x.entered)
		<-x.hold
	}
	if x.left.Add(-1) >= 0 {
		if x.err != nil {
			return x.err
		}
		// A store refusal, as the store answers one: a clone of nothing.
		return x.Existence.Clone(ctx, kvstore.CloneOp{}, nil)
	}
	return x.Existence.Clone(ctx, op, pending)
}

// A phase 1 refused after the journal step is not a failure: the clone is
// logged at Error and retried, and completes on the next attempt.
func TestARefusedClonePhase1IsRetried(t *testing.T) {
	x := &refusingClone{}
	x.left.Store(1)
	var logged lockedBuffer
	u := clonedUnit(t, func(d *engine.Deps) {
		x.Existence = d.Existence
		d.Existence = x
		d.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	errc := make(chan error, 1)
	go func() {
		errc <- u.e.Clone(context.Background(), u.share.ID, src, u.share.ID, dst, vfserr.Extent{Len: mib})
	}()
	var got error
	u.until(t, "the retried clone's answer", func() bool {
		select {
		case got = <-errc:
			return true
		default:
			return false
		}
	})
	if got != nil {
		t.Fatalf("a clone refused once at phase 1: %v, want it retried to success", got)
	}
	if n := strings.Count(logged.String(), "phase 1 was refused"); n != 1 {
		t.Errorf("the refusal was logged %d times, want once", n)
	}
	if !strings.Contains(logged.String(), "level=ERROR") {
		t.Error("the refusal was not logged at Error")
	}
}

// Six refusals in a row raise removal_stalled for the destination, keep its
// guard, so a group commit between two attempts captures none of its
// operations, and block no other file's commit on the journal.
func TestARefusedClonePinsItsGuardAndNothingElse(t *testing.T) {
	x := &refusingClone{}
	x.left.Store(6)
	u := clonedUnit(t, func(d *engine.Deps) { x.Existence = d.Existence; d.Existence = x })
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	u.write(t, dst, 4*mib, fill(10, 'w'))
	errc := make(chan error, 1)
	go func() {
		errc <- u.e.Clone(context.Background(), u.share.ID, src, u.share.ID, dst, vfserr.Extent{Len: mib})
	}()
	u.until(t, "removal_stalled for the destination", func() bool { return u.e.RemovalStalled(dst) })
	other := u.create(t, "other")
	u.write(t, other, 0, fill(10, 'o'))
	if err := u.e.CommitNow(t.Context(), u.share.ID, other); err != nil {
		t.Errorf("another file's commit behind the pinned guard: %v", err)
	}
	if applied, _ := u.fs.Store.Existence().Applied(t.Context(), u.share.ID, dst); !applied.IsZero() {
		t.Errorf("a batch committed the destination at %v while the clone held its guard", applied)
	}
	u.until(t, "the clone's answer", func() bool {
		select {
		case err := <-errc:
			if err != nil {
				t.Errorf("the clone: %v", err)
			}
			return true
		default:
			return false
		}
	})
}

// gatedCommit is the store's existence view holding its commits while on.
type gatedCommit struct {
	engine.Existence
	on               atomic.Bool
	entered, open    chan struct{}
	commitReturned   atomic.Bool
	cloneAfterCommit atomic.Bool
	cloned           atomic.Bool
}

func (x *gatedCommit) Commit(ctx context.Context, p []kvstore.PendingExistence, marks *kvstore.MarksWrite) (map[entity.FileID]error, error) {
	if x.on.CompareAndSwap(true, false) {
		close(x.entered)
		<-x.open
		defer x.commitReturned.Store(true)
	}
	return x.Existence.Commit(ctx, p, marks)
}

func (x *gatedCommit) Clone(ctx context.Context, op kvstore.CloneOp, pending []kvstore.PendingOp) error {
	x.cloneAfterCommit.Store(x.commitReturned.Load())
	x.cloned.Store(true)
	return x.Existence.Clone(ctx, op, pending)
}

// A batch held between its capture and its existence commit on the
// destination is waited out: the clone's phase 1 starts only once it
// returns.
func TestACloneWaitsOutABatchInFlight(t *testing.T) {
	x := &gatedCommit{entered: make(chan struct{}), open: make(chan struct{})}
	u := clonedUnit(t, func(d *engine.Deps) { x.Existence = d.Existence; d.Existence = x })
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	u.write(t, dst, 4*mib, fill(10, 'w'))
	x.on.Store(true)
	committed := make(chan error, 1)
	go func() { committed <- u.e.CommitNow(context.Background(), u.share.ID, dst) }()
	<-x.entered
	errc := make(chan error, 1)
	go func() {
		errc <- u.e.Clone(context.Background(), u.share.ID, src, u.share.ID, dst, vfserr.Extent{Len: mib})
	}()
	time.Sleep(50 * time.Millisecond)
	if x.cloned.Load() {
		t.Error("the clone's phase 1 ran while a batch on the destination was in flight")
	}
	close(x.open)
	if err := <-committed; err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("the clone: %v", err)
	}
	if !x.cloneAfterCommit.Load() {
		t.Error("the clone's phase 1 overtook the batch in flight")
	}
}

// durableAtClone records whether the destination was durable when the
// store's Clone was called.
type durableAtClone struct {
	engine.Existence
	j       interface{ Durable(journal.FileID) bool }
	dst     journal.FileID
	durable atomic.Bool
}

func (x *durableAtClone) Clone(ctx context.Context, op kvstore.CloneOp, pending []kvstore.PendingOp) error {
	x.durable.Store(x.j.Durable(x.dst))
	return x.Existence.Clone(ctx, op, pending)
}

// Phase 1 syncs the destination before the store records it: a write
// outside the extent staged during the clone, then left undurable by
// another file's failed sync window, is durable by the time the store's
// Clone runs.
func TestAClonesPhase1SyncsWhatItCommits(t *testing.T) {
	x := &durableAtClone{}
	u := clonedUnit(t, func(d *engine.Deps) { x.Existence = d.Existence; d.Existence = x })
	x.j = u.j
	src := u.committedFile(t, "src", fill(mib, 's'))
	other := u.committedFile(t, "other", fill(10, 'o'))
	dst := u.create(t, "dst")
	x.dst = journal.NewFileID(dst)
	release, answer := u.heldBeforePhase1(t, src, dst, vfserr.Extent{Len: mib})
	if _, err := u.e.Write(t.Context(), u.share.ID, dst, 3*mib, fill(10, 'w'), engine.WriteOpts{Done: func() {}}); err != nil {
		t.Fatal(err)
	}
	u.j.FailSync(journal.NewFileID(other), journal.Extent{Off: 0, Len: 10}, journal.NewFileID(dst))
	release()
	if err := <-answer; err != nil {
		t.Fatalf("the clone: %v", err)
	}
	if !x.durable.Load() {
		t.Error("the store's Clone ran over a destination not synced since its last write")
	}
}

// A clone whose retried phase 1 its loop's end cuts short leaves the
// destination's entry without its copies or its own operation, so nothing
// commits them above the clone before the next start rebuilds it, and its
// guard is released; the call answers unavailable.
func TestALoopEndingDropsARetriedClone(t *testing.T) {
	x := &refusingClone{}
	x.left.Store(1 << 30)
	u := clonedUnit(t, func(d *engine.Deps) { x.Existence = d.Existence; d.Existence = x })
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	errc := make(chan error, 1)
	go func() {
		errc <- u.e.Clone(context.Background(), u.share.ID, src, u.share.ID, dst, vfserr.Extent{Len: mib})
	}()
	u.until(t, "the clone's first refusal", func() bool { return x.calls.Load() > 0 })
	u.e.CloseGroupCommits()
	if err := <-errc; !errors.Is(err, vfserr.ErrUnavailable) {
		t.Errorf("a clone its loop's end cut short: %v, want ErrUnavailable", err)
	}
	if u.e.GuardHeld(dst) || len(u.e.Pending(dst)) != 0 {
		t.Errorf("after the loop ended: guard %v, pending %+v; want neither", u.e.GuardHeld(dst), u.e.Pending(dst))
	}
}

// probeHandle holds the clone's first copy read once CloneTarget is done,
// and, while gated, a write once the journal gave it its version.
type probeHandle struct {
	engine.Handle
	targeted, goClone chan struct{}
	reads             atomic.Int64
	gate              atomic.Bool
	staged, goWrite   chan struct{}
}

func (h *probeHandle) ReadAt(ctx context.Context, id journal.FileID, off int64, p []byte) (int, []journal.Extent, []journal.Held, journal.Version, error) {
	if h.reads.Add(1) == 2 { // the first copy's read, after CloneTarget
		close(h.targeted)
		<-h.goClone
	}
	return h.Handle.ReadAt(ctx, id, off, p)
}

func (h *probeHandle) WriteAt(ctx context.Context, id journal.FileID, off int64, p []byte, m int64) (journal.Version, error) {
	v, err := h.Handle.WriteAt(ctx, id, off, p, m)
	if h.gate.CompareAndSwap(true, false) {
		close(h.staged)
		<-h.goWrite
	}
	return v, err
}

// cloneCalled closes called once the store's Clone returned.
type cloneCalled struct {
	engine.Existence
	called chan struct{}
}

func (x *cloneCalled) Clone(ctx context.Context, op kvstore.CloneOp, p []kvstore.PendingOp) error {
	err := x.Existence.Clone(ctx, op, p)
	close(x.called)
	return err
}

// A write to the destination outside the extent that the journal gave a
// version between the clone's and its copies' newest, still on its way into
// the entry when phase 1 claims, is waited out and claimed: phase 1 records
// it, rather than setting applied past it, which every later commit would
// skip.
func TestAWriteInFlightDuringPhase1IsClaimed(t *testing.T) {
	x := &cloneCalled{called: make(chan struct{})}
	h := &probeHandle{targeted: make(chan struct{}), goClone: make(chan struct{}), staged: make(chan struct{}), goWrite: make(chan struct{})}
	u := clonedUnitWith(t, unitConfig{
		deps: []func(*engine.Deps){func(d *engine.Deps) { x.Existence = d.Existence; d.Existence = x }},
		wrap: func(x engine.Handle) engine.Handle {
			h.Handle = x
			return h
		},
	})
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	h.reads.Store(0)
	errc := make(chan error, 1)
	go func() {
		errc <- u.e.Clone(context.Background(), u.share.ID, src, u.share.ID, dst, vfserr.Extent{Len: mib})
	}()
	<-h.targeted
	h.gate.Store(true)
	werr := make(chan error, 1)
	go func() {
		_, err := u.e.Write(context.Background(), u.share.ID, dst, 3*mib, fill(10, 'w'), engine.WriteOpts{Done: func() {}})
		werr <- err
	}()
	<-h.staged // the write has its version, above the clone's, below the copies
	close(h.goClone)
	select {
	case <-x.called:
		t.Error("phase 1 ran while a write to the destination was still on its way into the entry")
	case <-time.After(200 * time.Millisecond):
	}
	close(h.goWrite)
	if err := <-werr; err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if err := u.e.CommitNow(t.Context(), u.share.ID, dst); err != nil {
		t.Fatal(err)
	}
	if size, _ := u.fs.Store.Existence().Size(t.Context(), u.share.ID, dst); size != 3*mib+10 {
		t.Errorf("the destination's committed size %d, want %d: the write was skipped as applied", size, 3*mib+10)
	}
}

// A release or a truncate of a destination a clone holds is answered at
// once, the release pending and the truncate DelayClone, rather than behind
// the clone's guard.
func TestARemovalOfAClonesDestinationAnswersAtOnce(t *testing.T) {
	u := clonedUnit(t)
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	release, answer := u.heldBeforePhase1(t, src, dst, vfserr.Extent{Len: mib})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if ch, err := u.e.Delete(ctx, u.share.ID, dst); err != nil || ch == nil {
		t.Errorf("a release of the clone's destination: %v, %v; want a pending channel", ch, err)
	}
	if err := u.e.Truncate(ctx, u.share.ID, dst, 4*mib); !isDelay(err, vfserr.DelayClone) {
		t.Errorf("a truncate of the clone's destination: %v, want DelayClone", err)
	}
	release()
	if err := <-answer; err != nil {
		t.Fatal(err)
	}
}

// A file holds one clone into it at a time: a second clone into another
// extent of a destination a clone writes is refused DelayClone.
func TestASecondCloneIntoADestinationIsRefused(t *testing.T) {
	u := clonedUnit(t)
	src := u.committedFile(t, "src", fill(2*mib, 's'))
	dst := u.create(t, "dst")
	release, answer := u.heldBeforePhase1(t, src, dst, vfserr.Extent{Len: mib})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err := u.e.Clone(ctx, u.share.ID, src, u.share.ID, dst, vfserr.Extent{SrcOff: mib, DstOff: 4 * mib, Len: mib})
	if !isDelay(err, vfserr.DelayClone) {
		t.Errorf("a second clone into the destination: %v, want DelayClone", err)
	}
	if _, d := u.e.Registrations(dst); d != 1 {
		t.Errorf("%d destination registrations on the file, want the first clone's alone", d)
	}
	release()
	if err := <-answer; err != nil {
		t.Fatal(err)
	}
}

// An allocation of an empty destination cloned far from its start answers a
// hole from 0 up to the extent, which the clone's journal step leaves.
func TestAnAllocationBeforeAFarCloneIsAHole(t *testing.T) {
	u := clonedUnit(t)
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	release, answer := u.heldBeforePhase1(t, src, dst, vfserr.Extent{DstOff: 8 * mib, Len: mib})
	sp, err := u.e.Allocation(t.Context(), u.share.ID, dst, 0)
	if err != nil || sp.Class != entity.Hole || sp.Offset != 0 || sp.End() != 8*mib {
		t.Errorf("an allocation from 0 of the destination: %+v, %v; want a hole up to 8 MiB", sp, err)
	}
	release()
	if err := <-answer; err != nil {
		t.Fatal(err)
	}
}

// losingTarget is the destination's handle running lose, once set, when
// CloneTarget is done, after the clone sized its copies.
type losingTarget struct {
	engine.Handle
	lose *armed
}

func (h losingTarget) CloneTarget(ctx context.Context, id journal.FileID, spec journal.CloneSpec) (journal.Version, error) {
	v, err := h.Handle.CloneTarget(ctx, id, spec)
	h.lose.run()
	return v, err
}

// A loss that splits a source run after the clone sized its copies takes one
// more copy write than it reserved: the copy is refused, retried by the loop
// with a reservation sized again, and the clone completes, the source read
// around the loss.
func TestACopyPastItsReservationIsRetried(t *testing.T) {
	lose := &armed{}
	u := clonedUnitWith(t, unitConfig{wrap: func(h engine.Handle) engine.Handle { return losingTarget{h, lose} }})
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	lose.set(func() { u.lose(src, mib/4, mib/4) })
	errc := make(chan error, 1)
	go func() {
		errc <- u.e.Clone(context.Background(), u.share.ID, src, u.share.ID, dst, vfserr.Extent{Len: mib})
	}()
	var got error
	u.until(t, "the clone's answer", func() bool {
		select {
		case got = <-errc:
			return true
		default:
			return false
		}
	})
	if got != nil {
		t.Fatalf("a clone whose copies outran their reservation: %v, want it retried to success", got)
	}
	if r := u.j.Reserves(); len(r) != 2 || r[1].Writes != 2 {
		t.Errorf("reserves %+v, want the retry's second one of 2 writes", r)
	}
	if got := u.read(t, dst, mib/2, mib/2); !bytes.Equal(got, fill(mib/2, 's')) {
		t.Error("the destination does not read the source after the loss")
	}
}

// A phase 1 failing on a conflict is retried without being logged as a
// refusal, and the clone completes.
func TestAConflictedClonePhase1IsRetriedQuietly(t *testing.T) {
	x := &refusingClone{err: errors.Join(kvstore.ErrConflict, context.DeadlineExceeded)}
	x.left.Store(1)
	var logged lockedBuffer
	u := clonedUnit(t, func(d *engine.Deps) {
		x.Existence = d.Existence
		d.Existence = x
		d.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	})
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	errc := make(chan error, 1)
	go func() {
		errc <- u.e.Clone(context.Background(), u.share.ID, src, u.share.ID, dst, vfserr.Extent{Len: mib})
	}()
	var got error
	u.until(t, "the clone's answer", func() bool {
		select {
		case got = <-errc:
			return true
		default:
			return false
		}
	})
	if got != nil {
		t.Fatalf("a clone conflicted once at phase 1: %v", got)
	}
	if strings.Contains(logged.String(), "refused") {
		t.Errorf("a conflict was logged as a refusal: %q", logged.String())
	}
}

// A share dropped while its loop is running its clone's phase 1 ends the
// clone once that run returns, unavailable, and leaves the destination off
// the group commit's due set.
func TestADroppedShareEndsItsCloneAndNotesNothing(t *testing.T) {
	x := &refusingClone{entered: make(chan struct{}), hold: make(chan struct{})}
	x.left.Store(1 << 30)
	u := clonedUnit(t, func(d *engine.Deps) { x.Existence = d.Existence; d.Existence = x })
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	u.write(t, dst, 4*mib, fill(10, 'w'))
	errc := make(chan error, 1)
	go func() {
		errc <- u.e.Clone(context.Background(), u.share.ID, src, u.share.ID, dst, vfserr.Extent{Len: mib})
	}()
	u.until(t, "the loop running the clone's phase 1", func() bool {
		select {
		case <-x.entered:
			return true
		default:
			return false
		}
	})
	if err := u.e.DropShare(t.Context(), u.rec, u.share.ID); err != nil {
		t.Fatal(err)
	}
	close(x.hold)
	u.until(t, "the dropped clone's answer", func() bool {
		select {
		case err := <-errc:
			if !errors.Is(err, vfserr.ErrUnavailable) {
				t.Errorf("a clone of a dropped share: %v, want ErrUnavailable", err)
			}
			return true
		default:
			return false
		}
	})
	if u.e.Due(u.rec, dst) {
		t.Error("the dropped share's destination was noted for a commit again")
	}
}

// Until a clone's hold, phase 2 and done signal exist, a clone answers
// ErrUnbuilt once registered and leaves nothing behind.
func TestACloneWithoutItsDoneSignalIsUnbuilt(t *testing.T) {
	u := cloneUnit(t)
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	if err := u.e.Clone(t.Context(), u.share.ID, src, u.share.ID, dst, vfserr.Extent{Len: mib}); !errors.Is(err, engine.ErrUnbuilt) {
		t.Errorf("a clone with no done signal: %v, want ErrUnbuilt", err)
	}
	if n := len(u.j.CloneTargets()); n != 0 || u.e.HasEntry(src) || u.e.HasEntry(dst) {
		t.Errorf("an unbuilt clone left %d CloneTargets, entries %v %v", n, u.e.HasEntry(src), u.e.HasEntry(dst))
	}
}

// recordingClone records the operations each store Clone was handed.
type recordingClone struct {
	engine.Existence
	mu      sync.Mutex
	handled []kvstore.PendingOp
}

func (x *recordingClone) Clone(ctx context.Context, op kvstore.CloneOp, p []kvstore.PendingOp) error {
	x.mu.Lock()
	x.handled = append(x.handled, p...)
	x.mu.Unlock()
	return x.Existence.Clone(ctx, op, p)
}

// The copies never reach the store's Clone, which Copied covers: none of the
// operations it is handed is a copy's.
func TestACloneHandsTheStoreNoCopy(t *testing.T) {
	x := &recordingClone{}
	u := clonedUnit(t, func(d *engine.Deps) { x.Existence = d.Existence; d.Existence = x })
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")
	u.write(t, dst, 4*mib, fill(10, 'w'))
	u.clone(t, src, dst, vfserr.Extent{Len: mib})
	copies := map[entity.JournalVersion]bool{}
	for _, v := range u.j.ReservedWrites() {
		copies[entity.JournalVersion(v.Bytes())] = true
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if len(x.handled) != 1 {
		t.Errorf("the store's Clone was handed %d operations, want the destination's one write", len(x.handled))
	}
	for _, op := range x.handled {
		if copies[op.Version] {
			t.Errorf("the store's Clone was handed a copy at %v", op.Version)
		}
	}
}

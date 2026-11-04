package engine_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore/kvstoretest"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// unit is the engine composed for a unit test: over the fake journal, the
// real store on an in-memory kv.KV and a test clock, with one share bound
// through Bind unless the test asks for none.
type unit struct {
	e     *engine.Engine
	j     *enginetest.FakeJournal
	rec   *recJournal // the engine's view of j, which records each Share and Close
	fs    *kvstoretest.FS
	clock *sim.TestClock
	share engine.Share
	h     engine.Handle // the share's handle on the fake journal, as its Share returned it
}

// unitLimit is the fake journal's version limit in a unit test.
const unitLimit = 1 << 62

// unitConfig is what a test asks of newUnitWith.
type unitConfig struct {
	deps    []func(*engine.Deps)              // each changes the engine's Deps before New
	unbound bool                              // leaves the share unbound: the test binds what it needs
	wrap    func(engine.Handle) engine.Handle // hands the engine every handle the fake journal opens through it
	kv      func(kv.KV) kv.KV                 // wraps the store's kv.KV
}

// newUnit is the unit with its share bound, each of deps changing the
// engine's Deps before New.
func newUnit(t *testing.T, deps ...func(*engine.Deps)) *unit {
	t.Helper()

	return newUnitWith(t, unitConfig{deps: deps})
}

// newUnitOver is newUnit with the store's kv.KV wrapped by wrap.
func newUnitOver(t *testing.T, wrap func(kv.KV) kv.KV, deps ...func(*engine.Deps)) *unit {
	t.Helper()

	return newUnitWith(t, unitConfig{deps: deps, kv: wrap})
}

// newUnitWith is the unit as c asks.
func newUnitWith(t *testing.T, c unitConfig) *unit {
	t.Helper()
	prof.Watch(t, prof.Options{})

	u := &unit{
		j:     enginetest.NewFakeJournal(),
		clock: sim.NewTestClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
	}
	u.rec = &recJournal{
		FakeJournal: u.j,
		wrap:        c.wrap,
	}
	u.fs = kvstoretest.NewFS(t, func(tb testing.TB) kv.KV {
		db := memKV(tb)
		if c.kv != nil {
			return c.kv(db)
		}
		return db
	})

	d := engine.Deps{
		Node:    entity.NodeID{1},
		Devices: []string{"unit"},
		Open: func(context.Context, string, journal.Version, journal.JournalID) (engine.Journal, error) {
			return u.rec, nil
		},
		Existence: u.fs.Store.Existence(),
		Content:   u.fs.Store.Content(),
		Lifecycle: u.fs.Store.Lifecycle(),
		Holds:     noHolds{},
		Clock:     u.clock,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Tracer: func(ctx context.Context, _ string, _ ...slog.Attr) (context.Context, func(error)) {
			return ctx, func(error) {}
		},
	}

	for _, f := range c.deps {
		f(&d)
	}

	e, err := engine.New(d)
	if err != nil {
		t.Fatal(err)
	}
	// An existence age no test reaches unless it applies its own.
	if err := e.Apply(engine.Settings{ExistenceAge: 24 * time.Hour, GroupCommitMaxFiles: 64, CloneMaxLen: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	// The cleanup drops its reference once run, so the profiler's heap check,
	// which runs last, does not count what the engine still held.
	held := e
	t.Cleanup(func() { held.CloseGroupCommits(); held = nil })

	u.e = e
	e.SetJournalsForTest(u.rec)

	// The version limit start raises; the fake refuses every version below
	// none. It sits above every version the store stamps, which follow the
	// clock in nanoseconds, so a share's floor stays below it.
	if err := u.j.SetVersionLimit(journal.Version{Counter: unitLimit}); err != nil {
		t.Fatal(err)
	}

	u.share = enginetest.ShareOf(t, u.fs.Store, u.fs.Share.ID, 1<<30)

	if c.unbound {
		return u
	}

	if err := e.Bind(t.Context(), u.share); err != nil {
		t.Fatal(err)
	}

	u.h = u.rec.raw(0)

	return u
}

// memKV is a fresh in-memory kv.KV, closed when the test ends.
func memKV(tb testing.TB) kv.KV {
	db, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		tb.Fatal(err)
	}

	tb.Cleanup(func() {
		_ = db.Close()
	})

	return db
}

// recJournal is the engine's view of a fake journal: it records the floor of
// every Share call and each handle it returned, closed or not, and hands the
// engine each handle through wrap when a test sets one.
type recJournal struct {
	*enginetest.FakeJournal
	wrap     func(engine.Handle) engine.Handle
	closeErr error // every handle's Close answers it, after closing the handle

	mu      sync.Mutex
	floors  []journal.Version
	handles []*recHandle
}

func (j *recJournal) Share(
	tag journal.ShareTag,
	limit int64,
	floor journal.Version,
) (engine.Handle, error) {
	j.mu.Lock()
	j.floors = append(j.floors, floor)
	j.mu.Unlock()

	h, err := j.FakeJournal.Share(tag, limit, floor)
	if err != nil {
		return nil, err
	}

	rh := &recHandle{
		Handle:   h,
		closeErr: j.closeErr,
	}

	j.mu.Lock()
	j.handles = append(j.handles, rh)
	j.mu.Unlock()

	if j.wrap != nil {
		return j.wrap(rh), nil
	}

	return rh, nil
}

// shares is the count of Share calls, refused ones included.
func (j *recJournal) shares() int {
	j.mu.Lock()

	defer j.mu.Unlock()

	return len(j.floors)
}

// floor is the floor the i-th Share call passed.
func (j *recJournal) floor(i int) journal.Version {
	j.mu.Lock()

	defer j.mu.Unlock()

	return j.floors[i]
}

// open is the count of handles returned and not closed.
func (j *recJournal) open() int {
	j.mu.Lock()

	defer j.mu.Unlock()

	n := 0

	for _, h := range j.handles {
		if !h.closed.Load() {
			n++
		}
	}

	return n
}

// raw is the fake's own i-th handle.
func (j *recJournal) raw(i int) engine.Handle {
	j.mu.Lock()

	defer j.mu.Unlock()

	return j.handles[i].Handle
}

// recHandle records its Close.
type recHandle struct {
	engine.Handle
	closed   atomic.Bool
	closeErr error
}

func (h *recHandle) Close() error {
	h.closed.Store(true)
	err := h.Handle.Close()

	if h.closeErr != nil {
		return h.closeErr
	}

	return err
}

// noHolds stands for the open-state view no unit test reaches: a call to it
// is a test's bug.
type noHolds struct {
	engine.CloneHold
}

// A share with no binding is ErrDelay with DelayFrozen, its binding still to
// come, and the bound share is found.
func TestAnUnboundShareIsFrozen(t *testing.T) {
	u := newUnit(t)
	if err := u.e.BindingOf(u.share.ID); err != nil {
		t.Errorf("the bound share: %v", err)
	}
	var d *vfserr.Delay
	if err := u.e.BindingOf(entity.NewShareID()); !errors.As(err, &d) || d.Cause != vfserr.DelayFrozen {
		t.Errorf("a share with no binding: %v, want ErrDelay(DelayFrozen)", err)
	}
}

// A lookup of a file with no entry allocates nothing and creates nothing.
func TestALookupCreatesNoEntry(t *testing.T) {
	u := newUnit(t)
	f := entity.NewFileID()
	if n := testing.AllocsPerRun(100, func() { _ = u.e.HasEntry(f) }); n != 0 {
		t.Errorf("a lookup of an absent file allocates %v times", n)
	}
	if n := u.e.FileEntries(); n != 0 {
		t.Errorf("after lookups, %d entries; want none", n)
	}
}

// An entry lives only while it holds something: a pending operation keeps
// it, and dropping the last removes it.
func TestAnEntryLivesWhileItHoldsSomething(t *testing.T) {
	u := newUnit(t)
	f := entity.NewFileID()
	drop := u.e.AddPending(f, 0, 4)
	if !u.e.HasEntry(f) {
		t.Fatal("a file with a pending write has no entry")
	}
	drop()
	if u.e.HasEntry(f) || u.e.FileEntries() != 0 {
		t.Errorf("after its last operation dropped: entry %v, %d entries; want none", u.e.HasEntry(f), u.e.FileEntries())
	}
}

// One file's entry lock held, and a caller waiting for it, hold up no other
// file in the same shard: its lookup, insert and release complete meanwhile,
// and record no lock wait. A shard lock held across an entry wait, or a
// stripe two files share, fails it.
func TestAHeldEntryHoldsUpNoOtherFile(t *testing.T) {
	u := newUnit(t)
	a := entity.NewFileID()
	b := entity.NewFileID()
	for !u.e.SameShard(a, b) {
		b = entity.NewFileID()
	}
	dropA := u.e.AddPending(a, 0, 1) // a stays in its shard, so releasing it takes no shard lock
	release := u.e.HoldEntry(a)
	waited := make(chan struct{})
	go func() { defer close(waited); u.e.HoldEntry(a)() }()
	deadline := time.Now().Add(time.Second)
	for u.e.FileStateWaiting() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the second caller of the held file never waited")
		}
		time.Sleep(time.Millisecond)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		u.e.AddPending(b, 0, 1)()
		_ = u.e.HasEntry(b)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("a second file in the held file's shard waited on it")
	}
	if n, _ := u.e.FileStateWaits(); n != 0 {
		t.Errorf("the second file's operations recorded %d lock waits", n)
	}
	release()
	<-waited
	<-done
	dropA()
}

// A caller that waited on an entry which was emptied and removed meanwhile
// takes the file's new entry, never the removed one: its operation stays
// visible, in the shard's map. Taking the dead entry, or not marking it dead,
// puts the operation into an entry nothing finds again.
func TestAWaiterOnARemovedEntryTakesTheNewOne(t *testing.T) {
	u := newUnit(t)
	f := entity.NewFileID()
	release := u.e.HoldEntry(f) // empty: removed when released
	added := make(chan struct{})
	go func() { defer close(added); u.e.AddPending(f, 0, 1) }()
	deadline := time.Now().Add(time.Second)
	for u.e.FileStateWaiting() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the adder never waited")
		}
		time.Sleep(time.Millisecond)
	}
	release()
	<-added
	if !u.e.HasEntry(f) || len(u.e.Pending(f)) != 1 {
		t.Errorf("after the wait: entry %v, %d pending; want the entry with the one operation", u.e.HasEntry(f), len(u.e.Pending(f)))
	}
}

// A wait for an entry's lock is recorded in the file_state series, timed by
// the clock; a lock taken without waiting records nothing.
func TestALockWaitIsRecorded(t *testing.T) {
	u := newUnit(t)
	f := entity.NewFileID()
	u.e.HoldEntry(f)() // uncontended
	if n, _ := u.e.FileStateWaits(); n != 0 {
		t.Fatalf("an uncontended lock recorded %d waits", n)
	}
	release := u.e.HoldEntry(f)
	done := make(chan struct{})
	go func() { defer close(done); u.e.HoldEntry(f)() }()
	deadline := time.Now().Add(time.Second)
	for u.e.FileStateWaiting() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the second caller never waited")
		}
		time.Sleep(time.Millisecond)
	}
	u.clock.Advance(3 * time.Second)
	release()
	<-done
	if n, total := u.e.FileStateWaits(); n != 1 || total < 3*time.Second {
		t.Errorf("file_state waits: %d totalling %v; want one of at least 3s", n, total)
	}
}

// Writers on 64 files at once, under the race detector, leave no entry.
func TestWritersOn64Files(t *testing.T) {
	u := newUnit(t)
	var wg sync.WaitGroup
	for range 64 {
		f := entity.NewFileID()
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 100 {
					u.e.AddPending(f, 0, 1)()
				}
			}()
		}
	}
	wg.Wait()
	if n := u.e.FileEntries(); n != 0 {
		t.Errorf("after every writer dropped its operations, %d entries remain", n)
	}
}

// A pending operation holds no bytes: no field is a slice or array of
// bytes, so the engine keeps no copy of what the journal holds.
func TestAPendingOperationHoldsNoBytes(t *testing.T) {
	typ := reflect.TypeFor[engine.PendingOpType]()
	for i := range typ.NumField() {
		f := typ.Field(i)
		k := f.Type.Kind()
		if (k == reflect.Slice || k == reflect.Array) && f.Type.Elem().Kind() == reflect.Uint8 {
			t.Errorf("pending operation field %s is %s: a copy of the payload", f.Name, f.Type)
		}
	}
}

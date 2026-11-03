package engine_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/engine/enginetest"
	"github.com/marmos91/dittofs/pkg/block/journal"
	"github.com/marmos91/dittofs/pkg/block/journal/journaltest/prof"
	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore/kvstoretest"
	"github.com/marmos91/dittofs/pkg/sim"
	"github.com/marmos91/dittofs/pkg/vfs/vfserr"
)

// unit is the engine composed for a unit test: over the fake journal, the
// real store on an in-memory kv.KV and a test clock, with one share bound
// through the test hook, as Bind and Start cannot yet.
type unit struct {
	e     *engine.Engine
	j     *enginetest.FakeJournal
	fs    *kvstoretest.FS
	clock *sim.TestClock
	share engine.Share
	h     engine.Handle // the share's handle on the fake journal
}

func newUnit(t *testing.T, opts ...func(*engine.Deps)) *unit {
	t.Helper()
	prof.Watch(t, prof.Options{})
	u := &unit{j: enginetest.NewFakeJournal(), clock: sim.NewTestClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))}
	u.fs = kvstoretest.NewFS(t, func(tb testing.TB) kv.KV {
		db, err := badgerkv.Open(badgerkv.Options{})
		if err != nil {
			tb.Fatal(err)
		}
		tb.Cleanup(func() { _ = db.Close() })
		return db
	})
	d := engine.Deps{
		Devices: []string{"unit"},
		Open: func(context.Context, string, journal.Version, journal.JournalID) (engine.Journal, error) {
			return u.j, nil
		},
		Existence: u.fs.Store.Existence(),
		Content:   u.fs.Store.Content(),
		Lifecycle: noLifecycle{},
		Holds:     noHolds{},
		Clock:     u.clock,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Tracer: func(ctx context.Context, _ string, _ ...slog.Attr) (context.Context, func(error)) {
			return ctx, func(error) {}
		},
	}
	for _, o := range opts {
		o(&d)
	}
	e, err := engine.New(d)
	if err != nil {
		t.Fatal(err)
	}
	// The version limit start raises; the fake refuses every version below
	// none.
	if err := u.j.SetVersionLimit(journal.Version{Counter: 1 << 40}); err != nil {
		t.Fatal(err)
	}
	u.share = engine.Share{ID: u.fs.Share.ID, Tag: 1, Limit: 1 << 30, State: engine.Serving}
	h, err := u.j.Share(u.share.Tag, u.share.Limit, journal.Version{}, u.share.SyncPolicy)
	if err != nil {
		t.Fatal(err)
	}
	e.BindForTest(u.share, u.j, h)
	u.h = h
	u.e = e
	return u
}

// noLifecycle and noHolds stand for views no unit test reaches: a call to
// one is a test's bug.
type noLifecycle struct{ engine.Lifecycle }
type noHolds struct{ engine.CloneHold }

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

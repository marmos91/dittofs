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
)

// failWindow fails the sync window of f's [off, off+n).
func (u *unit) failWindow(f entity.FileID, off, n int64) {
	u.j.FailSync(journal.NewFileID(f), journal.Extent{Off: off, Len: n})
}

// A window the group commit's sync fails, with no Commit waiting, is reported
// to the file's next Commit: a verifier other than its write's and a raised
// loss sequence. The window's write is dropped, not committed. The Commit
// after a new write answers that write's verifier.
func TestAFailedWindowReachesTheNextCommit(t *testing.T) {
	u := newUnit(t)
	if err := u.e.Apply(engine.Settings{ExistenceAge: time.Second, GroupCommitMaxFiles: 64, CloneMaxLen: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	h, made := u.fs.Create(t, u.fs.Admin, u.fs.Root, "a", kvstore.Regular(0o644))
	f := made.ID
	v, err := u.e.Write(t.Context(), u.share.ID, f, 0, []byte("lost"), engine.WriteOpts{Done: func() {}})
	if err != nil {
		t.Fatal(err)
	}
	u.failWindow(f, 0, 4)
	eventually(t, "the group commit's timer is set", func() bool { return u.clock.Pending() > 0 })
	u.clock.Advance(time.Second)
	eventually(t, "the group commit runs", func() bool { return !u.e.HasEntry(f) })
	if got, err := u.fs.Store.Files().Get(t.Context(), u.fs.Admin, h); err != nil || got.Size != 0 {
		t.Errorf("committed size %d, %v; want the dropped write uncommitted", got.Size, err)
	}

	cv, seq, err := u.e.Commit(t.Context(), u.share.ID, f, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if cv == v || seq != 1 {
		t.Errorf("the next Commit answered verifier %x, sequence %d; want other than the write's %x and 1", cv, seq, v)
	}
	w, err := u.e.Write(t.Context(), u.share.ID, f, 0, []byte("anew"), engine.WriteOpts{Done: func() {}})
	if err != nil {
		t.Fatal(err)
	}
	if cw, _, err := u.e.Commit(t.Context(), u.share.ID, f, 0, 0); err != nil || cw != w {
		t.Errorf("the Commit after a new write answered %x, %v; want the write's %x", cw, err, w)
	}
}

// A window Commit's own sync fails is answered as a success, with the new
// verifier and the raised sequence, never an I/O error.
func TestAFailedWindowOnCommitsSyncIsNoError(t *testing.T) {
	u := newUnit(t)
	f := u.create(t, "a")
	v, err := u.e.Write(t.Context(), u.share.ID, f, 0, []byte("lost"), engine.WriteOpts{Done: func() {}})
	if err != nil {
		t.Fatal(err)
	}
	u.failWindow(f, 0, 4)
	cv, seq, err := u.e.Commit(t.Context(), u.share.ID, f, 0, 0)
	if err != nil {
		t.Fatalf("Commit over a failed window: %v, want success", err)
	}
	if cv == v || seq != 1 {
		t.Errorf("verifier %x, sequence %d; want other than %x, and 1", cv, seq, v)
	}
}

// A SyncError naming a file no failed-window event covers still raises that
// file's sequence, once.
func TestASyncErrorWithNoEventRaisesItsFile(t *testing.T) {
	u := newUnit(t)
	f := u.create(t, "a")
	u.write(t, f, 0, []byte("held"))
	u.failWindow(f, 100, 4) // past what f holds: no event, only the SyncError
	if _, seq, err := u.e.Commit(t.Context(), u.share.ID, f, 0, 0); err != nil || seq != 1 {
		t.Errorf("Commit: sequence %d, %v; want 1", seq, err)
	}
}

// durableAtCommit is a store that records, for each existence commit, any
// file it commits that the fake journal does not hold durable.
type durableAtCommit struct {
	engine.Existence
	j     *enginetest.FakeJournal
	mu    sync.Mutex
	early []entity.FileID
}

func (x *durableAtCommit) Commit(ctx context.Context, p []kvstore.PendingExistence, marks *kvstore.MarksWrite) (map[entity.FileID]error, error) {
	x.mu.Lock()
	for _, pe := range p {
		if !x.j.Durable(journal.NewFileID(pe.File)) {
			x.early = append(x.early, pe.File)
		}
	}
	x.mu.Unlock()
	return x.Existence.Commit(ctx, p, marks)
}

// A failed window promises nothing durable, the other files' writes
// included: the group commit syncs again before it commits any of them.
func TestAFailedWindowIsSyncedAgain(t *testing.T) {
	x := &durableAtCommit{}
	u := newUnit(t, func(d *engine.Deps) { x.Existence = d.Existence; d.Existence = x })
	x.j = u.j
	a, b := u.create(t, "a"), u.create(t, "b")
	u.write(t, a, 0, []byte("lost"))
	u.write(t, b, 0, []byte("kept"))
	u.j.FailSync(journal.NewFileID(a), journal.Extent{Len: 4}, journal.NewFileID(b))
	if err := u.e.CommitAll(t.Context(), u.rec); err != nil {
		t.Fatal(err)
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if len(x.early) > 0 {
		t.Errorf("%d files committed before their writes were durable", len(x.early))
	}
	if u.e.HasEntry(b) {
		t.Error("b was not committed")
	}
}

// One failed window raises its file's sequence once, though LossSeq's drain
// read its event before Commit's sync met its SyncError.
func TestAFailedWindowRaisesOnce(t *testing.T) {
	u := newUnit(t)
	f := u.create(t, "a")
	u.write(t, f, 0, []byte("lost"))
	u.failWindow(f, 0, 4)
	if s := u.e.LossSeq(u.share.ID, f); s != 1 {
		t.Fatalf("LossSeq after the window failed: %d, want 1", s)
	}
	if _, seq, err := u.e.Commit(t.Context(), u.share.ID, f, 0, 0); err != nil || seq != 1 {
		t.Errorf("Commit after LossSeq: sequence %d, %v; want still 1", seq, err)
	}
}

// A failing file whose writes a failed window dropped is no longer named
// failing once its next batch finds nothing to commit.
func TestAFailingFileWhoseWritesWereDroppedIsCleared(t *testing.T) {
	u := newUnit(t)
	_, dir := u.fs.Create(t, u.fs.Admin, u.fs.Root, "d", kvstore.Dir(0o755))
	d := dir.ID
	u.write(t, d, 0, []byte("not content"))
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	_ = u.e.CommitNow(ctx, u.share.ID, d)
	if !u.e.Failing(u.rec, d) {
		t.Fatal("the directory's commit is not named failing")
	}
	u.failWindow(d, 0, 11)
	_ = u.e.LossSeq(u.share.ID, d) // drains: the window's write leaves the entry
	u.clock.Advance(time.Minute)   // past the backoff
	eventually(t, "the failure is cleared", func() bool { return !u.e.Failing(u.rec, d) })
}

// A failed window's loss drained between a write's WriteAt and its insert
// finds no pending write to drop, so the insert drops it: the committed size
// excludes it, nothing is charged, and Done runs once.
func TestAFailedWindowBeforeTheWritesInsertDropsIt(t *testing.T) {
	u := newUnit(t)
	h, made := u.fs.Create(t, u.fs.Admin, u.fs.Root, "a", kvstore.Regular(0o644))
	f := made.ID
	var once sync.Once
	u.e.OnWritten(func() {
		once.Do(func() {
			u.failWindow(f, 0, 4)
			u.e.LossSeq(u.share.ID, f) // drains the loss
		})
	})
	var dones atomic.Int32
	if _, err := u.e.Write(t.Context(), u.share.ID, f, 0, []byte("lost"), engine.WriteOpts{Done: func() { dones.Add(1) }}); err != nil {
		t.Fatal(err)
	}
	if err := u.e.CommitNow(t.Context(), u.share.ID, f); err != nil {
		t.Fatal(err)
	}
	if got, err := u.fs.Store.Files().Get(t.Context(), u.fs.Admin, h); err != nil || got.Size != 0 || got.Charged != 0 {
		t.Errorf("size %d, charged %d, %v; want the dropped write uncommitted, charging nothing", got.Size, got.Charged, err)
	}
	if n := dones.Load(); n != 1 {
		t.Errorf("Done ran %d times, want once", n)
	}
}

package engine_test

import (
	"bytes"
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
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// callLog records the order of the journal syncs and existence commits a
// group commit makes, and the files of each commit.
type callLog struct {
	mu      sync.Mutex
	calls   []string
	commits [][]entity.FileID
}

func (l *callLog) add(c string) {
	l.mu.Lock()
	l.calls = append(l.calls, c)
	l.mu.Unlock()
}

type loggedHandle struct {
	engine.Handle
	log *callLog
}

func (h loggedHandle) Sync(ctx context.Context, ids ...journal.FileID) error {
	h.log.add("sync")
	return h.Handle.Sync(ctx, ids...)
}

type loggedExistence struct {
	engine.Existence
	log *callLog
}

func (x loggedExistence) Commit(ctx context.Context, p []kvstore.PendingExistence, marks *kvstore.MarksWrite) (map[entity.FileID]error, error) {
	var fs []entity.FileID
	for _, pe := range p {
		fs = append(fs, pe.File)
	}
	x.log.mu.Lock()
	x.log.calls = append(x.log.calls, "commit")
	x.log.commits = append(x.log.commits, fs)
	x.log.mu.Unlock()
	return x.Existence.Commit(ctx, p, marks)
}

// newLoggedUnit is a unit whose share's handle and store log their calls,
// with existence_age age and group_commit.max_files maxFiles.
func newLoggedUnit(t *testing.T, age time.Duration, maxFiles int) (*unit, *callLog) {
	t.Helper()
	log := &callLog{}
	u := newUnitWith(t, unitConfig{
		deps: []func(*engine.Deps){func(d *engine.Deps) { d.Existence = loggedExistence{d.Existence, log} }},
		wrap: func(h engine.Handle) engine.Handle { return loggedHandle{h, log} },
	})
	if err := u.e.Apply(engine.Settings{ExistenceAge: age, GroupCommitMaxFiles: maxFiles, CloneMaxLen: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	u.h = loggedHandle{u.h, log}
	return u, log
}

// eventually fails the test unless ok holds within five seconds.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); {
		if time.Now().After(deadline) {
			t.Fatalf("after 5 s: %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// A file with pending writes and no stability point is committed once its
// oldest write is existence_age old by the clock, synced before it commits.
func TestALoneWriteIsCommittedAtItsAge(t *testing.T) {
	u, log := newLoggedUnit(t, time.Second, 64)
	f := u.create(t, "a")
	u.write(t, f, 0, []byte("abc"))
	eventually(t, "the group commit's timer is set", func() bool { return u.clock.Pending() > 0 })
	if u.e.PendingApplied() != 0 {
		t.Fatal("committed before its age")
	}
	u.clock.Advance(time.Second)
	eventually(t, "the write is committed", func() bool { return !u.e.HasEntry(f) })
	log.mu.Lock()
	defer log.mu.Unlock()
	if len(log.calls) != 2 || log.calls[0] != "sync" || log.calls[1] != "commit" {
		t.Errorf("calls %v, want one sync then one commit", log.calls)
	}
	if u.e.PendingApplied() != 1 {
		t.Errorf("engine_pending_existence_ops %d, want 1", u.e.PendingApplied())
	}
}

// 64 files due together commit in ⌈64/G⌉ calls of at most G files, each
// after a sync of its files.
func TestDueFilesCommitInBoundedBatches(t *testing.T) {
	u, log := newLoggedUnit(t, time.Second, 16)
	var fs []entity.FileID
	for i := range 64 {
		f := u.create(t, string(rune('a'+i/26))+string(rune('a'+i%26)))
		u.write(t, f, 0, []byte("x"))
		fs = append(fs, f)
	}
	u.clock.Advance(time.Second)
	eventually(t, "every file is committed", func() bool { return u.e.FileEntries() == 0 })
	log.mu.Lock()
	defer log.mu.Unlock()
	if len(log.commits) != 4 {
		t.Fatalf("%d existence commits, want 4 of 16 files", len(log.commits))
	}
	for i, c := range log.commits {
		if len(c) != 16 {
			t.Errorf("commit %d holds %d files, want 16", i, len(c))
		}
	}
	for i := 0; i < len(log.calls); i += 2 {
		if log.calls[i] != "sync" || log.calls[i+1] != "commit" {
			t.Fatalf("calls %v: each commit must follow its batch's sync", log.calls)
		}
	}
}

// A write whose open suspended the modification time commits its own change
// time and leaves the modification time an earlier write set.
func TestASuspendedWriteCommitsItsChangeTimeAlone(t *testing.T) {
	u := newUnit(t)
	h, made := u.fs.Create(t, u.fs.Admin, u.fs.Root, "a", kvstore.Regular(0o644))
	f := made.ID
	u.clock.Advance(time.Since(u.clock.Now()) + time.Hour) // past the store's own time of the create
	first := u.clock.Now()
	u.write(t, f, 0, []byte("one"))
	u.clock.Advance(time.Minute)
	second := u.clock.Now()
	if _, err := u.e.Write(t.Context(), u.share.ID, f, 3, []byte("two"), engine.WriteOpts{Suspend: entity.TimeModify, Done: func() {}}); err != nil {
		t.Fatal(err)
	}
	if err := u.e.CommitNow(t.Context(), u.share.ID, f); err != nil {
		t.Fatal(err)
	}
	got, err := u.fs.Store.Files().Get(t.Context(), u.fs.Admin, h)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Modify.Equal(first) || !got.Change.Equal(second) {
		t.Errorf("modify %v, change %v; want modify %v, change %v", got.Modify, got.Change, first, second)
	}
}

// heldBatch holds the group commit's next batch after its capture until
// release is called.
func heldBatch(u *unit) (captured <-chan struct{}, release func()) {
	c, r := make(chan struct{}), make(chan struct{})
	var once sync.Once
	u.e.OnCapture(func() {
		once.Do(func() {
			close(c)
			<-r
		})
	})
	return c, sync.OnceFunc(func() { close(r) })
}

// While a batch is held between its capture and its commit, the captured
// writes are still answered by Overlay, a waitBatch on the file waits it out,
// and a commitNow for a write staged after the capture waits for the next
// batch, not the held one.
func TestABatchInFlight(t *testing.T) {
	u := newUnit(t)
	f := u.create(t, "a")
	u.write(t, f, 0, []byte("abc"))
	captured, release := heldBatch(u)
	defer release()
	first := make(chan error, 1)
	go func() { first <- u.e.CommitNow(context.Background(), u.share.ID, f) }()
	<-captured

	if o, pending, err := u.e.Overlay(t.Context(), u.share.ID, f); err != nil || !pending || o.Size != 3 {
		t.Errorf("Overlay during the batch: %+v, pending %v, %v; want size 3 pending", o, pending, err)
	}

	u.write(t, f, 3, []byte("def")) // after the capture
	waited := make(chan error, 1)
	go func() { waited <- u.e.WaitBatch(context.Background(), f) }()
	second := make(chan error, 1)
	var leftAfterSecond atomic.Int32
	go func() {
		err := u.e.CommitNow(context.Background(), u.share.ID, f)
		leftAfterSecond.Store(int32(len(u.e.Pending(f))))
		second <- err
	}()
	select {
	case <-waited:
		t.Fatal("waitBatch returned while the batch is held")
	case <-second:
		t.Fatal("commitNow of a later write returned while only the held batch ran")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	if err := <-waited; err != nil {
		t.Fatal(err)
	}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if n := leftAfterSecond.Load(); n != 0 {
		t.Errorf("commitNow of the later write returned with %d operations still pending", n)
	}
	if u.e.HasEntry(f) {
		t.Errorf("after both batches f holds %+v", u.e.Pending(f))
	}
}

// A write that finds the entry just before a commit drops it lands in a live
// entry: Overlay answers it, the next batch commits it and its Done runs once.
func TestAWriteRacingTheEntrysDropIsNotOrphaned(t *testing.T) {
	u := newUnit(t)
	f := u.create(t, "a")
	u.write(t, f, 0, []byte("abc"))
	var dones atomic.Int32
	var hook sync.Once
	lookedUp, dropped := make(chan struct{}), make(chan struct{})
	u.e.OnLookup(func() {
		hook.Do(func() {
			close(lookedUp)
			<-dropped
		})
	})
	wrote := make(chan error, 1)
	go func() {
		_, err := u.e.Write(context.Background(), u.share.ID, f, 3, []byte("def"), engine.WriteOpts{Done: func() { dones.Add(1) }})
		wrote <- err
	}()
	<-lookedUp
	if err := u.e.CommitNow(t.Context(), u.share.ID, f); err != nil {
		t.Fatal(err)
	}
	if u.e.HasEntry(f) {
		t.Fatal("the commit left the entry")
	}
	close(dropped)
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}
	if o, pending, _ := u.e.Overlay(t.Context(), u.share.ID, f); !pending || o.Size != 6 {
		t.Fatalf("Overlay after the racing write: %+v, pending %v; want size 6", o, pending)
	}
	if err := u.e.CommitNow(t.Context(), u.share.ID, f); err != nil {
		t.Fatal(err)
	}
	if n := dones.Load(); n != 1 || u.e.HasEntry(f) {
		t.Errorf("the racing write's Done ran %d times, entry %v; want once, none", n, u.e.HasEntry(f))
	}
}

// A share dropped between a batch's take and its capture leaves the batch
// with the files it took: they commit, and the drop returns once the batch
// does.
func TestADropBetweenTakeAndCapture(t *testing.T) {
	u := newUnit(t)
	f := u.create(t, "a")
	u.write(t, f, 0, []byte("abc"))
	dropped := make(chan error, 1)
	var once sync.Once
	u.e.OnTake(func() {
		once.Do(func() {
			go func() { dropped <- u.e.DropShare(context.Background(), u.rec, u.share.ID) }()
			time.Sleep(20 * time.Millisecond) // the drop runs while the batch holds its files
		})
	})
	if err := u.e.CommitNow(t.Context(), u.share.ID, f); err != nil {
		t.Fatal(err)
	}
	if err := <-dropped; err != nil {
		t.Fatal(err)
	}
	if u.e.HasEntry(f) {
		t.Errorf("the taken file was not committed: %+v", u.e.Pending(f))
	}
}

// A file due sooner than the loop's timer wakes the loop: with existence_age
// shortened after a first write set a long timer, a second file is committed
// at the new age.
func TestASoonerFileWakesTheLoop(t *testing.T) {
	u := newUnit(t)
	apply := func(age time.Duration) {
		if err := u.e.Apply(engine.Settings{ExistenceAge: age, GroupCommitMaxFiles: 64, CloneMaxLen: 1 << 30}); err != nil {
			t.Fatal(err)
		}
	}
	apply(time.Hour)
	a, b := u.create(t, "a"), u.create(t, "b")
	u.write(t, a, 0, []byte("a"))
	eventually(t, "the hour's timer is set", func() bool { return u.clock.Pending() == 1 })
	apply(time.Second)
	u.write(t, b, 0, []byte("b"))
	eventually(t, "a timer for the second's due time is set", func() bool { return u.clock.Pending() == 2 })
	u.clock.Advance(time.Second)
	eventually(t, "the second file commits at the new age", func() bool { return !u.e.HasEntry(b) })
}

// A store that never answers does not hold Close: the batch's context is
// cancelled and the loop joins.
func TestCloseCancelsAStalledBatch(t *testing.T) {
	stall := &ctxStall{entered: make(chan struct{}, 1)}
	u := newUnit(t, func(d *engine.Deps) { stall.Existence = d.Existence; d.Existence = stall })
	f := u.create(t, "a")
	u.write(t, f, 0, []byte("abc"))
	go func() { _ = u.e.CommitNow(context.Background(), u.share.ID, f) }()
	<-stall.entered
	closed := make(chan struct{})
	go func() { u.e.CloseGroupCommits(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close waited on a store that never answers")
	}
}

// ctxStall is a store whose existence commits wait for their context's end.
type ctxStall struct {
	engine.Existence
	entered chan struct{}
}

func (x *ctxStall) Commit(ctx context.Context, _ []kvstore.PendingExistence, _ *kvstore.MarksWrite) (map[entity.FileID]error, error) {
	select {
	case x.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// After Close no group commit starts again: a share bound after it answers
// its writes unavailable, and the leak check finds no goroutine.
func TestNoGroupCommitStartsAfterClose(t *testing.T) {
	u := newUnit(t)
	u.e.CloseGroupCommits()
	s := enginetest.AddShare(t, u.fs.Store, "late", 1<<20)
	if err := u.e.Bind(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	if _, err := u.e.Write(t.Context(), s.ID, entity.NewFileID(), 0, []byte("x"), engine.WriteOpts{Done: func() {}}); !errors.Is(err, vfserr.ErrUnavailable) {
		t.Errorf("a write after Close: %v, want ErrUnavailable", err)
	}
}

// commitAll waits for a batch already in flight, though the due set is
// empty while it runs.
func TestCommitAllWaitsForTheBatchInFlight(t *testing.T) {
	u := newUnit(t)
	f := u.create(t, "a")
	u.write(t, f, 0, []byte("abc"))
	captured, release := heldBatch(u)
	defer release()
	go func() { _ = u.e.CommitNow(context.Background(), u.share.ID, f) }()
	<-captured
	all := make(chan error, 1)
	go func() { all <- u.e.CommitAll(context.Background(), u.rec) }()
	select {
	case <-all:
		t.Fatal("commitAll returned while a batch is in flight")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	if err := <-all; err != nil {
		t.Fatal(err)
	}
	if u.e.HasEntry(f) {
		t.Errorf("commitAll returned with f still pending: %+v", u.e.Pending(f))
	}
}

// A file whose operations start with one no batch captures stays on the due
// set, so its writes are taken once that operation is gone.
func TestAFileHeldByAnUncapturedOperationStaysDue(t *testing.T) {
	u := newUnit(t)
	if err := u.e.Apply(engine.Settings{ExistenceAge: time.Second, GroupCommitMaxFiles: 64, CloneMaxLen: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	f := u.create(t, "a")
	u.write(t, f, 0, []byte("abc"))
	u.e.AddOp(f, 1, true, 0, 0) // below the write: the entry starts with it
	taken := make(chan struct{})
	var once sync.Once
	u.e.OnTake(func() { once.Do(func() { close(taken) }) })
	eventually(t, "the group commit's timer is set", func() bool { return u.clock.Pending() > 0 })
	u.clock.Advance(time.Second)
	<-taken
	eventually(t, "f is back on the due set", func() bool { return u.e.Due(u.rec, f) })
}

// settlesLog is a handle that records each Settle.
type settlesLog struct {
	engine.Handle
	mu      sync.Mutex
	settled map[journal.FileID]journal.Version
}

func (h *settlesLog) Settle(id journal.FileID, v journal.Version) {
	h.mu.Lock()
	h.settled[id] = v
	h.mu.Unlock()
	h.Handle.Settle(id, v)
}

// A committed file is settled in the journal through the newest version its
// batch committed, and one staged after the capture is not.
func TestACommitSettlesTheJournal(t *testing.T) {
	var log *settlesLog
	u := newUnitWith(t, unitConfig{wrap: func(h engine.Handle) engine.Handle {
		log = &settlesLog{Handle: h, settled: map[journal.FileID]journal.Version{}}
		return log
	}})
	f := u.create(t, "a")
	u.write(t, f, 0, []byte("one"))
	u.write(t, f, 3, []byte("two"))
	newest := u.e.Pending(f)[1].Version
	var once sync.Once
	u.e.OnCapture(func() { once.Do(func() { u.write(t, f, 6, []byte("late")) }) })
	if err := u.e.CommitNow(t.Context(), u.share.ID, f); err != nil {
		t.Fatal(err)
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	if got, ok := log.settled[journal.NewFileID(f)]; !ok || got != newest {
		t.Errorf("settled through %v (%v), want the batch's newest %v", got, ok, newest)
	}
}

// A file the store fails on its own never fails another: the others'
// overwrite Commits succeed beside it, its own answers ErrDelay, and
// existence_commit_failing names it once it fails alone.
func TestAFailingFileFailsNoOther(t *testing.T) {
	u := newUnit(t)
	a := committedFile(t, u, "a", 4)
	b := committedFile(t, u, "b", 4)
	_, dir := u.fs.Create(t, u.fs.Admin, u.fs.Root, "d", kvstore.Dir(0o755))
	d := dir.ID
	u.write(t, a, 0, []byte("over"))
	u.write(t, b, 0, []byte("over"))
	u.write(t, d, 0, []byte("not content"))
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i, f := range []entity.FileID{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, _, errs[i] = u.e.Commit(ctx, u.share.ID, f, 0, 4)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		errs[2] = u.e.CommitNow(ctx, u.share.ID, d)
	}()
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Errorf("the other files' Commits: %v, %v; want both answered", errs[0], errs[1])
	}
	if !errors.Is(errs[2], context.DeadlineExceeded) {
		t.Errorf("the failing file's commit: %v, want its deadline", errs[2])
	}
	if !u.e.Failing(u.rec, d) || u.e.Failing(u.rec, a) {
		t.Errorf("existence_commit_failing names d %v and a %v; want d alone", u.e.Failing(u.rec, d), u.e.Failing(u.rec, a))
	}
}

// conflicting is a store whose existence commits, once set, answer a
// conflict.
type conflicting struct {
	engine.Existence
	on    atomic.Bool
	calls atomic.Int32 // the commits answered a conflict
}

func (x *conflicting) Commit(ctx context.Context, p []kvstore.PendingExistence, marks *kvstore.MarksWrite) (map[entity.FileID]error, error) {
	if x.on.Load() {
		x.calls.Add(1)
		return nil, kvstore.ErrConflict
	}
	return x.Existence.Commit(ctx, p, marks)
}

// A conflict unresolved at a Commit's deadline is ErrDelay with
// DelayConflict, never an I/O error.
func TestAConflictAtTheDeadlineIsDelayConflict(t *testing.T) {
	c := &conflicting{}
	u := newUnit(t, func(d *engine.Deps) { c.Existence = d.Existence; d.Existence = c })
	f := committedFile(t, u, "a", 4)
	c.on.Store(true)
	u.write(t, f, 0, []byte("over"))
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	_, _, err := u.e.Commit(ctx, u.share.ID, f, 0, 4)
	var d *vfserr.Delay
	if !errors.As(err, &d) || d.Cause != vfserr.DelayConflict {
		t.Errorf("Commit under a conflict: %v, want ErrDelay(DelayConflict)", err)
	}
	// The file failed alone, so it backs off by itself: the clock has not
	// moved past its backoff, so it was not tried again.
	if n := c.calls.Load(); n != 1 {
		t.Errorf("the failing file was tried %d times within its backoff, want once", n)
	}
	if !u.e.Failing(u.rec, f) {
		t.Error("existence_commit_failing does not name the failing file")
	}
	c.on.Store(false)
	u.clock.Advance(time.Second) // past the backoff
	if err := u.e.CommitNow(t.Context(), u.share.ID, f); err != nil {
		t.Fatal(err)
	}
	if u.e.Failing(u.rec, f) {
		t.Error("existence_commit_failing still names the file after its commit succeeded")
	}
}

// A file the store names in its map is retried alone in the next batch, even
// with another file due beside it.
func TestAFailedFileIsRetriedAlone(t *testing.T) {
	u, log := newLoggedUnit(t, 24*time.Hour, 64)
	_, dir := u.fs.Create(t, u.fs.Admin, u.fs.Root, "d", kvstore.Dir(0o755))
	d, a, c := dir.ID, u.create(t, "a"), u.create(t, "c")
	u.write(t, d, 0, []byte("not content"))
	u.write(t, a, 0, []byte("a"))
	var once sync.Once
	u.e.OnCapture(func() {
		once.Do(func() {
			u.write(t, c, 0, []byte("c"))
			go func() { _ = u.e.CommitNow(context.Background(), u.share.ID, c) }()
			eventually(t, "c is due now", func() bool { return u.e.Due(u.rec, c) })
		})
	})
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	_ = u.e.CommitAll(ctx, u.rec)
	log.mu.Lock()
	defer log.mu.Unlock()
	if len(log.commits) < 2 {
		t.Fatalf("%d commits, want the batch and the retry", len(log.commits))
	}
	if r := log.commits[1]; len(r) != 1 || r[0] != d {
		t.Errorf("the retry's batch holds %d files, want d alone", len(r))
	}
}

// conflictingKV is a kv.KV whose writes, once on, conflict until their
// context ends, as a file another writer keeps changing does.
type conflictingKV struct {
	kv.KV
	on *atomic.Bool
}

func (c conflictingKV) Update(ctx context.Context, fn func(kv.Txn) error) error {
	if !c.on.Load() {
		return c.KV.Update(ctx, fn)
	}
	return kv.Retry(ctx, func() error { return kv.ErrConflict })
}

// A conflict the store keeps retrying ends at the commit's own bound: the
// overwrite's Commit answers DelayConflict at its deadline, and once the
// conflict clears the journal's group commit commits again.
func TestAConflictTheStoreKeepsRetryingHoldsNoBatch(t *testing.T) {
	engine.SetConflictBound(50 * time.Millisecond)
	t.Cleanup(func() { engine.SetConflictBound(time.Second) })
	on := new(atomic.Bool)
	u := newUnitOver(t, func(k kv.KV) kv.KV { return conflictingKV{k, on} })
	f := committedFile(t, u, "a", 4)
	on.Store(true)
	u.write(t, f, 0, []byte("over"))
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, _, err := u.e.Commit(ctx, u.share.ID, f, 0, 4)
	var d *vfserr.Delay
	if !errors.As(err, &d) || d.Cause != vfserr.DelayConflict {
		t.Errorf("Commit under a conflict the store keeps retrying: %v, want ErrDelay(DelayConflict)", err)
	}
	on.Store(false)
	u.clock.Advance(time.Minute) // past the backoff
	ctx, cancel = context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := u.e.CommitNow(ctx, u.share.ID, f); err != nil {
		t.Errorf("after the conflict cleared: %v; the group commit is still held", err)
	}
}

// conflictingFile is a store whose existence commits conflict whenever they
// hold file x, counting those calls.
type conflictingFile struct {
	engine.Existence
	x     entity.FileID
	calls atomic.Int32
}

func (s *conflictingFile) Commit(ctx context.Context, p []kvstore.PendingExistence, marks *kvstore.MarksWrite) (map[entity.FileID]error, error) {
	for _, pe := range p {
		if pe.File == s.x {
			s.calls.Add(1)
			return nil, errors.Join(kvstore.ErrConflict, context.DeadlineExceeded)
		}
	}
	return s.Existence.Commit(ctx, p, marks)
}

// A file a split left failing alone has already failed alone: it backs off
// at once, with no further solo try, and existence_commit_failing names it.
func TestASplitLeafBacksOffAtOnce(t *testing.T) {
	s := &conflictingFile{}
	u := newUnit(t, func(d *engine.Deps) { s.Existence = d.Existence; d.Existence = s })
	a, x := u.create(t, "a"), u.create(t, "x")
	s.x = x
	u.write(t, a, 0, []byte("a"))
	u.write(t, x, 0, []byte("x"))
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	_ = u.e.CommitAll(ctx, u.rec)
	if n := s.calls.Load(); n != 2 {
		t.Errorf("x was sent %d times, want twice: with a, then alone by the split", n)
	}
	if !u.e.Failing(u.rec, x) {
		t.Error("existence_commit_failing does not name the split's failing leaf")
	}
	if u.e.HasEntry(a) {
		t.Error("the file beside it did not commit")
	}
}

// A failing file whose writes leave another way is no longer named failing:
// its journal's device lost, or its share dropped.
func TestAFailingFileLeftAnotherWayIsCleared(t *testing.T) {
	for _, how := range []string{"lost whole", "share dropped"} {
		t.Run(how, func(t *testing.T) {
			u := newUnit(t)
			_, dir := u.fs.Create(t, u.fs.Admin, u.fs.Root, "d", kvstore.Dir(0o755))
			u.write(t, dir.ID, 0, []byte("not content"))
			ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
			defer cancel()
			_ = u.e.CommitNow(ctx, u.share.ID, dir.ID)
			if !u.e.Failing(u.rec, dir.ID) {
				t.Fatal("the directory's commit is not named failing")
			}
			if how == "lost whole" {
				u.e.LostWhole(u.rec)
			} else if err := u.e.DropShare(t.Context(), u.rec, u.share.ID); err != nil {
				t.Fatal(err)
			}
			if u.e.Failing(u.rec, dir.ID) {
				t.Errorf("after the %s, existence_commit_failing still names it", how)
			}
		})
	}
}

// corruptKV is a kv.KV whose reads of any key naming file f fail, as a
// record of f the store cannot decode does: a whole-call error of the
// existence commit that names no file.
type corruptKV struct {
	kv.KV
	f *entity.FileID
}

func (c corruptKV) Update(ctx context.Context, fn func(kv.Txn) error) error {
	return c.KV.Update(ctx, func(tx kv.Txn) error { return fn(corruptTxn{tx, c.f}) })
}

type corruptTxn struct {
	kv.Txn
	f *entity.FileID
}

func (t corruptTxn) bad(key []byte) bool {
	return *t.f != (entity.FileID{}) && bytes.Contains(key, t.f[:])
}

func (t corruptTxn) Get(key []byte) ([]byte, error) {
	if t.bad(key) {
		return nil, errors.New("a record that does not decode")
	}
	return t.Txn.Get(key)
}

func (t corruptTxn) GetMany(keys [][]byte) ([][]byte, error) {
	for _, k := range keys {
		if t.bad(k) {
			return nil, errors.New("a record that does not decode")
		}
	}
	return t.Txn.GetMany(keys)
}

// A file whose records the store cannot decode fails its batch's whole call,
// and the split leaves it alone: every other file of the batch commits.
func TestACorruptFileFailsNoOther(t *testing.T) {
	bad := new(entity.FileID)
	u := newUnitOver(t, func(k kv.KV) kv.KV { return corruptKV{k, bad} })
	a, b, x := u.create(t, "a"), u.create(t, "b"), u.create(t, "x")
	for _, f := range []entity.FileID{a, b, x} {
		u.write(t, f, 0, []byte("abc"))
	}
	*bad = x
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	_ = u.e.CommitAll(ctx, u.rec)
	if u.e.HasEntry(a) || u.e.HasEntry(b) {
		t.Errorf("the other files did not commit: a %v, b %v", u.e.HasEntry(a), u.e.HasEntry(b))
	}
	if !u.e.Failing(u.rec, x) {
		t.Error("existence_commit_failing does not name the corrupt file")
	}
}

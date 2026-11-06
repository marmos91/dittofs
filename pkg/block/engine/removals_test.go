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

// resuming is the store's content view recording each file's phase 2 and
// prune, and failing the phase 2 of the files in failing.
type resuming struct {
	engine.Content
	mu      sync.Mutex
	failing map[entity.FileID]bool
	resumes map[entity.FileID]int
	pruned  map[entity.FileID]entity.JournalVersion
}

func newResuming() *resuming {
	return &resuming{failing: map[entity.FileID]bool{}, resumes: map[entity.FileID]int{}, pruned: map[entity.FileID]entity.JournalVersion{}}
}

func (x *resuming) Resume(ctx context.Context, share entity.ShareID, file entity.FileID) error {
	x.mu.Lock()
	x.resumes[file]++
	fail := x.failing[file]
	x.mu.Unlock()
	if fail {
		return errors.New("an injected phase 2 failure")
	}
	return x.Content.Resume(ctx, share, file)
}

func (x *resuming) PruneRemovals(ctx context.Context, share entity.ShareID, file entity.FileID, atOrBelow entity.JournalVersion) error {
	err := x.Content.PruneRemovals(ctx, share, file, atOrBelow)
	if err == nil {
		x.mu.Lock()
		x.pruned[file] = atOrBelow
		x.mu.Unlock()
	}
	return err
}

func (x *resuming) prunedAt(f entity.FileID) (entity.JournalVersion, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	v, ok := x.pruned[f]
	return v, ok
}

func (x *resuming) resumed(f entity.FileID) int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.resumes[f]
}

// until advances the unit's clock past the loop's backoff until cond holds.
func (u *unit) until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("never: %s", what)
		}
		u.clock.Advance(time.Second)
		time.Sleep(time.Millisecond)
	}
}

// A truncate's removal runs its phase 2 on its journal's loop and is pruned
// at its own version, leaving no removal of the file unfinished.
func TestARemovalRunsItsPhase2AndIsPruned(t *testing.T) {
	x := newResuming()
	u := newUnit(t, func(d *engine.Deps) { x.Content = d.Content; d.Content = x })
	f := u.create(t, "a")
	u.write(t, f, 0, fill(16, 'a'))
	if err := u.e.CommitNow(t.Context(), u.share.ID, f); err != nil {
		t.Fatal(err)
	}
	if err := u.e.Truncate(t.Context(), u.share.ID, f, 4); err != nil {
		t.Fatal(err)
	}
	u.until(t, "the removal is pruned", func() bool { _, ok := x.prunedAt(f); return ok })
	if v, _ := x.prunedAt(f); v.IsZero() {
		t.Error("pruned at the zero version, not the removal's")
	}
	for r, err := range u.fs.Store.Content().UnfinishedRemovals(t.Context(), u.share.ID, 0) {
		if err != nil {
			t.Fatal(err)
		}
		if r.File == f {
			t.Errorf("after the loop, an unfinished removal of the file: %+v", r)
		}
	}
	if n := u.e.RemovalsPending(); n != 0 {
		t.Errorf("%d removals still pending on the loop", n)
	}
}

// A removal whose phase 2 keeps failing raises removal_stalled after five
// failures and wedges no other: another file's removal on the same journal
// completes meanwhile, and the stalled one finishes once the fault clears.
func TestAFailingRemovalWedgesNoOther(t *testing.T) {
	x := newResuming()
	u := newUnit(t, func(d *engine.Deps) { x.Content = d.Content; d.Content = x })
	f, g := u.create(t, "f"), u.create(t, "g")
	for _, id := range []entity.FileID{f, g} {
		u.write(t, id, 0, fill(16, 'a'))
		if err := u.e.CommitNow(t.Context(), u.share.ID, id); err != nil {
			t.Fatal(err)
		}
	}
	x.mu.Lock()
	x.failing[f] = true
	x.mu.Unlock()
	if err := u.e.Truncate(t.Context(), u.share.ID, f, 4); err != nil {
		t.Fatal(err)
	}
	u.until(t, "f's phase 2 fails five times", func() bool { return x.resumed(f) >= 5 })
	if err := u.e.Truncate(t.Context(), u.share.ID, g, 4); err != nil {
		t.Fatal(err)
	}
	u.until(t, "g's removal is pruned while f's fails", func() bool { _, ok := x.prunedAt(g); return ok })
	u.until(t, "f is removal_stalled", func() bool { return u.e.RemovalStalled(f) })
	x.mu.Lock()
	x.failing[f] = false
	x.mu.Unlock()
	u.until(t, "f's removal is pruned once the fault clears", func() bool { _, ok := x.prunedAt(f); return ok })
	if u.e.RemovalStalled(f) {
		t.Error("f still removal_stalled after its removal finished")
	}
}

// stalledResume is the store's content view whose phase 2 of the files in
// stalled holds until its context ends.
type stalledResume struct {
	*resuming
	stalled entity.FileID
}

func (x *stalledResume) Resume(ctx context.Context, share entity.ShareID, file entity.FileID) error {
	if file == x.stalled {
		<-ctx.Done()
		return ctx.Err()
	}
	return x.resuming.Resume(ctx, share, file)
}

// A phase 2 whose store never answers is cut at the conflict bound, so the
// loop moves on: another file's removal is pruned meanwhile.
func TestAStalledPhase2HoldsNoOther(t *testing.T) {
	engine.SetConflictBound(10 * time.Millisecond)
	t.Cleanup(func() { engine.SetConflictBound(time.Second) })
	x := &stalledResume{resuming: newResuming()}
	u := newUnit(t, func(d *engine.Deps) { x.Content = d.Content; d.Content = x })
	f, g := u.create(t, "f"), u.create(t, "g")
	x.stalled = f
	for _, id := range []entity.FileID{f, g} {
		u.write(t, id, 0, fill(16, 'a'))
		if err := u.e.CommitNow(t.Context(), u.share.ID, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := u.e.Truncate(t.Context(), u.share.ID, f, 4); err != nil {
		t.Fatal(err)
	}
	if err := u.e.Truncate(t.Context(), u.share.ID, g, 4); err != nil {
		t.Fatal(err)
	}
	u.until(t, "g's removal is pruned while f's phase 2 stalls", func() bool { _, ok := x.prunedAt(g); return ok })
}

// refusing is the store's existence view refusing every removal as
// invalid, as the store refuses an operation it cannot apply.
type refusing struct {
	engine.Existence
	on bool
}

func (x *refusing) Remove(ctx context.Context, share entity.ShareID, file entity.FileID, epoch uint64, op kvstore.RemoveOp, pending []kvstore.PendingOp) error {
	if x.on {
		op.Kind = 0 // a kind the store refuses as invalid
	}
	return x.Existence.Remove(ctx, share, file, epoch, op, pending)
}

// A phase 1 the store refuses as invalid ends at once: the call answers an
// error, the guard is released and the truncate's operation leaves the
// entry, so the next removal of the file is not held behind it.
func TestARefusedPhase1EndsAndReleasesTheGuard(t *testing.T) {
	x := &refusing{on: true}
	u := newUnit(t, func(d *engine.Deps) { x.Existence = d.Existence; d.Existence = x })
	f := u.create(t, "a")
	u.write(t, f, 0, fill(16, 'a'))
	if err := u.e.CommitNow(t.Context(), u.share.ID, f); err != nil {
		t.Fatal(err)
	}
	if err := u.e.Truncate(t.Context(), u.share.ID, f, 4); err == nil {
		t.Fatal("a refused phase 1 answered nil")
	}
	if u.e.GuardHeld(f) || len(u.e.Pending(f)) != 0 || u.e.RemovalsPending() != 0 {
		t.Fatalf("after the refusal: guard %v, pending %+v, loop items %d; want none",
			u.e.GuardHeld(f), u.e.Pending(f), u.e.RemovalsPending())
	}
	x.on = false
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := u.e.Truncate(ctx, u.share.ID, f, 2); err != nil {
		t.Errorf("the next truncate: %v", err)
	}
}

// A phase 1 that ends on a terminal failure settles the journal's marker
// without the removal, so no later start re-applies it, and logs the loss
// of a file that still exists (RFC 8 §8.1).
func TestARefusedPhase1SettlesItsMarker(t *testing.T) {
	x := &refusing{on: true}
	var logged lockedBuffer
	var log *settlesLog
	u := newUnitWith(t, unitConfig{
		deps: []func(*engine.Deps){func(d *engine.Deps) {
			x.Existence = d.Existence
			d.Existence = x
			d.Logger = slog.New(slog.NewTextHandler(&logged, nil))
		}},
		wrap: func(h engine.Handle) engine.Handle {
			log = &settlesLog{Handle: h, settled: map[journal.FileID]journal.Version{}}
			return log
		},
	})
	f := u.create(t, "a")
	u.write(t, f, 0, fill(16, 'a'))
	written := u.e.Pending(f)[0].Version
	if err := u.e.CommitNow(t.Context(), u.share.ID, f); err != nil {
		t.Fatal(err)
	}
	if err := u.e.Truncate(t.Context(), u.share.ID, f, 4); err == nil {
		t.Fatal("a refused phase 1 answered nil")
	}
	log.mu.Lock()
	got := log.settled[journal.NewFileID(f)]
	log.mu.Unlock()
	if got.Compare(written) <= 0 {
		t.Errorf("settled through %v after the refusal, want the truncate's version, above the write's %v", got, written)
	}
	if s := logged.String(); !strings.Contains(s, "level=ERROR") || !strings.Contains(s, "marker settled") {
		t.Errorf("the loss was not logged at Error: %q", s)
	}
}

// lockedBuffer is a bytes.Buffer a logger may write from any goroutine.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// endingStore is the store's existence view refusing every removal as
// invalid, so a removal ends on a terminal failure, and failing the next
// fail existence commits with a conflict, recording the operations of each
// commit it passed.
type endingStore struct {
	engine.Existence
	fail      atomic.Int32
	mu        sync.Mutex
	committed [][]kvstore.PendingOp
}

func (x *endingStore) Remove(ctx context.Context, share entity.ShareID, file entity.FileID, epoch uint64, op kvstore.RemoveOp, pending []kvstore.PendingOp) error {
	op.Kind = 0 // a kind the store refuses as invalid
	return x.Existence.Remove(ctx, share, file, epoch, op, pending)
}

func (x *endingStore) Commit(ctx context.Context, p []kvstore.PendingExistence, marks *kvstore.MarksWrite) (map[entity.FileID]error, error) {
	if x.fail.Add(-1) >= 0 {
		return nil, kvstore.ErrConflict
	}
	m, err := x.Existence.Commit(ctx, p, marks)
	if err == nil {
		x.mu.Lock()
		for _, pe := range p {
			x.committed = append(x.committed, pe.Ops)
		}
		x.mu.Unlock()
	}
	return m, err
}

func (x *endingStore) commits() [][]kvstore.PendingOp {
	x.mu.Lock()
	defer x.mu.Unlock()
	return append([][]kvstore.PendingOp(nil), x.committed...)
}

// endedTruncate writes 16 bytes at 0 of a new file and commits them, appends
// 16 more at 16 and makes them stable without committing their existence,
// as an append's stability point does, then truncates the file to 8 with
// the store refusing the removal. It answers the file, the append's
// version, and the store's size whenever the journal was settled.
func (u *unit) endedTruncate(t *testing.T, h *hookedHandle, lose bool) (f entity.FileID, appended journal.Version, sizes func() []int64) {
	t.Helper()
	var mu sync.Mutex
	var seen []int64
	f = u.create(t, "a")
	h.settle = func() {
		n, _ := u.fs.Store.Existence().Size(context.Background(), u.share.ID, f)
		mu.Lock()
		seen = append(seen, n)
		mu.Unlock()
	}
	u.write(t, f, 0, fill(16, 'a'))
	if err := u.e.CommitNow(t.Context(), u.share.ID, f); err != nil {
		t.Fatal(err)
	}
	u.write(t, f, 16, fill(16, 'b'))
	appended = u.e.Pending(f)[0].Version
	if _, _, err := u.e.Commit(t.Context(), u.share.ID, f, 0, 0); err != nil {
		t.Fatal(err)
	}
	if lose {
		u.lose(f, 16, 16)
	}
	if err := u.e.Truncate(t.Context(), u.share.ID, f, 8); err == nil {
		t.Fatal("a refused phase 1 answered nil")
	}
	return f, appended, func() []int64 {
		mu.Lock()
		defer mu.Unlock()
		return append([]int64(nil), seen...)
	}
}

// After a truncate's terminal failure the append below it, stable but not
// yet committed, has its existence committed before the journal's marker is
// settled, so a crash after the settle, which leaves only the store, reads
// the append's range lost rather than past the end of the file. Settling
// without that commit leaves the store at the size before the append.
func TestATerminalRemovalCommitsTheExistenceBelowItBeforeItsSettle(t *testing.T) {
	x := &endingStore{}
	u, h := newHookedUnit(t, func(d *engine.Deps) { x.Existence = d.Existence; d.Existence = x })
	f, appended, sizes := u.endedTruncate(t, h, false)
	if s := sizes(); len(s) != 2 || s[1] != 32 {
		t.Errorf("the store's size at each settle: %v, want 32 at the truncate's", s)
	}
	c := x.commits()
	if len(c) == 0 || c[len(c)-1][0].Version != entity.JournalVersion(appended.Bytes()) {
		t.Errorf("commits %+v, want the last to commit the append", c)
	}
	u.e.ClearOps(f) // the crash: the engine's state goes, the store stays
	if _, err := u.e.Read(t.Context(), u.share.ID, f, 16, make([]byte, 16)); !errors.Is(err, vfserr.ErrLost) {
		t.Errorf("after the crash the append reads %v, want ErrLost", err)
	}
}

// The same with the append's record lost, its loss synced: its existence is
// committed lost, from the loss record, before the settle drops the record.
func TestATerminalRemovalCommitsALossBelowItBeforeItsSettle(t *testing.T) {
	x := &endingStore{}
	u, h := newHookedUnit(t, func(d *engine.Deps) { x.Existence = d.Existence; d.Existence = x })
	_, appended, sizes := u.endedTruncate(t, h, true)
	if s := sizes(); len(s) != 2 || s[1] != 32 {
		t.Fatalf("the store's size at each settle: %v, want 32 at the truncate's", s)
	}
	c := x.commits()
	if len(c) == 0 {
		t.Fatal("nothing committed")
	}
	last := c[len(c)-1]
	if len(last) != 1 || last[0].Version != entity.JournalVersion(appended.Bytes()) || last[0].Kind != kvstore.OpLost {
		t.Errorf("the last commit %+v, want the append committed lost", last)
	}
}

// An existence commit that fails after a terminal failure is retried by the
// loop with the guard held, and the marker stays unsettled until it commits.
func TestATerminalRemovalsFailedCommitIsRetried(t *testing.T) {
	x := &endingStore{}
	u, h := newHookedUnit(t, func(d *engine.Deps) { x.Existence = d.Existence; d.Existence = x })
	f := u.create(t, "a")
	u.write(t, f, 0, fill(16, 'a'))
	const failing = 1 << 20
	x.fail.Store(failing)
	if err := u.e.Truncate(t.Context(), u.share.ID, f, 8); err == nil {
		t.Fatal("a refused phase 1 answered nil")
	}
	u.until(t, "the loop retrying the commit", func() bool { return x.fail.Load() <= failing-3 })
	if !u.e.GuardHeld(f) || len(h.settleLog()) != 0 {
		t.Fatalf("while the commit fails: guard %v, settles %+v; want the guard held and none", u.e.GuardHeld(f), h.settleLog())
	}
	x.fail.Store(0)
	u.until(t, "the retried commit", func() bool { return !u.e.GuardHeld(f) })
	if s := h.settleLog(); len(s) != 1 {
		t.Errorf("settles %+v, want one once the commit was retried", s)
	}
	if n := u.size(t, f); n != 16 {
		t.Errorf("the store's size %d, want the write's 16", n)
	}
}

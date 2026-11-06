package engine_test

import (
	"context"
	"errors"
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

// verifierOf is the verifier a one-byte write of a new file answers.
func (u *unit) verifierOf(t *testing.T) vfserr.Verifier {
	t.Helper()
	v, err := u.e.Write(t.Context(), u.share.ID, entity.NewFileID(), 0, []byte("x"), engine.WriteOpts{Done: func() {}})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// Writes in one process and binding answer one verifier however far the
// clock moved, and raising any one of its inputs alone answers another.
func TestTheVerifierChangesWithEachInputAndNothingElse(t *testing.T) {
	u := newUnit(t)
	node := entity.NodeID{1}
	u.e.SetPrimary(u.share.ID, node, 1, 1)
	v := u.verifierOf(t)
	u.clock.Advance(time.Hour)
	if got := u.verifierOf(t); got != v {
		t.Fatalf("an hour later the verifier is %x, want %x: the clock is not an input", got, v)
	}
	for _, c := range []struct {
		name  string
		raise func()
	}{
		{"the node", func() { u.e.SetPrimary(u.share.ID, entity.NodeID{2}, 1, 1) }},
		{"the node epoch", func() { u.e.SetPrimary(u.share.ID, node, 2, 1) }},
		{"the incarnation", func() { u.e.SetPrimary(u.share.ID, node, 1, 2) }},
		{"the process instance", u.e.NewInstance},
		{"the loss generation", func() {
			f := entity.NewFileID()
			u.write(t, f, 0, []byte("lost"))
			u.lose(f, 0, 4)
		}},
	} {
		u.e.SetPrimary(u.share.ID, node, 1, 1) // the others at their base
		base := u.verifierOf(t)
		c.raise()
		if got := u.verifierOf(t); got == base {
			t.Errorf("with %s raised alone the verifier is unchanged, %x", c.name, got)
		}
	}
	// Each input has its own place in the derivation: two that trade a step
	// between them answer two verifiers.
	u.e.SetPrimary(u.share.ID, node, 2, 1)
	a := u.verifierOf(t)
	u.e.SetPrimary(u.share.ID, node, 1, 2)
	if b := u.verifierOf(t); a == b {
		t.Errorf("node epoch 2 with incarnation 1 and the reverse answer one verifier, %x", a)
	}
}

// loseDuring is a handle whose WriteAt runs *lose, when set, once the write
// is staged, before the engine answers it: a test sets it to raise the
// journal's loss generation.
type loseDuring struct {
	engine.Handle
	lose *func()
}

func (h loseDuring) WriteAt(ctx context.Context, id journal.FileID, off int64, p []byte, mtime int64) (journal.Version, error) {
	v, err := h.Handle.WriteAt(ctx, id, off, p, mtime)
	if *h.lose != nil {
		(*h.lose)()
	}
	return v, err
}

// A write's verifier is sampled before it is staged: a loss between the
// staging and the answer leaves that write's verifier and changes the next.
func TestALossDuringAWriteShowsInTheNextVerifier(t *testing.T) {
	var lose func()
	u := newUnitWith(t, unitConfig{wrap: func(h engine.Handle) engine.Handle { return loseDuring{h, &lose} }})
	before := u.verifierOf(t)
	f := entity.NewFileID()
	u.write(t, f, 0, []byte("held"))
	lose = func() { u.lose(f, 0, 4) }
	if got := u.verifierOf(t); got != before {
		t.Errorf("the write a loss followed answered %x, want the %x sampled before it", got, before)
	}
	lose = nil
	if got := u.verifierOf(t); got == before {
		t.Errorf("the write after the loss answered the verifier from before it, %x", got)
	}
}

// committedFile is a new file of the unit's share holding n committed bytes.
func committedFile(t *testing.T, u *unit, name string, n int) entity.FileID {
	t.Helper()
	_, made := u.fs.Create(t, u.fs.Admin, u.fs.Root, name, kvstore.Regular(0o644))
	u.write(t, made.ID, 0, make([]byte, n))
	if err := u.e.CommitNow(t.Context(), u.share.ID, made.ID); err != nil {
		t.Fatal(err)
	}
	return made.ID
}

func (l *callLog) count(c string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, x := range l.calls {
		if x == c {
			n++
		}
	}
	return n
}

// A Commit over an append syncs and makes no existence commit; one over an
// overwrite of committed content returns only once its existence commits.
func TestCommitWaitsOnlyForAnOverwrite(t *testing.T) {
	u, log := newLoggedUnit(t, 24*time.Hour, 64)
	f := committedFile(t, u, "a", 4)
	commits := log.count("commit")

	u.write(t, f, 4, []byte("tail"))
	if _, _, err := u.e.Commit(t.Context(), u.share.ID, f, 4, 4); err != nil {
		t.Fatal(err)
	}
	if n := log.count("commit") - commits; n != 0 {
		t.Errorf("an append's Commit made %d existence commits, want none", n)
	}
	if len(u.e.Pending(f)) != 1 {
		t.Errorf("the append is no longer pending: %+v", u.e.Pending(f))
	}

	u.write(t, f, 0, []byte("over"))
	if _, _, err := u.e.Commit(t.Context(), u.share.ID, f, 0, 4); err != nil {
		t.Fatal(err)
	}
	if n := log.count("commit") - commits; n != 1 {
		t.Errorf("an overwrite's Commit made %d existence commits, want 1", n)
	}
	if u.e.HasEntry(f) {
		t.Errorf("after the overwrite's Commit f still holds %+v", u.e.Pending(f))
	}
	if n := u.e.StabilityPoints(); n != 2 {
		t.Errorf("engine_stability_points_total %d, want 2", n)
	}
}

// armed is a test's hook a wrapped handle runs, once the test sets it.
type armed struct{ f atomic.Pointer[func()] }

func (a *armed) set(f func()) { a.f.Store(&f) }

// run runs the hook if set, and reports whether it was.
func (a *armed) run() bool {
	f := a.f.Load()
	if f != nil {
		(*f)()
	}
	return f != nil
}

// stagingDuringSync is a handle whose first Sync once armed stages an
// operation of file f that no batch commits, as an operation staged during
// the sync.
type stagingDuringSync struct {
	engine.Handle
	stage *sync.Once
	f     *armed
}

func (h stagingDuringSync) Sync(ctx context.Context, ids ...journal.FileID) error {
	if h.f.f.Load() != nil {
		h.stage.Do(func() { h.f.run() })
	}
	return h.Handle.Sync(ctx, ids...)
}

// A Commit over an overwrite waits for the operations staged before its
// sync, never for one staged during it.
func TestCommitWaitsOnlyBelowItsSyncsVersion(t *testing.T) {
	stage := &armed{}
	u := newUnitWith(t, unitConfig{wrap: func(h engine.Handle) engine.Handle {
		return stagingDuringSync{h, new(sync.Once), stage}
	}})
	f := committedFile(t, u, "a", 4)
	u.write(t, f, 0, []byte("over"))
	stage.set(func() { u.e.AddOp(f, unitLimit-1, true, 8, 0) })
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, _, err := u.e.Commit(ctx, u.share.ID, f, 0, 4); err != nil {
		t.Errorf("Commit: %v; it waited for the operation staged during its sync", err)
	}
}

// stalledExistence is a store whose existence commits, once stalled, wait
// until release.
type stalledExistence struct {
	engine.Existence
	stalled atomic.Bool
	release chan struct{}
}

func (x *stalledExistence) Commit(ctx context.Context, p []kvstore.PendingExistence, marks *kvstore.MarksWrite) (map[entity.FileID]error, error) {
	if x.stalled.Load() {
		select {
		case <-x.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return x.Existence.Commit(ctx, p, marks)
}

// Under a store that commits nothing, an append's Commit still answers after
// its sync, and an overwrite's answers ErrDelay with DelayCommit at its
// deadline, never success.
func TestCommitUnderAStalledStore(t *testing.T) {
	stall := &stalledExistence{release: make(chan struct{})}
	u := newUnit(t, func(d *engine.Deps) { stall.Existence = d.Existence; d.Existence = stall })
	t.Cleanup(func() { close(stall.release) }) // before the group commit is closed
	over := committedFile(t, u, "over", 4)
	app := committedFile(t, u, "append", 4)
	stall.stalled.Store(true)

	u.write(t, over, 0, []byte("over"))
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, _, err := u.e.Commit(ctx, u.share.ID, over, 0, 4)
	var d *vfserr.Delay
	if !errors.As(err, &d) || d.Cause != vfserr.DelayCommit {
		t.Errorf("an overwrite's Commit under a stalled store: %v, want ErrDelay(DelayCommit)", err)
	}

	u.write(t, app, 4, []byte("tail"))
	ctx, cancel = context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, _, err := u.e.Commit(ctx, u.share.ID, app, 4, 4); err != nil {
		t.Errorf("an append's Commit under a stalled store: %v, want it answered", err)
	}
}

// A Commit waits only for an overwrite in the extent it names: one over an
// append beside a pending overwrite makes no existence commit.
func TestCommitWaitsOnlyInItsExtent(t *testing.T) {
	u, log := newLoggedUnit(t, 24*time.Hour, 64)
	f := committedFile(t, u, "a", 4)
	commits := log.count("commit")
	u.write(t, f, 0, []byte("over"))
	u.write(t, f, 4, []byte("tail"))
	if _, _, err := u.e.Commit(t.Context(), u.share.ID, f, 4, 4); err != nil {
		t.Fatal(err)
	}
	if n := log.count("commit") - commits; n != 0 {
		t.Errorf("a Commit of the append alone made %d existence commits, want none", n)
	}
}

// losingSync is a handle whose Sync, once armed, raises the journal's loss
// generation before it returns.
type losingSync struct {
	engine.Handle
	lose *armed
}

func (h losingSync) Sync(ctx context.Context, ids ...journal.FileID) error {
	h.lose.run()
	return h.Handle.Sync(ctx, ids...)
}

// Commit samples its verifier after its sync: a loss during the sync shows in
// the verifier Commit answers, though not in the write's.
func TestCommitSamplesAfterItsSync(t *testing.T) {
	lose := &armed{}
	u := newUnitWith(t, unitConfig{wrap: func(h engine.Handle) engine.Handle { return losingSync{h, lose} }})
	f, g := entity.NewFileID(), u.create(t, "g")
	u.write(t, g, 0, []byte("held"))
	v, err := u.e.Write(t.Context(), u.share.ID, f, 0, []byte("x"), engine.WriteOpts{Done: func() {}})
	if err != nil {
		t.Fatal(err)
	}
	lose.set(func() { u.lose(g, 0, 4) })
	cv, seq, err := u.e.Commit(t.Context(), u.share.ID, g, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if after := u.verifierOf(t); cv == v || cv != after {
		t.Errorf("Commit answered verifier %x; want the %x sampled after the loss during its sync, not the write's %x", cv, after, v)
	}
	if seq != 1 {
		t.Errorf("Commit answered loss sequence %d for the file the sync lost, want 1", seq)
	}
}

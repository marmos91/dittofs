package engine

import (
	"context"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// ErrNotStarted is the answer of a Bind while no journal is open.
var ErrNotStarted = errNotStarted

// The errors start answers and refuses shares with, for a test to match.
var (
	ErrUnbuilt      = errUnbuilt
	ErrStarted      = errStarted
	ErrJournalLost  = errJournalLost
	ErrNotRecorded  = errNotRecorded
	ErrUnidentified = errUnidentified
)

// SetJournalsForTest sets the journals the engine has open, as Start will.
func (e *Engine) SetJournalsForTest(js ...Journal) {
	e.mu.Lock()
	e.journals = js
	e.mu.Unlock()
}

// MarkRaisedForTest records shard as raised by this process, as Start will.
func (e *Engine) MarkRaisedForTest(shard entity.ShardID) {
	e.mu.Lock()
	e.raised[shard] = struct{}{}
	e.mu.Unlock()
}

// TableForTest is the share table's pointer, compared for identity.
func (e *Engine) TableForTest() any {
	return e.shares.Load()
}

// StateOf is share s's binding state, zero when it has no binding.
func (e *Engine) StateOf(s entity.ShareID) ShareState {
	b := e.table()[s]

	if b == nil {
		return 0
	}

	return ShareState(b.state.Load())
}

// LifecycleFree reports whether the lifecycle mutex is free now.
func (e *Engine) LifecycleFree() bool {
	if !e.mu.TryLock() {
		return false
	}

	e.mu.Unlock()

	return true
}

// HoldLifecycle takes the lifecycle mutex and returns the function that
// releases it.
func (e *Engine) HoldLifecycle() (release func()) {
	e.mu.Lock()

	return e.mu.Unlock
}

// BindingOf reports whether share s is bound, through the content path's
// own lookup.
func (e *Engine) BindingOf(s entity.ShareID) error {
	_, err := e.bindingOf(s)
	return err
}

// FileEntries is the count of files holding an entry.
func (e *Engine) FileEntries() int {
	n := 0
	for i := range e.files.shards {
		sh := &e.files.shards[i]
		sh.mu.Lock()
		n += len(sh.m)
		sh.mu.Unlock()
	}
	return n
}

// HasEntry reports whether f has an entry, through entryOf.
func (e *Engine) HasEntry(f entity.FileID) bool { return e.files.entryOf(f) != nil }

// SameShard reports whether a and b share a shard of the entry map.
func (e *Engine) SameShard(a, b entity.FileID) bool { return e.files.shardOf(a) == e.files.shardOf(b) }

// HoldEntry takes f's entry lock, inserting the entry, and returns the
// function that releases it, as a call adding to it would.
func (e *Engine) HoldEntry(f entity.FileID) func() {
	fe := e.files.lockFor(f)
	return func() { e.files.unlock(f, fe) }
}

// AddPending adds a pending write of [off, off+n) to f's entry, and returns
// the function that drops it again, as a commit would, bumping the removal
// counter.
func (e *Engine) AddPending(f entity.FileID, off, n int64) (drop func()) {
	fe := e.files.lockFor(f)
	fe.ops = append(fe.ops, pendingOp{kind: opWrite})
	fe.ops[len(fe.ops)-1].extent.Off, fe.ops[len(fe.ops)-1].extent.Len = off, n
	e.files.unlock(f, fe)
	return func() {
		fe := e.files.lockOf(f)
		if fe == nil {
			return
		}
		if len(fe.ops) > 0 {
			fe.ops = fe.ops[1:]
			e.files.removals.Add(1)
		}
		e.files.unlock(f, fe)
	}
}

// FileStateWaits is the file_state lock-wait series: its count and total.
func (e *Engine) FileStateWaits() (int64, time.Duration) {
	return e.files.waits.count.Load(), time.Duration(e.files.waits.total.Load())
}

// FileStateWaiting is the callers waiting for an entry lock now.
func (e *Engine) FileStateWaiting() int64 { return e.files.waits.waiting.Load() }

// PendingOpType is the pending operation's type, for a check of its fields.
type PendingOpType = pendingOp

// SetSample replaces how a write samples its verifier.
func (e *Engine) SetSample(f func() vfserr.Verifier) {
	e.sample = func(*binding) vfserr.Verifier { return f() }
}

// SetPrimary sets share s's verifier inputs from its shard's primary, as
// Bind will from the start transaction, copying the share table as a
// binding's change does.
func (e *Engine) SetPrimary(s entity.ShareID, node entity.NodeID, nodeEpoch, incarnation uint64) {
	t := bindings{}
	for k, v := range *e.shares.Load() {
		t[k] = v
	}
	old := t[s]
	b := &binding{
		share:       old.share,
		j:           old.j,
		h:           old.h,
		incarnation: incarnation,
		primary:     primary{node, nodeEpoch},
		gc:          old.gc,
	}
	b.state.Store(old.state.Load())
	t[s] = b
	e.shares.Store(&t)
}

// NewInstance draws the process instance again, as a new process would.
func (e *Engine) NewInstance() { e.instance = instanceID() }

// PendingView is one pending operation as a test reads it.
type PendingView struct {
	Version   journal.Version
	Off, Len  int64
	At        time.Time
	SetsMtime bool
	HasDone   bool
}

// Pending is f's pending operations, in the entry's order.
func (e *Engine) Pending(f entity.FileID) []PendingView {
	fe := e.files.lockOf(f)
	if fe == nil {
		return nil
	}
	defer e.files.unlock(f, fe)
	var out []PendingView
	for _, op := range fe.ops {
		out = append(out, PendingView{op.version, op.extent.Off, op.extent.Len, op.at, op.setsMtime, op.done != nil})
	}
	return out
}

// AddOp adds a pending deallocate of [off, off+n), or with truncate a pending
// truncate to off, to f's entry at version v, as those calls will once built.
func (e *Engine) AddOp(f entity.FileID, v uint64, truncate bool, off, n int64) {
	op := pendingOp{version: journal.Version{Counter: v}, kind: opDeallocate, extent: journal.Extent{Off: off, Len: n}}
	if truncate {
		op.kind, op.extent.Len = opTruncate, 0
	}
	fe := e.files.lockFor(f)
	fe.insert(op)
	e.files.unlock(f, fe)
}

// AddWriteOp adds a pending write of [off, off+n) to f's entry at version v.
func (e *Engine) AddWriteOp(f entity.FileID, v uint64, off, n int64) {
	fe := e.files.lockFor(f)
	fe.insert(pendingOp{version: journal.Version{Counter: v}, kind: opWrite, extent: journal.Extent{Off: off, Len: n}})
	e.files.unlock(f, fe)
}

// ClearOps drops f's pending operations, as a commit of their existence does.
func (e *Engine) ClearOps(f entity.FileID) {
	fe := e.files.lockFor(f)
	fe.ops = nil
	e.files.removals.Add(1)
	e.files.unlock(f, fe)
}

// Reads is engine_reads_total of class, by its label.
func (e *Engine) Reads(class string) int64 {
	for c, name := range readClassNames {
		if name == class {
			return e.reads.byClass[c].Load()
		}
	}
	panic("no read class " + class)
}

// ReadSeconds is engine_read_seconds: its count and total.
func (e *Engine) ReadSeconds() (int64, time.Duration) {
	return e.reads.count.Load(), time.Duration(e.reads.total.Load())
}

// CloseGroupCommits stops every journal's group commit, as Close will.
func (e *Engine) CloseGroupCommits() { e.closeGroupCommits() }

// OnCapture runs f after each batch's capture, before its sync.
func (e *Engine) OnCapture(f func()) { e.hooks.captured.Store(&f) }

// CommitNow is commitNow on share s's file f.
func (e *Engine) CommitNow(ctx context.Context, s entity.ShareID, f entity.FileID) error {
	b, err := e.bindingOf(s)
	if err != nil {
		return err
	}
	return e.commitNow(ctx, b, f)
}

// WaitBatch is waitBatch on f.
func (e *Engine) WaitBatch(ctx context.Context, f entity.FileID) error { return e.waitBatch(ctx, f) }

// PendingApplied is engine_pending_existence_ops.
func (e *Engine) PendingApplied() int64 { return e.pendingApplied.Load() }

// OnLookup runs f in lockFor between its lookup and its lock.
func (e *Engine) OnLookup(f func()) { e.files.afterLookup = f }

// OnTake runs f after each batch's take, before its capture.
func (e *Engine) OnTake(f func()) { e.hooks.taken.Store(&f) }

// DropShare is dropShare on journal j.
func (e *Engine) DropShare(ctx context.Context, j Journal, s entity.ShareID) error {
	return e.dropShare(ctx, j, s)
}

// CommitAll is commitAll on journal j.
func (e *Engine) CommitAll(ctx context.Context, j Journal) error { return e.commitAll(ctx, j) }

// Due reports whether f is on journal j's due set.
func (e *Engine) Due(j Journal, f entity.FileID) bool {
	g := e.groupCommitFor(j)
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.due[f] != nil
}

// LostWhole is lostWhole on journal j.
func (e *Engine) LostWhole(j Journal) { e.lostWhole(j) }

// OnWritten runs f in each Write between its journal's WriteAt and its insert.
func (e *Engine) OnWritten(f func()) { e.hooks.written.Store(&f) }

// OnDrain runs f in each drain that read new losses, before it marks writes.
func (e *Engine) OnDrain(f func()) { e.hooks.drained.Store(&f) }

// SyncFile syncs file f of share s through the engine's one sync function.
func (e *Engine) SyncFile(ctx context.Context, s entity.ShareID, f entity.FileID) error {
	b, err := e.bindingOf(s)
	if err != nil {
		return err
	}
	return e.sync(ctx, b, fileID(f))
}

// StabilityPoints is engine_stability_points_total.
func (e *Engine) StabilityPoints() int64 { return e.stabilityPoints.Load() }

// Failing reports whether existence_commit_failing names f on journal j.
func (e *Engine) Failing(j Journal, f entity.FileID) bool {
	g := e.groupCommitFor(j)
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.failing[f] != nil && g.failing[f].raised
}

// SetConflictBound sets how long one existence commit may run.
func SetConflictBound(d time.Duration) { conflictBound.Store(d) }

// SetAfterRegister makes h run once each clone's registrations are
// installed; the clone then stops, its registrations undone, answering
// ErrUnbuilt: a test of admission alone.
func (e *Engine) SetAfterRegister(h func()) { e.afterRegister = h }

// FakeCloneDone has a clone whose phase 1 committed drop its registrations
// and answer, standing in for its hold, its phase 2 and its done signal.
func (e *Engine) FakeCloneDone() { e.fakeCloneDone.Store(true) }

// LoopSteps is the count of items the share's journal's removal loop ran.
func (e *Engine) LoopSteps(s entity.ShareID) int64 {
	b, err := e.bindingOf(s)
	if err != nil {
		return 0
	}
	return b.gc.removals.steps.Load()
}

// Registrations is the count of f's source and destination registrations.
func (e *Engine) Registrations(f entity.FileID) (src, dst int) {
	fe := e.files.lockOf(f)
	if fe == nil {
		return 0, 0
	}
	defer e.files.unlock(f, fe)
	for _, r := range fe.regs {
		if r.dst {
			dst++
		} else {
			src++
		}
	}
	return src, dst
}

// Admitted is the count of f's writes admitted and not yet in its entry.
func (e *Engine) Admitted(f entity.FileID) int {
	fe := e.files.lockOf(f)
	if fe == nil {
		return 0
	}
	defer e.files.unlock(f, fe)
	return len(fe.writing)
}

// PendingWrites is the count of f's pending writes overlapping [off, end).
func (e *Engine) PendingWrites(f entity.FileID, off, end int64) int {
	fe := e.files.lockOf(f)
	if fe == nil {
		return 0
	}
	defer e.files.unlock(f, fe)
	n := 0
	for _, op := range fe.ops {
		if op.kind == opWrite && op.extent.Off < end && off < op.extent.Off+op.extent.Len {
			n++
		}
	}
	return n
}

// OnStaged runs f in each removal between its journal step and its phase 1.
func (e *Engine) OnStaged(f func()) { e.hooks.staged.Store(&f) }

// GuardHeld reports whether a removal holds f's guard.
func (e *Engine) GuardHeld(f entity.FileID) bool {
	fe := e.files.entryOf(f)
	return fe != nil && fe.guarded.Load()
}

// RemovalStalled reports whether f has an item removal_stalled on a loop.
func (e *Engine) RemovalStalled(f entity.FileID) bool {
	e.gcMu.Lock()
	defer e.gcMu.Unlock()
	for _, g := range e.gcs {
		if g.removals.stalledOn(f) {
			return true
		}
	}
	return false
}

// RemovalsPending is engine_removals_pending over every loop.
func (e *Engine) RemovalsPending() int {
	e.gcMu.Lock()
	defer e.gcMu.Unlock()
	n := 0
	for _, g := range e.gcs {
		k, _ := g.removals.pending()
		n += k
	}
	return n
}

// NoGuardSkip has every capture take a file whose guard a removal holds,
// as one would with the guard's skip removed.
func (e *Engine) NoGuardSkip() { e.noGuardSkip.Store(true) }

// StopRemovalLoops ends every journal's removal loop, as Close's stop of
// the work queue does before its last group commit, and leaves the group
// commits running.
func (e *Engine) StopRemovalLoops() {
	e.gcMu.Lock()
	gs := make([]*groupCommit, 0, len(e.gcs))
	for _, g := range e.gcs {
		gs = append(gs, g)
	}
	e.gcMu.Unlock()
	for _, g := range gs {
		g.removals.close()
	}
}

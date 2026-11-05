package engine

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// removal is one removal of a file between its guard's taking and its
// release: its share, its file, the extent it removes and, once the journal
// has removed it, the version the journal gave it.
type removal struct {
	b    *binding
	f    entity.FileID
	x    extent
	kind entity.RemovalKind
	v    journal.Version
	at   time.Time
}

// guard takes file f's guard for a removal of x and marks x in flight,
// waiting, within ctx, for a removal of f already holding it. A truncate or
// deallocate into a clone's registration is answered at once, with nothing
// taken: one overlapping a source extent, or of a file holding a destination
// extent, is delayClone(). The guard keeps the entry alive.
//
// decision: the registration check and the in-flight mark sit in one
// section under the entry's lock. A removal that checked before a clone
// registered could otherwise drop the bytes the clone is to read, and a clone
// registering while the mark is set waits until the removal's phase 1 ends.
// Withdraw it only with a clone that reads the source through the journal's
// own snapshot.
func (e *Engine) guard(ctx context.Context, b *binding, f entity.FileID, kind entity.RemovalKind, x extent) (*removal, error) {
	fe := e.files.lockFor(f)
	for fe.guardDone != nil {
		ch := fe.guardDone
		e.files.unlock(f, fe)
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		fe = e.files.lockFor(f)
	}
	if kind != entity.RemoveRelease {
		for _, r := range fe.regs {
			if r.dst || r.overlaps(x.off, x.end) {
				e.files.unlock(f, fe)
				return nil, delayClone()
			}
		}
	}
	fe.guardDone = make(chan struct{})
	fe.guarded.Store(true)
	fe.removing = &x
	e.files.unlock(f, fe)
	return &removal{b: b, f: f, x: x, kind: kind, at: e.d.Clock.Now()}, nil
}

// guardOf is the channel closed when f's guard is released, nil when no
// removal holds it.
func (e *Engine) guardOf(f entity.FileID) <-chan struct{} {
	fe := e.files.lockOf(f)
	if fe == nil {
		return nil
	}
	defer e.files.unlock(f, fe)
	if fe.guardDone == nil {
		return nil
	}
	return fe.guardDone
}

// release ends r's hold on its file: the in-flight mark is cleared, the guard
// released and its waiters woken. Then, once its phase 1 committed, the
// journal is settled through it, and the file, if it still holds operations,
// is noted for the group commit, in that order. One whose phase 1 did not
// commit settles only when unstage asks it to, for a terminal failure.
func (e *Engine) release(r *removal, settle bool) {
	fe := e.files.lockFor(r.f)
	fe.removing = nil
	fe.guarded.Store(false)
	close(fe.guardDone)
	fe.guardDone = nil
	var oldest time.Time
	if len(fe.ops) > 0 {
		oldest = fe.ops[0].at
	}
	e.files.unlock(r.f, fe)
	if settle {
		r.b.h.Settle(fileID(r.f), r.v)
	}
	if !oldest.IsZero() {
		_ = r.b.gc.note(r.b, r.f, oldest) // refused only once closed or lost, when nothing commits
	}
}

// Truncate sets file f's size. At or above the size its operations leave, it
// is a grow: the journal's Truncate gives it its version and the group commit
// commits it, with no phase 1. Below, it is a removal: the guard is held
// across the journal's Truncate and phase 1, which commits the file's pending
// existence below the truncate with it, and it answers once phase 1 commits.
//
// decision: a grow needs no phase 1, since it masks nothing: it is one more
// pending operation, the store's OpGrow, committed like a write. Revisit if
// a grow ever has to drop records.
func (e *Engine) Truncate(ctx context.Context, s entity.ShareID, f entity.FileID, size int64) error {
	if size < 0 {
		return vfserr.ErrInvalid
	}
	b, err := e.bindingOf(s)
	if err != nil {
		return err
	}
	r, err := e.guard(ctx, b, f, entity.RemoveTruncate, extent{size, entity.EndOfFile})
	if err != nil {
		return err
	}
	if err := e.waitBatch(ctx, f); err != nil {
		e.release(r, false)
		return err
	}
	committed, err := e.d.Existence.Size(ctx, s, f)
	if err != nil {
		e.release(r, false)
		return e.storeErr(err)
	}
	grow, err := e.stage(ctx, r, func(fe *fileEntry) (journal.Version, error) {
		return b.h.Truncate(ctx, fileID(f), size)
	}, opTruncate, journal.Extent{Off: size}, func(fe *fileEntry) bool {
		return size >= replaySize(committed, fe.ops)
	})
	if err != nil || grow {
		e.release(r, false)
		return err
	}
	e.staged()
	return e.phase1(ctx, r, kvstore.RemoveOp{Kind: entity.RemoveTruncate, Extent: entity.ExtentOf(size, entity.EndOfFile)})
}

// Deallocate makes [off, off+n) of file f a hole: the journal stops holding
// it and phase 1 records the extent's merged hole at once, staging no zeros.
// A zero length removes nothing.
func (e *Engine) Deallocate(ctx context.Context, s entity.ShareID, f entity.FileID, off, n int64) error {
	switch {
	case off < 0 || n < 0 || n > entity.EndOfFile-off:
		return vfserr.ErrInvalid
	case n == 0:
		return nil
	}
	b, err := e.bindingOf(s)
	if err != nil {
		return err
	}
	r, err := e.guard(ctx, b, f, entity.RemoveDeallocate, extent{off, off + n})
	if err != nil {
		return err
	}
	if err := e.waitBatch(ctx, f); err != nil {
		e.release(r, false)
		return err
	}
	x := journal.Extent{Off: off, Len: n}
	if _, err := e.stage(ctx, r, func(*fileEntry) (journal.Version, error) {
		return b.h.Deallocate(ctx, fileID(f), x)
	}, opDeallocate, x, nil); err != nil {
		e.release(r, false)
		return err
	}
	e.staged()
	return e.phase1(ctx, r, kvstore.RemoveOp{Kind: entity.RemoveDeallocate, Extent: entity.Extent{Offset: off, Length: n}})
}

// Delete is file f's release, never the journal's eviction: the guard, the
// journal's Delete, then phase 1, which re-checks the file's holders. It
// reads and writes no open state: the filesystem service marked the file
// releasing before calling it. A phase 1 that commits within the call
// answers a nil channel and nil. One a holder aborts answers
// vfserr.ErrFileOpen. One the call cannot commit, the journal's Delete done,
// is pending: it answers a channel and nil, the loop retries it keeping the
// guard, and the channel yields its outcome once: nil once it commits,
// vfserr.ErrFileOpen if a holder aborts it, vfserr.ErrUnavailable if the loop
// ends first.
//
// decision: a release holds no lock across the journal's Delete and stages
// no pending operation, unlike a truncate: the filesystem service marks the
// file releasing through open state before it calls Delete, and no write,
// read or open reaches a file so marked. Give it a staged operation if a
// call can ever reach the engine past that mark.
//
// decision: a release is pending, not failed, from the moment the journal's
// Delete returns, as the journal no longer holds the file's bytes and only
// its marker brings them back; before that, the context's end is a refusal
// that changed nothing. Withdraw it only with a journal Delete that can be
// undone.
func (e *Engine) Delete(ctx context.Context, s entity.ShareID, f entity.FileID) (retry <-chan error, err error) {
	b, err := e.bindingOf(s)
	if err != nil {
		return nil, err
	}
	r, err := e.guard(ctx, b, f, entity.RemoveRelease, extent{0, entity.EndOfFile})
	if err != nil {
		return nil, err
	}
	if err := e.waitBatch(ctx, f); err != nil {
		e.release(r, false)
		return nil, err
	}
	v, err := b.h.Delete(ctx, fileID(f))
	if err != nil {
		e.release(r, false)
		return nil, e.journalErr(err)
	}
	r.v = v
	e.staged()
	op := kvstore.RemoveOp{Kind: entity.RemoveRelease, Extent: entity.ExtentOf(0, entity.EndOfFile),
		Version: entity.JournalVersion(v.Bytes()), Time: r.at}
	switch err := e.tryPhase1(ctx, r, op); {
	case err == nil:
		return nil, nil
	case errors.Is(err, kvstore.ErrHeld):
		return nil, e.abort(r)
	case terminal(err):
		e.unstage(r, err)
		return nil, e.storeErr(err)
	}
	ch := make(chan error, 1) // buffered: the loop never blocks on a caller gone
	e.retryLater(r, op, func(err error) { ch <- err })
	return ch, nil
}

// abort ends release r that a holder refused: the file's pending writes
// below it are dropped, their Dones run, the journal is settled without the removal, the
// loss is raised on the file's sequence and logged naming it, and the guard
// is released. It answers vfserr.ErrFileOpen, which the service tells from
// ErrUnavailable: it clears the file's releasing mark for one and keeps it
// for the other.
//
// decision: the store's ErrHeld crosses as vfserr.ErrFileOpen, which only
// the engine and the filesystem service exchange, so the service's release
// clears its mark for an abort and keeps it for an outage. Withdraw it if a
// release's outcomes ever reach an adapter.
func (e *Engine) abort(r *removal) error {
	fe := e.files.lockFor(r.f)
	var dones []func()
	kept := fe.ops[:0]
	for _, op := range fe.ops {
		switch {
		case op.version.Compare(r.v) >= 0:
			kept = append(kept, op)
		case !op.dropped && op.done != nil:
			dones = append(dones, op.done)
		}
	}
	clear(fe.ops[len(kept):])
	fe.ops = kept
	e.files.unlock(r.f, fe)
	l := &r.b.gc.losses
	l.mu.Lock()
	l.raise(r.f)
	l.mu.Unlock()
	e.d.Logger.Error("a release was aborted by a holder: the file's content is lost", "file", r.f)
	e.release(r, true)
	for _, d := range dones {
		d()
	}
	return vfserr.ErrFileOpen
}

// stage runs the journal's removal j under f's entry lock and inserts its
// pending operation before the lock is released, answering whether grow,
// asked under the same lock, made it a grow rather than a removal.
//
// decision: the entry's lock is held across the journal's Truncate or
// Deallocate and the insert of its operation, unlike a write's, as the
// journal stops holding the extent at once and a read between the two would
// find it missing with no pending removal and read it lost. Reads of the
// file wait out one journal removal. Withdraw it if the journal hands back a
// removal's version before it stops holding the extent.
func (e *Engine) stage(ctx context.Context, r *removal, j func(*fileEntry) (journal.Version, error), kind opKind, x journal.Extent, grow func(*fileEntry) bool) (bool, error) {
	fe := e.files.lockFor(r.f)
	defer e.files.unlock(r.f, fe)
	// A write the journal staged before the removal is in the entry before
	// the removal decides or is staged: a write admitted and not yet inserted
	// is waited out, as a clone's registration waits it out, while no new
	// write is admitted, since the entry's lock is held between waits.
	if len(fe.writing) > 0 {
		stop := context.AfterFunc(ctx, func() {
			fe.mu.Lock()
			fe.staged.Broadcast()
			fe.mu.Unlock()
		})
		for len(fe.writing) > 0 && ctx.Err() == nil {
			fe.staged.Wait()
		}
		stop()
		if err := ctx.Err(); err != nil {
			return false, err
		}
	}
	g := grow != nil && grow(fe)
	v, err := j(fe)
	if err != nil {
		return false, e.journalErr(err)
	}
	r.v = v
	fe.insert(pendingOp{version: v, kind: kind, extent: x, at: r.at, grow: g})
	return g, nil
}

// replaySize is the size the file's operations leave over committed: a
// truncate sets it, a write raises it to its end.
func replaySize(committed int64, ops []pendingOp) int64 {
	size := committed
	for _, op := range ops {
		switch {
		case op.dropped:
		case op.kind == opTruncate:
			size = op.extent.Off
		case op.kind == opWrite:
			size = max(size, opEnd(op.extent))
		}
	}
	return size
}

// phase1 is the removal r's first metadata transaction, op at r's version,
// run once the caller waited out a batch in flight on the file: it claims the file's pending
// operations below the removal, syncs the file, and hands them to the
// store's Remove with the removal. Once that commits, the claimed operations
// and the removal's own leave the entry and their Dones run, and the guard
// is released; a failure releases the claim and answers the store's error,
// a conflict at ctx's end DelayConflict.
//
// decision: phase 1 claims only the operations below the removal's version.
// One staged between the journal's removal and phase 1 is above it and stays
// pending: the store's Remove skips a removal at or below the version the
// file has applied, so committing an operation above it first would lose the
// removal. Withdraw it if the store ever applies a removal below applied.
func (e *Engine) phase1(ctx context.Context, r *removal, op kvstore.RemoveOp) error {
	op.Version, op.Time = entity.JournalVersion(r.v.Bytes()), r.at
	err := e.tryPhase1(ctx, r, op)
	switch {
	case err == nil:
		return nil
	case terminal(err):
		e.unstage(r, err)
		return e.storeErr(err)
	}
	// decision: once the journal has removed the extent, a phase 1 the call
	// cannot commit is not failed: the journal's loop retries it, keeping the
	// guard and the in-flight mark, and the call answers retry-later. A file
	// found gone, or a removal the store refuses as invalid, ends it, its
	// marker settled without the removal (RFC 8 §8.1).
	e.retryLater(r, op, nil)
	if errors.Is(err, kvstore.ErrConflict) {
		return &vfserr.Delay{Cause: vfserr.DelayConflict}
	}
	return &vfserr.Delay{Cause: vfserr.DelayCommit}
}

// tryPhase1 runs r's phase 1 once: the claim, the sync, the store's Remove,
// then, once it commits, the claimed operations and the removal's own out
// of the entry, r finished and the Dones run. A failure releases the claim
// and keeps the guard.
func (e *Engine) tryPhase1(ctx context.Context, r *removal, op kvstore.RemoveOp) error {
	e.claimBelow(r)
	if err := e.sync(ctx, r.b, fileID(r.f)); err != nil {
		e.unclaim(r)
		return err
	}
	if err := e.d.Existence.Remove(ctx, r.b.share.ID, r.f, 0, op, e.claimed(r)); err != nil {
		e.unclaim(r)
		return err
	}
	dones := e.committed(r)
	e.finish(r)
	for _, d := range dones {
		d()
	}
	return nil
}

// terminal reports a phase 1 failure that retrying cannot change: the file
// gone, or the removal refused as invalid.
func terminal(err error) bool {
	return errors.Is(err, entity.ErrStale) || kvstore.Refused(err)
}

// unstage ends removal r whose phase 1 will not commit: its own pending
// operation leaves the entry, so no Commit waits on it and the file's other
// operations commit, and the guard is released. A phase 1 that ended on a
// terminal failure, err, settles the journal's marker without the removal
// (RFC 8 §8.1), so no later start re-applies it, and logs the loss when the
// file still exists. One its loop's end cut short, err nil, leaves the marker
// for the next start's recovery.
func (e *Engine) unstage(r *removal, err error) {
	fe := e.files.lockFor(r.f)
	fe.ops = slices.DeleteFunc(fe.ops, func(op pendingOp) bool { return op.version == r.v })
	e.files.unlock(r.f, fe)
	if err != nil && !errors.Is(err, entity.ErrStale) {
		e.d.Logger.Error("removal ended unapplied, its journal marker settled", "file", r.f, "kind", r.kind.String(), "err", err)
	}
	e.release(r, err != nil)
}

// claimBelow claims r's file's live operations below r's version, writes and
// grows, for phase 1. The guard keeps every batch off the file, so none is
// claimed already.
func (e *Engine) claimBelow(r *removal) {
	fe := e.files.lockFor(r.f)
	defer e.files.unlock(r.f, fe)
	for i := range fe.ops {
		op := &fe.ops[i]
		if op.version.Compare(r.v) >= 0 {
			break
		}
		if !op.dropped {
			op.claimed = true
		}
	}
}

// claimed is r's claimed operations as the store commits them, read after
// the sync, so a loss its drain marked commits lost.
func (e *Engine) claimed(r *removal) []kvstore.PendingOp {
	fe := e.files.lockFor(r.f)
	defer e.files.unlock(r.f, fe)
	var out []kvstore.PendingOp
	for _, op := range fe.ops {
		if !op.claimed || op.dropped {
			continue
		}
		var mtime time.Time
		if op.setsMtime {
			mtime = op.at
		}
		out = append(out, storeOp(op, mtime))
	}
	return out
}

// unclaim releases r's claim after a failed phase 1: the operations stay
// pending.
func (e *Engine) unclaim(r *removal) {
	fe := e.files.lockFor(r.f)
	defer e.files.unlock(r.f, fe)
	for i := range fe.ops {
		fe.ops[i].claimed = false
	}
}

// committed removes from r's file's entry, after its phase 1 committed, the
// operations it claimed and its own, before the guard is released, so no
// batch captures either: it answers the claimed writes' Dones. A claimed
// write a failed window dropped leaves too, its Done having run then.
func (e *Engine) committed(r *removal) (dones []func()) {
	fe := e.files.lockFor(r.f)
	defer e.files.unlock(r.f, fe)
	kept := fe.ops[:0]
	for _, op := range fe.ops {
		switch {
		case op.claimed:
			if !op.dropped {
				if op.done != nil {
					dones = append(dones, op.done)
				}
				e.pendingApplied.Add(1)
			}
		case op.version == r.v:
		default:
			kept = append(kept, op)
		}
	}
	clear(fe.ops[len(kept):])
	fe.ops = kept
	e.files.removals.Add(1) // committed operations left the entry: see Allocation
	return dones
}

// staged runs a test's seam between a removal's journal step and its phase 1.
func (e *Engine) staged() {
	if h := e.hooks.staged.Load(); h != nil {
		(*h)()
	}
}

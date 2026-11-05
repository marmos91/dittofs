package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// extent is a run of bytes [off, end) of one file.
type extent struct{ off, end int64 }

func (x extent) overlaps(o, e int64) bool { return x.off < e && o < x.end }

// reg is one running clone's registration on a file: the extent it holds,
// and whether the file is the clone's destination or its source.
type reg struct {
	extent
	dst bool
}

// delayClone is the answer to a call behind a running clone.
//
// decision: a call behind a clone answers at once and the engine never waits
// inside it, since the filesystem service holds the file's admission and
// accept locks across the call and holds it behind the clone itself (RFC 17
// §5.9), and a call held behind a clone holds no entry mutex (RFC 14 §11).
// Withdraw it if RFC 17 moves that hold into the engine.
func delayClone() error { return &vfserr.Delay{Cause: vfserr.DelayClone} }

// refuseWrite answers delayClone() if [o, e) of the file overlaps an extent a
// clone registered on it, source or destination; nil otherwise. The caller
// holds the entry's lock.
func (fe *fileEntry) refuseWrite(o, e int64) error {
	for _, r := range fe.regs {
		if r.overlaps(o, e) {
			return delayClone()
		}
	}
	return nil
}

// refuseRead answers delayClone() if [o, e) of the file overlaps an extent a
// clone registered on it as its destination, whose content the clone is
// changing; a source extent reads as it is. The caller holds the entry's lock.
func (fe *fileEntry) refuseRead(o, e int64) error {
	for _, r := range fe.regs {
		if r.dst && r.overlaps(o, e) {
			return delayClone()
		}
	}
	return nil
}

// leave removes x, one admitted write, from those writing, and wakes a
// registration waiting for it. The caller holds the entry's lock.
func (fe *fileEntry) leave(x extent) {
	for i, w := range fe.writing {
		if w == x {
			fe.writing = append(fe.writing[:i], fe.writing[i+1:]...)
			break
		}
	}
	fe.staged.Broadcast()
}

// writingInto reports whether an admitted write not yet inserted overlaps x.
// The caller holds the entry's lock.
func (fe *fileEntry) writingInto(x extent) bool {
	for _, w := range fe.writing {
		if w.overlaps(x.off, x.end) {
			return true
		}
	}
	return false
}

// register installs r on f's entry and waits until every write admitted
// before it into r's extent has its operation in the entry, so the clone
// reads the file as those writes leave it. A destination extent is refused
// delayClone() where it overlaps any registration on f, as a write there
// would be, and on a file another clone already writes into, which holds
// one clone into it at a time; a source extent only where it overlaps a
// destination extent, so
// several clones may share a source, none of them changing it. The
// registration keeps the entry alive. It answers ctx's error, registering
// nothing, if ctx ends first.
func (e *Engine) register(ctx context.Context, f entity.FileID, r *reg) error {
	fe := e.files.lockFor(f)
	// A removal in flight over the extent is waited out: it checked the
	// file's registrations before this one, so its phase 1 must end before
	// the clone reads the file.
	for fe.removing != nil && fe.removing.overlaps(r.off, r.end) {
		ch := fe.guardDone
		e.files.unlock(f, fe)
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
		fe = e.files.lockFor(f)
	}
	var err error
	if r.dst {
		err = fe.refuseWrite(r.off, r.end)
		for _, o := range fe.regs {
			if o.dst {
				err = delayClone() // a file holds one clone into it at a time
			}
		}
	} else {
		err = fe.refuseRead(r.off, r.end)
	}
	if err != nil {
		e.files.unlock(f, fe)
		return err
	}
	fe.regs = append(fe.regs, r)
	if fe.writingInto(r.extent) {
		stop := context.AfterFunc(ctx, func() {
			fe.mu.Lock()
			fe.staged.Broadcast()
			fe.mu.Unlock()
		})
		for fe.writingInto(r.extent) && ctx.Err() == nil {
			fe.staged.Wait()
		}
		stop()
		if err := ctx.Err(); err != nil {
			fe.drop(r)
			e.files.unlock(f, fe)
			return err
		}
	}
	e.files.unlock(f, fe)
	return nil
}

// unregister removes r from f's entry, which it kept alive.
func (e *Engine) unregister(f entity.FileID, r *reg) {
	fe := e.files.lockOf(f)
	if fe == nil {
		return
	}
	fe.drop(r)
	e.files.unlock(f, fe)
}

// drop removes r from the entry's registrations. The caller holds its lock.
func (fe *fileEntry) drop(r *reg) {
	for i, x := range fe.regs {
		if x == r {
			fe.regs = append(fe.regs[:i], fe.regs[i+1:]...)
			return
		}
	}
}

// Clone makes x of dst the content of x of src, atomically, answered when
// done. Each file is named with its own share: on one node a clone between
// two shares of one namespace reaches the engine.
//
// Admission refuses, before any journal or store call, a share with no
// binding on either side (DelayFrozen), a zero or negative length or offset,
// overlapping extents of one file and a length over clone_max_len
// (ErrInvalid), two write namespaces (ErrCross with CrossNamespace), and an
// extent a running clone holds (DelayClone). It then registers the source
// extent and the destination extent. The clone's hold on its source, its
// phase 2 and its done signal are not built, so it then undoes both and
// answers errUnbuilt; with a test's stand-in for the done signal it runs on:
// step 1 on the call (prepare), whose refusal undoes both registrations and
// changes nothing, then from the journal step on its own goroutine
// (runClone), where it never fails; the call waits for it within its own
// context, a deadline answering DelayClone and leaving the clone running.
//
// decision: a clone between two shares on one node is admitted, as both
// shards are this node's and no failover or handover can split them while
// the clone runs; RFC 8 §9.1 refuses a clone across shards only in a cluster.
// Withdraw it when a node can lose one of the two shards mid-clone.
//
// decision: an incomplete source is refused by the filesystem service (RFC
// 17 §5.9 step 2), which reads the File record: the engine reads none, and
// the store keeps no incomplete flag for it to check.
func (e *Engine) Clone(ctx context.Context, srcShare entity.ShareID, src entity.FileID, dstShare entity.ShareID, dst entity.FileID, x vfserr.Extent) error {
	sb, err := e.bindingOf(srcShare)
	if err != nil {
		return err
	}
	db, err := e.bindingOf(dstShare)
	if err != nil {
		return err
	}
	st := e.settings.Load()
	switch {
	case st == nil:
		return fmt.Errorf("engine: no settings applied: %w", vfserr.ErrUnavailable)
	case x.Len <= 0 || x.SrcOff < 0 || x.DstOff < 0 || x.Len > entity.EndOfFile-max(x.SrcOff, x.DstOff):
		return vfserr.ErrInvalid
	case src == dst && extent{x.SrcOff, x.SrcOff + x.Len}.overlaps(x.DstOff, x.DstOff+x.Len):
		return vfserr.ErrInvalid
	case x.Len > st.CloneMaxLen:
		return vfserr.ErrInvalid
	}
	// decision: the namespace check compares the two shares' write
	// namespaces, the one each Share names. A share listing two during a
	// re-home (RFC 27) needs the whole list, which the store's phase 1 also
	// compares, as a backstop; widen it here once re-homes are built.
	//
	// decision: the check holds only once Bind fills Share.Namespace. Until
	// then every bound share carries the zero namespace, so two shares of
	// different namespaces compare equal here and are refused only by the
	// store's phase 1, after the journal step. Bind's test must then clone
	// between two shares Bind gave different namespaces and see ErrCross
	// with no journal call.
	if sb.share.Namespace != db.share.Namespace {
		return &vfserr.CrossError{Kind: vfserr.CrossNamespace}
	}
	rs := &reg{extent: extent{x.SrcOff, x.SrcOff + x.Len}}
	rd := &reg{extent: extent{x.DstOff, x.DstOff + x.Len}, dst: true}
	if err := e.register(ctx, src, rs); err != nil {
		return err
	}
	if err := e.register(ctx, dst, rd); err != nil {
		e.unregister(src, rs)
		return err
	}
	if h := e.afterRegister; h != nil || !e.fakeCloneDone.Load() {
		// A clone stops once registered while its hold on the source, its
		// phase 2 and its done signal are not built, as nothing could answer
		// one past its journal step: a test of admission alone runs its hook
		// first, and a test's stand-in for the done signal lets it run on.
		if h != nil {
			h()
		}
		e.unregister(dst, rd)
		e.unregister(src, rs)
		return errUnbuilt
	}
	c := &cloneRun{sb: sb, db: db, src: src, dst: dst, rs: rs, rd: rd, done: make(chan struct{})}
	if err := e.prepare(ctx, c, x); err != nil {
		e.unregister(dst, rd)
		e.unregister(src, rs)
		return err
	}
	go e.runClone(c)
	select {
	case <-c.done:
		return c.err
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return delayClone()
		}
		return ctx.Err()
	}
}

// cloneRun is one clone past its admission: its two files and bindings,
// their registrations, its spec, the source's held runs it copies, one copy
// write each, the reservation taken for them, and once its copies are
// synced, the newest copy's version.
type cloneRun struct {
	sb, db   *binding
	src, dst entity.FileID
	rs, rd   *reg
	spec     journal.CloneSpec
	runs     []journal.Held
	res      Reservation // nil when none is held
	copied   bool        // the copies are written and synced
	newest   journal.Version
	done     chan struct{}
	err      error
}

// copyBuf is the clone's copy buffer: the source is resolved and copied in
// windows of it.
const copyBuf = 1 << 20

// prepare is the clone's step 1, on the call: the source's pending existence
// committed, which syncs it; a source extent past the source's committed end
// refused ErrInvalid; then the sizing pass and the reservation (size).
// Nothing it does changes the destination.
func (e *Engine) prepare(ctx context.Context, c *cloneRun, x vfserr.Extent) error {
	if err := e.commitNow(ctx, c.sb, c.src); err != nil {
		return e.waitErr(c.sb, c.src, err)
	}
	size, err := e.d.Existence.Size(ctx, c.sb.share.ID, c.src)
	if err != nil {
		return e.storeErr(err)
	}
	if x.SrcOff+x.Len > size {
		return vfserr.ErrInvalid
	}
	c.spec = journal.CloneSpec{SrcShare: journal.ShareID(c.sb.share.ID), Src: fileID(c.src),
		SrcOff: x.SrcOff, DstOff: x.DstOff, Len: x.Len}
	asOf, err := e.size(ctx, c)
	if err != nil {
		return e.journalErr(err)
	}
	c.spec.AsOf = asOf
	return nil
}

// size resolves the source extent in windows of copyBuf, keeping its held
// runs, and reserves on the destination's handle every byte and write the
// copies need, so no copy is refused part-way. It answers the newest
// version the source was read at.
//
// ponytail: a clone reads its source's held bytes twice, here to size them
// and in its copies. Give the handle RFC 1 §3.7's Extents to size without
// reading when clone time on journal-held sources shows in a profile.
func (e *Engine) size(ctx context.Context, c *cloneRun) (journal.Version, error) {
	end := c.spec.SrcOff + c.spec.Len
	buf := make([]byte, min(copyBuf, c.spec.Len))
	var asOf journal.Version
	var n int64
	c.runs = c.runs[:0]
	for off := c.spec.SrcOff; off < end; off += int64(len(buf)) {
		w := buf[:min(int64(len(buf)), end-off)]
		_, _, held, at, err := c.sb.h.ReadAt(ctx, fileID(c.src), off, w)
		if err != nil {
			return journal.Version{}, err
		}
		for _, h := range held {
			n += h.Extent.Len
		}
		c.runs = append(c.runs, held...)
		if asOf.Compare(at) < 0 {
			asOf = at
		}
	}
	if len(c.runs) > 0 {
		res, err := c.db.h.Reserve(n, len(c.runs))
		if err != nil {
			return journal.Version{}, err
		}
		c.res = res
	}
	return asOf, nil
}

// runClone is the clone from its journal step on, on its own goroutine under
// the destination journal's loop context: the destination's guard, a batch in
// flight on it waited out, CloneTarget with the clone's operation in the
// entry, then what attempt runs. A failure before CloneTarget changes nothing
// and fails the clone; from CloneTarget on it never fails: what attempt
// cannot finish, the loop retries, the guard held.
//
// ponytail: the goroutine is not tracked, so nothing joins it at Unbind or
// Close but the loop context's end, and it may call the destination's handle
// or the store after Close; the engine's map of running clones, which
// retries, Unbind and Close join, replaces it.
func (e *Engine) runClone(c *cloneRun) {
	ctx := c.db.gc.removals.ctx
	r, err := e.guard(ctx, c.db, c.dst, entity.RemoveClone, c.rd.extent)
	if err != nil {
		e.cloneFailed(c, err)
		return
	}
	if err := e.waitBatch(ctx, c.dst); err != nil {
		e.release(r, false)
		e.cloneFailed(c, err)
		return
	}
	dx := journal.Extent{Off: c.spec.DstOff, Len: c.spec.Len}
	if _, err := e.stage(ctx, r, func(*fileEntry) (journal.Version, error) {
		return c.db.h.CloneTarget(ctx, fileID(c.dst), c.spec)
	}, opClone, dx, nil); err != nil {
		e.release(r, false)
		e.cloneFailed(c, err)
		return
	}
	if e.attempt(ctx, c, r) == itemRetry {
		e.retryClone(c, r)
	}
}

// attempt runs what the clone has left past its journal step: its copies
// and the destination's sync, unless done, then its phase 1, bounded by the
// conflict bound and counted as a removal batch. It answers itemDone once
// the clone finished or ended, itemRetry for the loop to run it again.
//
// decision: once the journal step dropped the destination extent no undo
// exists, so a copy, a sync or a phase 1 that fails is retried by the loop
// at its backoff, the guard held and both files registered, and five
// failures in a row raise removal_stalled. A copy refused because a loss
// split a run past what was reserved is retried after sizing and reserving
// again. A failure that never clears pins the destination's guard and both
// registrations until Close, which leaves the clone to the next start's
// rebuild from the journal's marker. Withdraw it if a clone can ever be
// undone after its journal step.
func (e *Engine) attempt(ctx context.Context, c *cloneRun, r *removal) answer {
	if !c.copied {
		if err := e.copyAll(ctx, c); err != nil {
			if ctx.Err() == nil {
				e.d.Logger.Error("a clone's copies failed after its journal step: retried", "src", c.src, "dst", c.dst, "err", err)
			}
			return itemRetry
		}
		c.copied = true
		e.staged()
	}
	pctx, cancel := context.WithTimeout(ctx, conflictBound.Load().(time.Duration))
	defer cancel()
	err := e.tryClonePhase1(pctx, c, r)
	e.count(err)
	switch {
	case err == nil:
		e.cloned(c)
		return itemDone
	case errors.Is(err, entity.ErrStale):
		e.cloneEnded(c, r, err)
		return itemDone
	case kvstore.Refused(err):
		e.d.Logger.Error("a clone's phase 1 was refused after its journal step: retried", "src", c.src, "dst", c.dst, "err", err)
	}
	return itemRetry
}

// copyAll writes the clone's copies through a reservation, sized again
// unless step 1's is still held, syncs the destination and closes the
// reservation.
func (e *Engine) copyAll(ctx context.Context, c *cloneRun) error {
	if c.res == nil {
		if _, err := e.size(ctx, c); err != nil {
			return err
		}
	}
	newest, err := e.copyRuns(ctx, c)
	if err == nil {
		err = e.sync(ctx, c.db, fileID(c.dst))
	}
	if c.res != nil {
		if cerr := c.res.Close(); err == nil && cerr != nil {
			err = cerr
		}
		c.res = nil
	}
	if err != nil {
		return err
	}
	if c.newest.Compare(newest) < 0 {
		c.newest = newest
	}
	return nil
}

// copyRuns copies every source run size found held into the destination,
// through the reservation, each a pending write of the destination's above
// the clone's version, with no Done, as no client write is charged; it
// answers the newest copy's version. A run the journal no longer holds is
// copied as far as it still holds it: the rest is the source's uncarved
// content, which the destination reads lost, never zeros.
//
// decision: a clone copies the source's unoffloaded bytes through the
// journal rather than offloading them first, so a clone never waits on the
// remote tier; the price is journal capacity, a second upload of those
// bytes, and source writes held for one clone's length, capped by
// clone_max_len for CLONE, by one chunk for a copy. Offload first instead,
// as one path, if clones of large dirty extents are shown to push the
// journal into pacing.
func (e *Engine) copyRuns(ctx context.Context, c *cloneRun) (journal.Version, error) {
	var newest journal.Version
	if len(c.runs) == 0 {
		return newest, nil
	}
	now := e.d.Clock.Now()
	buf := make([]byte, copyBuf)
	for _, run := range c.runs {
		p := buf[:run.Extent.Len]
		_, _, held, _, err := c.sb.h.ReadAt(ctx, fileID(c.src), run.Extent.Off, p)
		if err != nil {
			return newest, err
		}
		for _, h := range held {
			at := h.Extent.Off - run.Extent.Off
			off := c.spec.DstOff + h.Extent.Off - c.spec.SrcOff
			v, err := c.res.WriteAt(ctx, fileID(c.dst), off, p[at:at+h.Extent.Len], now.UnixNano())
			if err != nil {
				return newest, err
			}
			fe := e.files.lockFor(c.dst)
			fe.insert(pendingOp{version: v, kind: opWrite, extent: journal.Extent{Off: off, Len: h.Extent.Len},
				at: now, setsMtime: true, copy: true})
			e.files.unlock(c.dst, fe)
			if newest.Compare(v) < 0 {
				newest = v
			}
		}
	}
	return newest, nil
}

// tryClonePhase1 runs the clone's phase 1 once: the destination's pending
// operations claimed, the copies and the clone's own aside, the destination
// synced, and the store's Clone handed them; once it commits, they leave the
// entry with the clone's operation and its copies, the guard is released and
// the journal settled through the clone's version, and the claimed writes'
// Dones run. A failure releases the claim and keeps the guard.
func (e *Engine) tryClonePhase1(ctx context.Context, c *cloneRun, r *removal) error {
	if err := e.claimAll(ctx, r); err != nil {
		return err
	}
	if err := e.sync(ctx, c.db, fileID(c.dst)); err != nil {
		e.unclaim(r)
		return err
	}
	op := kvstore.CloneOp{Share: c.db.share.ID, File: c.dst, Version: entity.JournalVersion(r.v.Bytes()),
		Copied: entity.JournalVersion(c.newest.Bytes()), Time: r.at,
		Spec: entity.CloneSpec{SrcShare: c.sb.share.ID, Src: c.src, SrcOff: c.spec.SrcOff, DstOff: c.spec.DstOff,
			Len: c.spec.Len, AsOf: entity.JournalVersion(c.spec.AsOf.Bytes())}}
	if err := e.d.Existence.Clone(ctx, op, e.claimed(r)); err != nil {
		e.unclaim(r)
		return err
	}
	dones := e.committed(r)
	e.release(r, true)
	for _, d := range dones {
		d()
	}
	return nil
}

// claimAll claims r's file's live operations but r's own and a clone's
// copies, for a clone's phase 1, once no write admitted to the file is
// still in the journal: the guard keeps every batch off the file, and a
// write staged between the clone's version and its copies' newest, still
// unclaimed when the store's Clone sets applied past it, would be skipped as
// applied by every later commit. No new write is admitted while the entry's
// lock is held between waits.
func (e *Engine) claimAll(ctx context.Context, r *removal) error {
	fe := e.files.lockFor(r.f)
	defer e.files.unlock(r.f, fe)
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
			return err
		}
	}
	for i := range fe.ops {
		op := &fe.ops[i]
		if !op.dropped && !op.copy && op.version != r.v {
			op.claimed = true
		}
	}
	return nil
}

// retryClone hands the rest of the clone, which its goroutine could not
// finish, to the destination journal's loop, which runs attempt again at its
// backoff, the guard held, so no group commit raises applied above the
// clone. A destination found gone ends it; the loop's end leaves it to the
// next start.
//
// decision: the store's phase 1 refuses an overlap, a source past its end,
// two namespaces and a destination already cloning, after the journal step
// RFC 8 §9.1 says no refusal may follow. Admission refuses each first, so
// one is met here only when the store changed under a registered extent by
// a path this part does not have; met anyway, it is logged at Error,
// retried and never fails or undoes. A refusal that never clears pins the
// destination's guard, which a group commit's capture skips. Withdraw it if
// the store's checks move to a read at admission.
func (e *Engine) retryClone(c *cloneRun, r *removal) {
	c.db.gc.removals.enqueue(&loopItem{b: c.db, f: c.dst, kind: entity.RemoveClone,
		run: func(ctx context.Context) answer { return e.attempt(ctx, c, r) },
		end: func() { e.cloneEnded(c, r, nil) },
	})
}

// cloned is the clone after its phase 1. Its hold on the source, its phase 2
// and its done signal are not built: a test's stand-in drops the
// registrations and raises the done signal.
func (e *Engine) cloned(c *cloneRun) {
	e.unregister(c.dst, c.rd)
	e.unregister(c.src, c.rs)
	close(c.done)
}

// cloneEnded ends a clone whose phase 1 will not commit: the destination
// gone, err its store error, or its loop ended first, err nil, which leaves
// the journal's marker for the next start (unstage). Its copies leave the
// entry with its own operation: committed as writes, they would raise the
// destination's applied above the clone, and the next start's rebuild of it
// would then be skipped as applied. Both registrations go, and the call
// answers why.
func (e *Engine) cloneEnded(c *cloneRun, r *removal, err error) {
	if c.res != nil {
		_ = c.res.Close()
		c.res = nil
	}
	fe := e.files.lockFor(c.dst)
	fe.ops = slices.DeleteFunc(fe.ops, func(op pendingOp) bool { return op.copy })
	e.files.unlock(c.dst, fe)
	e.unstage(r, err)
	e.unregister(c.dst, c.rd)
	e.unregister(c.src, c.rs)
	if err != nil {
		c.err = e.storeErr(err)
	} else {
		c.err = errLoopEnded
	}
	close(c.done)
}

// cloneFailed fails a clone before its journal step: nothing changed, the
// reservation is given back and both registrations go.
func (e *Engine) cloneFailed(c *cloneRun, err error) {
	if c.res != nil {
		_ = c.res.Close()
	}
	e.unregister(c.dst, c.rd)
	e.unregister(c.src, c.rs)
	if errors.Is(err, context.Canceled) {
		err = errLoopEnded
	}
	c.err = err
	close(c.done)
}

package engine

import (
	"context"
	"fmt"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
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
// would be; a source extent only where it overlaps a destination extent, so
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
// extent and the destination extent. The clone's steps from the source's sync
// on are not built: the registrations are undone and the call answers
// errUnbuilt.
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
	if h := e.afterRegister; h != nil {
		h()
	}
	e.unregister(dst, rd)
	e.unregister(src, rs)
	return errUnbuilt
}

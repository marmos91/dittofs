package openstate

import (
	"context"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/openstate/conflict"
	"github.com/marmos91/dittofs/pkg/openstate/ranges"
)

// ByteRange is Length bytes from Offset, ending at Offset+Length-1, a zero
// Length's included; a length of all ones reaches the last byte.
type ByteRange = ranges.Range

// ChangeMask names what a change does to a file or directory.
type ChangeMask uint16

const (
	ChangeRemove   ChangeMask = 1 << iota // a name of the file goes
	ChangeRename                          // the file is renamed, or a rename replaces it
	ChangeAddEntry                        // a name is added to the directory
	ChangeAttrs                           // the file's attributes are set
	ChangeSetID                           // a mode change sets the setuid or setgid bit
)

// Checks is the view the service asks before I/O and before a change.
type Checks interface {
	// CheckIO admits I/O to f through o, an open, a lock state or a
	// delegation.
	// A lock state is judged as the open it was taken under: a granted open
	// is used only by its opener and only for the access it holds, ErrAccess
	// otherwise, and its deny modes were judged once, at open. An anonymous
	// read or write, and one through a delegation, is judged against every
	// deny mode held, per operation; a delegation writes only with write
	// caching. In grace, an anonymous one is ErrGrace while f's shard's
	// records name a client that can hold a deny mode or a mandatory lock,
	// which may still be reclaimed. Any is refused ErrLocked across a range a
	// mandatory lock forbids it, and recalls another holder's caching the I/O
	// conflicts with, answering ErrDelay until the recall ends.
	CheckIO(ctx context.Context, who Caller, f FileKey, o OpenRef, r ByteRange, write bool) error
	// CheckChange admits a change to f, made through via, an open of f, or
	// through none: what the open state of f refuses. It recalls the caching
	// grants the change conflicts with and never waits, answering ErrDelay
	// until each recall ends. Only a grant under via's lease key, or the
	// oplock over via, is the caller's own; with no via, every grant of the
	// caller's client is. In grace, a remove or rename in a shard whose
	// records name a client that can hold a deny mode is ErrGrace.
	CheckChange(ctx context.Context, who Caller, f FileKey, via *OpenRef, what ChangeMask) error
}

type checks struct{ t *Table }

// Checks is the view of the checks before I/O and before a change.
func (t *Table) Checks() Checks { return checks{t} }

// ponytail: an anonymous check on a file with no state still creates and
// frees its entry, an allocation per NFSv3 read or write. Look the entry up
// without creating it when CheckIO shows in a profile.
func (v checks) CheckIO(ctx context.Context, who Caller, f FileKey, o OpenRef, r ByteRange, write bool) error {
	t := v.t
	sink, err := t.running()
	if err != nil {
		return err
	}
	want, keeps := entity.ReadData, GrantRead|GrantHandle
	if write {
		want, keeps = entity.WriteData, GrantNone
	}
	others := func(*grant) GrantKind { return keeps }
	var out outbox
	through := func(e *entry, op *open) error {
		if !t.confirmed(op) {
			return ErrBadState
		}
		if conflict.Of(want)&^conflict.Of(op.Mode.Access) != 0 {
			return ErrAccess
		}
		if err := e.lockedOut(op.Ref.Token, r, write); err != nil {
			return err
		}
		return t.recallOthers(e, func(g *grant) bool { return g.heldBy(op.Client, op.lease, op) }, others, &out)
	}
	// unopened is I/O through no open: anonymous, or through a delegation
	// own holds, which its holder's own deny modes do not refuse.
	unopened := func(e *entry, own func(*grant) bool) error {
		var hs []*open
		for _, h := range e.opens {
			if o.Kind == RefDelegated && h.Client == who.Client {
				continue
			}
			if !conflict.Shares(want, 0, h.Mode.Access, h.Mode.Deny) {
				hs = append(hs, h)
			}
		}
		if err := t.breakHandles(e, hs, own, &out); err != nil {
			return err
		}
		if err := e.lockedOut(0, r, write); err != nil {
			return err
		}
		return t.recallOthers(e, own, others, &out)
	}
	switch o.Kind {
	case RefAnonymous:
		var id entity.FileID
		if id, err = o.file(); err == nil && id.IsZero() {
			err = ErrBadState
		}
		if err == nil && t.guarded(f.Shard) {
			err = ErrGrace
		}
		if err == nil {
			err = t.step(id, func(e *entry) error {
				return unopened(e, func(g *grant) bool { return g.heldBy(who.Client, LeaseKey{}, nil) })
			})
		}
	case RefDelegated:
		err = t.throughGrant(who, o, write, unopened)
	case RefLocked:
		err = t.throughLockState(who, o, through)
	default:
		err = t.withOpen(who, o, nil, seqOther, through)
	}
	out.raise(t, sink)
	return err
}

// throughGrant runs fn, as I/O through the delegation ref names, once it is
// found under its file's step: one without write caching is ErrAccess for a
// write, and a revoked one ErrRevoked.
func (t *Table) throughGrant(who Caller, ref OpenRef, write bool, fn func(e *entry, own func(*grant) bool) error) error {
	f, err := ref.file()
	if err != nil {
		return err
	}
	h, err := t.resolve(who.Client, f, ref.State)
	if err != nil {
		return err
	}
	return t.step(h.file, func(e *entry) error {
		if _, err := t.resolve(who.Client, e.file, ref.State); err != nil {
			return err
		}
		g := e.grants[ref.State.Token]
		switch {
		case g == nil || g.covers():
			return ErrBadState
		case write && g.fsm.Kind&GrantWrite == 0:
			return ErrAccess
		}
		return fn(e, func(x *grant) bool { return x.heldBy(who.Client, LeaseKey{}, nil) })
	})
}

// decision: the step is released before the change commits, so the check
// holds only while the service keeps the file's admission lock from the
// check to the commit, as it does for every Checks call: an open granted in
// between would not be judged against the change. Hold the step to the
// commit, with a callback run under it, if a caller is found that changes a
// file without that lock.
//
// decision: only a caller that is not SMB is refused a remove or rename
// against a deny-delete, and only an SMB caller is refused one of a
// directory an open was made through. An SMB client removes and renames
// through an open holding delete access, whose own deny modes were judged at
// open, so judging its own open's deny-delete again would refuse a rename
// through an exclusive handle; and POSIX allows an NFS rename of a directory
// with open children. Judge every caller if a protocol is served that
// removes a name without an open of its own.
func (v checks) CheckChange(ctx context.Context, who Caller, f FileKey, via *OpenRef, what ChangeMask) error {
	t := v.t
	sink, err := t.running()
	if err != nil {
		return err
	}
	if what&(ChangeRemove|ChangeRename) != 0 && t.guarded(f.Shard) {
		return ErrGrace // a name may be held by a deny mode still to be reclaimed
	}
	smb := who.Protocol == entity.ProtocolSMB
	var out outbox
	err = t.step(f.ID, func(e *entry) error {
		own := func(g *grant) bool { return g.client == who.Client }
		if via != nil {
			_, k, err := t.findOpen(who, *via)
			if err != nil {
				return err
			}
			o := e.opens[k.token]
			if o == nil {
				return ErrBadState
			}
			own = func(g *grant) bool { return g.heldBy(who.Client, o.lease, o) }
		}
		// decision: a delete-pending directory also refuses an added name,
		// beyond RFC 14's opens and renames, as Windows answers a create in
		// one STATUS_DELETE_PENDING. Drop it if an NFS workload is shown to
		// create in a directory an SMB client has marked.
		if what&(ChangeRename|ChangeAddEntry) != 0 && e.pending != nil {
			return ErrDeletePending
		}
		if what&(ChangeRemove|ChangeRename) != 0 && !smb {
			var hs []*open
			for _, h := range e.opens {
				if conflict.Of(h.Mode.Deny)&conflict.Delete != 0 {
					hs = append(hs, h)
				}
			}
			if err := t.breakHandles(e, hs, own, &out); err != nil {
				return err
			}
		}
		if what&(ChangeRemove|ChangeRename) != 0 && smb {
			t.idx.Lock()
			children := t.via[f.ID] > 0
			t.idx.Unlock()
			if children {
				return ErrShareViolation
			}
		}
		return t.recallOthers(e, func(*grant) bool { return false }, func(g *grant) GrantKind {
			k := ^GrantNone
			if what&ChangeSetID != 0 {
				k &^= GrantWrite
			}
			if !own(g) && what&(ChangeRemove|ChangeRename|ChangeAttrs) != 0 {
				k = GrantNone
			}
			return k
		}, &out)
	})
	out.raise(t, sink)
	return err
}

// throughLockState runs fn, as I/O through the open the lock state ref
// names was taken under, once that open is found and who may use it. A lock
// state taken under no open names nothing I/O runs through.
func (t *Table) throughLockState(who Caller, ref OpenRef, fn func(e *entry, op *open) error) error {
	f, err := ref.file()
	if err != nil {
		return err
	}
	h, err := t.resolve(who.Client, f, ref.State)
	if err != nil {
		return err
	}
	return t.step(h.file, func(e *entry) error {
		op, _, cur, err := t.lockTarget(e, who, ref)
		switch {
		case err != nil:
			return err
		case op == nil:
			return ErrBadState
		case !t.mayUse(op, who):
			return ErrNotYours
		}
		if err := seqErr(cur, ref.State.Seq); err != nil {
			return err
		}
		return fn(e, op)
	})
}

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
)

// Checks is the view the service asks before I/O and before a change.
type Checks interface {
	// CheckIO admits I/O through o, an open or a lock state, which is judged
	// as the open it was taken under: a granted open is used only by its
	// opener and only for the access it holds, ErrAccess otherwise, and its
	// deny modes were judged once, at open; an anonymous read or write is
	// judged against every deny mode held, per operation. Either is refused
	// ErrLocked across a range a mandatory lock forbids it.
	CheckIO(ctx context.Context, who Caller, o OpenRef, r ByteRange, write bool) error
	// CheckChange admits a change to f: what the open state of f refuses.
	CheckChange(ctx context.Context, who Caller, f FileKey, what ChangeMask) error
}

type checks struct{ t *Table }

// Checks is the view of the checks before I/O and before a change.
func (t *Table) Checks() Checks { return checks{t} }

// ponytail: an anonymous check on a file with no state still creates and
// frees its entry, an allocation per NFSv3 read or write. Look the entry up
// without creating it when CheckIO shows in a profile.
func (v checks) CheckIO(ctx context.Context, who Caller, o OpenRef, r ByteRange, write bool) error {
	t := v.t
	want := entity.ReadData
	if write {
		want = entity.WriteData
	}
	through := func(e *entry, op *open) error {
		if !t.confirmed(op) {
			return ErrBadState
		}
		if conflict.Of(want)&^conflict.Of(op.Mode.Access) != 0 {
			return ErrAccess
		}
		return e.lockedOut(op.Ref.Token, r, write)
	}
	switch o.Kind {
	case RefAnonymous:
	case RefLocked:
		return t.throughLockState(who, o, through)
	default:
		return t.withOpen(who, o, nil, false, through)
	}
	f, err := o.file()
	if err != nil || f.IsZero() {
		return ErrBadState
	}
	return t.step(f, func(e *entry) error {
		for _, h := range e.opens {
			if !conflict.Shares(want, 0, h.Mode.Access, h.Mode.Deny) {
				return ErrShareViolation
			}
		}
		return e.lockedOut(0, r, write)
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
func (v checks) CheckChange(ctx context.Context, who Caller, f FileKey, what ChangeMask) error {
	t := v.t
	smb := who.Protocol == entity.ProtocolSMB
	return t.step(f.ID, func(e *entry) error {
		// decision: a delete-pending directory also refuses an added name,
		// beyond RFC 14's opens and renames, as Windows answers a create in
		// one STATUS_DELETE_PENDING. Drop it if an NFS workload is shown to
		// create in a directory an SMB client has marked.
		if what&(ChangeRename|ChangeAddEntry) != 0 && e.pending != nil {
			return ErrDeletePending
		}
		if what&(ChangeRemove|ChangeRename) == 0 {
			return nil
		}
		if !smb {
			for _, h := range e.opens {
				if conflict.Of(h.Mode.Deny)&conflict.Delete != 0 {
					return ErrShareViolation
				}
			}
			return nil
		}
		t.idx.Lock()
		defer t.idx.Unlock()
		if t.via[f.ID] > 0 {
			return ErrShareViolation
		}
		return nil
	})
}

// throughLockState runs fn, as I/O through the open the lock state ref
// names was taken under, once that open is found and who may use it. A lock
// state taken under no open names nothing I/O runs through.
func (t *Table) throughLockState(who Caller, ref OpenRef, fn func(e *entry, op *open) error) error {
	f, err := ref.file()
	if err != nil {
		return err
	}
	h, err := t.resolve(who.Client, f, StateRef{Type: ref.State.Type, Shard: ref.State.Shard, Token: ref.State.Token})
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

package openstate

import (
	"context"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/openstate/conflict"
)

// ByteRange is the bytes an I/O covers. No check reads it yet: byte-range
// locks are what judge it.
type ByteRange struct {
	Offset, Length uint64
}

// ChangeMask names what a change does to a file or directory. Each kind is
// added with the check that reads it.
type ChangeMask uint16

const (
	ChangeRemove   ChangeMask = 1 << iota // a name of the file goes
	ChangeRename                          // the file is renamed, or a rename replaces it
	ChangeAddEntry                        // a name is added to the directory
)

// Checks is the view the service asks before I/O and before a change.
type Checks interface {
	// CheckIO admits I/O through o: a granted open is used only by its
	// opener, and its deny modes were judged once, at open; an anonymous
	// read or write is judged against every deny mode held, per operation.
	CheckIO(ctx context.Context, who Caller, o OpenRef, r ByteRange, write bool) error
	// CheckChange admits a change to f: what the open state of f refuses.
	CheckChange(ctx context.Context, who Caller, f entity.FileID, what ChangeMask) error
}

type checks struct{ t *Table }

// Checks is the view of the checks before I/O and before a change.
func (t *Table) Checks() Checks { return checks{t} }

// ponytail: an anonymous check on a file with no state still creates and
// frees its entry, an allocation per NFSv3 read or write. Look the entry up
// without creating it when CheckIO shows in a profile.
func (v checks) CheckIO(ctx context.Context, who Caller, o OpenRef, _ ByteRange, write bool) error {
	t := v.t
	if !o.anonymous() {
		return t.withOpen(who, o, func(*entry, *open) error { return nil })
	}
	if o.File.IsZero() {
		return ErrBadState
	}
	want := entity.ReadData
	if write {
		want = entity.WriteData
	}
	return t.step(o.File, func(e *entry) error {
		for _, h := range e.opens {
			if !conflict.Shares(want, 0, h.Mode.Access, h.Mode.Deny) {
				return ErrShareViolation
			}
		}
		return nil
	})
}

// decision: only a caller that is not SMB is refused a remove or rename
// against a deny-delete, and only an SMB caller is refused one of a
// directory an open was made through. An SMB client removes and renames
// through an open holding delete access, whose own deny modes were judged at
// open, so judging its own open's deny-delete again would refuse a rename
// through an exclusive handle; and POSIX allows an NFS rename of a directory
// with open children. Judge every caller if a protocol is served that
// removes a name without an open of its own.
func (v checks) CheckChange(ctx context.Context, who Caller, f entity.FileID, what ChangeMask) error {
	t := v.t
	smb := t.protocol(who.Client) == entity.ProtocolSMB
	return t.step(f, func(e *entry) error {
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
		if t.via[f] > 0 {
			return ErrShareViolation
		}
		return nil
	})
}

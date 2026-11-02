package kvstore

import (
	"bytes"
	"context"
	"iter"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/keys"
)

// Bounds on a file's xattrs, so the release that deletes them stays one
// bounded transaction.
//
// ponytail: fixed here; they are share settings once settings reach the
// namespace.
const (
	maxXattrValue = 64 << 10
	maxXattrs     = 128
)

// changed stamps a change of f: Change advances to the later of now and
// just past its current value, so a clock behind the last writer's never
// moves it back, and Version is drawn above the file's.
func changed(tx kv.Txn, f *entity.File) {
	f.Change = laterOf(tx.Now(), f.Change)
	f.Version = nextFileVersion(tx, f.Version)
}

func laterOf(now, prev time.Time) time.Time {
	if next := prev.Add(time.Microsecond); !now.After(next) {
		return next
	}
	return now
}

// SetAttrs changes the attributes a names. Only a privileged caller changes
// the owner or the immutable and append-only flags; the owner may change
// the group to one it is in, the mode and the times, and a caller holding
// write the ACL the mode's permission bits: never setuid or setgid on a
// non-directory, and never the sticky bit, which it neither sets nor clears.
// A chmod on a file with a stored ACL merges into the ACL, and both are
// written together.
//
// decision: a non-owner holding write the ACL may chmod, though RFC 19 §4.1
// refuses a non-owner's mode set ErrPerm: SetACL already lets it set the
// permission bits, and the special bits are kept from it as above. Refuse it
// ErrPerm, or amend RFC 7 §7.4 and RFC 19 §4.1, once one is agreed.
//
// A size change is refused: it goes through an existence commit, which
// needs the write path.
//
// ponytail: an explicit Modify or Access is written to the record here. Once
// writes are staged by the engine it must go through an existence commit
// after them, or a staged write's commit would overwrite the time.
func (f Files) SetAttrs(ctx context.Context, id entity.Identity, h entity.Handle, a entity.Attrs) (entity.File, error) {
	if a.Changes(entity.AttrSize) {
		return entity.File{}, merrs.NewInvalidArgumentError("a size change goes through the write path")
	}
	var out entity.File
	err := f.s.kv.Update(ctx, func(tx kv.Txn) error {
		share, _, err := h.Parse()
		if err != nil {
			return err
		}
		_, file, err := resolveIn(tx, share, h)
		if err != nil {
			return err
		}
		if err := mayChange(tx, id, file, a); err != nil {
			return err
		}
		// A directory's unfolded deltas fold first, so a time set
		// explicitly here is not overtaken by a later delta's on read.
		//
		// ponytail: it folds every unfolded delta in this transaction,
		// unbounded; the primary's regular fold keeps them few. Fold only
		// those before the cut the transaction read when cuts land.
		stored := file.Version
		if file.IsDir() {
			if _, _, err := foldDeltas(tx, &file, -1); err != nil {
				return err
			}
		}
		next := file.Apply(a)
		if a.Changes(entity.AttrMode) && !file.IsDir() && next.Mode&entity.ModeSetgid != 0 && !id.Privileged && !id.InGroup(next.Group) {
			next.Mode &^= entity.ModeSetgid // a non-member cannot make a setgid program of a group it is not in
		}
		if a.Changes(entity.AttrMode) && !id.Privileged && id.Principal != file.Owner {
			// A write-ACL holder's chmod sets only the permission bits: one
			// who is not the owner cannot make a setuid or setgid program
			// that runs as the owner or the owner's group, nor set or clear
			// the sticky bit that keeps others' entries from it. A directory's
			// setuid and setgid pass as asked: it runs nothing, and its setgid
			// names the group new entries take.
			if !file.IsDir() {
				next.Mode &^= entity.ModeSetuid | entity.ModeSetgid
			}
			next.Mode = next.Mode&^entity.ModeSticky | file.Mode&entity.ModeSticky
		}
		if a.Changes(entity.AttrMode) {
			if b, ok, err := get(tx, aclKey(share, file.ID)); err != nil {
				return err
			} else if ok {
				acl, err := decodeACL(b)
				if err != nil {
					return err
				}
				if err := tx.Set(aclKey(share, file.ID), encodeACL(acl.WithMode(next.Mode&0o777))); err != nil {
					return err
				}
			}
		}
		changed(tx, &next)
		out = next
		if err := chargeFile(tx, share, &file, &next); err != nil {
			return err
		}
		return putFile(tx, stored, next)
	})
	return out, err
}

// mayChange decides whether the caller may make the attribute change a.
func mayChange(r kv.Reader, id entity.Identity, f entity.File, a entity.Attrs) error {
	// Every change is a write to the share, which a read-only grant refuses
	// before any privilege below is considered.
	if err := grantAllows(r, id, f.Share, entity.WriteAttributes); err != nil {
		return err
	}
	const locked = entity.FlagImmutable | entity.FlagAppendOnly
	if f.Flags&locked != 0 {
		// The one change such a file accepts, from anyone, is a privileged
		// caller setting or clearing those two flags.
		if a.Mask != entity.AttrFlags || (a.Flags^f.Flags)&^locked != 0 {
			return merrs.NewPermissionDeniedError("immutable or append-only " + f.ID.String())
		}
		if !id.Privileged {
			return merrs.NewPrivilegeRequiredError("clear immutable or append-only")
		}
		return nil
	}
	if id.Privileged {
		return nil
	}
	owner := id.Principal == f.Owner
	if a.Changes(entity.AttrMode) && !owner {
		// A chmod asks write the ACL, which the owner holds implicitly and
		// an entry can grant to another.
		if err := authorize(r, id, f, entity.WriteACL); err != nil {
			return err
		}
		if a.Mask &^= entity.AttrMode; a.Mask == 0 {
			return nil
		}
	}
	switch {
	case a.Changes(entity.AttrOwner) && a.Owner != f.Owner:
		return merrs.NewPrivilegeRequiredError("change owner")
	case a.Changes(entity.AttrFlags) && (a.Flags^f.Flags)&(entity.FlagImmutable|entity.FlagAppendOnly) != 0:
		return merrs.NewPrivilegeRequiredError("set or clear immutable or append-only")
	case a.Changes(entity.AttrGroup) && a.Group != f.Group && (!owner || !id.InGroup(a.Group)):
		return merrs.NewPermissionDeniedError("change group")
	case a.Mask&^(entity.AttrOwner|entity.AttrGroup) != 0 && !owner:
		if a.Mask&^times != 0 {
			return merrs.NewPermissionDeniedError("only the owner changes the mode or flags")
		}
		// A non-owner sets times only through write attributes an ACL
		// entry grants; one that a mode bit grants allows no set of a
		// time the client gives.
		//
		// decision: setting the times to the server's time with write data
		// alone, as utimensat's UTIME_NOW allows a writer, is refused too:
		// Attrs carries a time, not whether it is the server's. Allow it once
		// the call can say so.
		ok, err := aclGrants(r, id, f, entity.WriteAttributes)
		if err != nil {
			return err
		}
		if !ok {
			return merrs.NewPermissionDeniedError("only the owner, or write attributes in an ACL, changes the times")
		}
	}
	return authorize(r, id, f, entity.WriteAttributes)
}

// times are the attributes a non-owner may set with write attributes from
// an ACL entry.
const times = entity.AttrBirth | entity.AttrAccess | entity.AttrModify

// ClearSetID clears setuid and setgid on a regular file, as an unprivileged
// caller's write clears them, and changes nothing else: unlike a chmod, it
// merges nothing into a stored ACL. It asks write data, the right of the
// write that clears them, or append data of an append-only file, which takes
// appends alone. A file with neither bit is left as it is.
func (f Files) ClearSetID(ctx context.Context, id entity.Identity, h entity.Handle) (entity.File, error) {
	var out entity.File
	err := f.s.kv.Update(ctx, func(tx kv.Txn) error {
		share, _, err := h.Parse()
		if err != nil {
			return err
		}
		_, file, err := resolveIn(tx, share, h)
		if err != nil {
			return err
		}
		want := entity.WriteData
		if file.IsAppendOnly() {
			want = entity.AppendData
		}
		if err := authorize(tx, id, file, want); err != nil {
			return err
		}
		out = file
		m := file.ClearSetID()
		if m == file.Mode {
			return nil
		}
		was := file.Version
		file.Mode = m
		changed(tx, &file)
		out = file
		return putFile(tx, was, file)
	})
	return out, err
}

// ACL is the file's access-control list: the stored one, or the one its
// mode implies.
func (f Files) ACL(ctx context.Context, id entity.Identity, h entity.Handle) (entity.ACL, error) {
	var acl entity.ACL
	err := f.s.kv.View(ctx, func(r kv.Reader) error {
		share, _, err := h.Parse()
		if err != nil {
			return err
		}
		_, file, err := resolveIn(r, share, h)
		if err != nil {
			return err
		}
		if err := authorize(r, id, file, entity.ReadACL); err != nil {
			return err
		}
		acl, err = aclOf(r, file)
		return err
	})
	return acl, err
}

func aclOf(r kv.Reader, f entity.File) (entity.ACL, error) {
	b, ok, err := get(r, aclKey(f.Share, f.ID))
	if err != nil || !ok {
		return entity.ACLFromMode(f.Mode & 0o777), err
	}
	return decodeACL(b)
}

// SetACL stores an ACL and the mode it implies, in one transaction, so the
// two always agree. Setuid, setgid and sticky are kept from the file.
func (f Files) SetACL(ctx context.Context, id entity.Identity, h entity.Handle, acl entity.ACL) (entity.File, error) {
	if !acl.Valid() {
		return entity.File{}, merrs.NewInvalidArgumentError("an ACL entry is of the wrong list, names nobody, or denies an unknown special principal")
	}
	acl = acl.Stored()
	var out entity.File
	err := f.s.kv.Update(ctx, func(tx kv.Txn) error {
		share, _, err := h.Parse()
		if err != nil {
			return err
		}
		_, file, err := resolveIn(tx, share, h)
		if err != nil {
			return err
		}
		if err := authorize(tx, id, file, entity.WriteACL); err != nil {
			return err
		}
		was := file.Version
		file.Mode = file.Mode&^0o777 | acl.Mode()
		changed(tx, &file)
		if err := tx.Set(aclKey(share, file.ID), encodeACL(acl)); err != nil {
			return err
		}
		out = file
		return putFile(tx, was, file)
	})
	return out, err
}

// Xattrs lists a file's xattrs in name order.
func (f Files) Xattrs(ctx context.Context, id entity.Identity, h entity.Handle) iter.Seq2[entity.Xattr, error] {
	return func(yield func(entity.Xattr, error) bool) {
		var xs []entity.Xattr
		err := f.s.kv.View(ctx, func(r kv.Reader) error {
			share, _, err := h.Parse()
			if err != nil {
				return err
			}
			_, file, err := resolveIn(r, share, h)
			if err != nil {
				return err
			}
			if err := authorize(r, id, file, entity.ReadNamedAttrs); err != nil {
				return err
			}
			xs, err = xattrsOf(r, file)
			return err
		})
		if err != nil {
			yield(entity.Xattr{}, err)
			return
		}
		for _, x := range xs {
			if !yield(x, nil) {
				return
			}
		}
	}
}

func xattrsOf(r kv.Reader, f entity.File) ([]entity.Xattr, error) {
	prefix := xattrsPrefix(f.Share, f.ID)
	var xs []entity.Xattr
	for row, err := range r.Scan(prefix, nil) {
		if err != nil {
			return nil, err
		}
		name, _, err := keys.ReadString(row.Key[len(prefix):])
		if err != nil {
			return nil, err
		}
		x, err := decodeXattr(f.ID, name, row.Value)
		if err != nil {
			return nil, err
		}
		xs = append(xs, x)
	}
	return xs, nil
}

// SetXattr sets one xattr, refusing one past the file's bounds.
func (f Files) SetXattr(ctx context.Context, id entity.Identity, h entity.Handle, x entity.Xattr) error {
	if len(x.Name) == 0 || len(x.Name) > maxNameBytes || bytes.IndexByte(x.Name, 0) >= 0 {
		return entity.ErrInvalidName
	}
	if len(x.Value) > maxXattrValue {
		return merrs.NewInvalidArgumentError("xattr value too large")
	}
	return f.changeXattr(ctx, id, h, func(tx kv.Txn, file entity.File) error {
		key := xattrKey(file.Share, file.ID, x.Name)
		if _, ok, err := get(tx, key); err != nil {
			return err
		} else if !ok {
			xs, err := xattrsOf(tx, file)
			if err != nil {
				return err
			}
			if len(xs) >= maxXattrs {
				return merrs.NewInvalidArgumentError("too many xattrs on one file")
			}
		}
		return tx.Set(key, encodeXattr(x.Value))
	})
}

// RemoveXattr removes one xattr; removing one that is not there is an
// error, as both protocols report it.
func (f Files) RemoveXattr(ctx context.Context, id entity.Identity, h entity.Handle, name []byte) error {
	return f.changeXattr(ctx, id, h, func(tx kv.Txn, file entity.File) error {
		key := xattrKey(file.Share, file.ID, name)
		if _, ok, err := get(tx, key); err != nil {
			return err
		} else if !ok {
			return merrs.NewNotFoundError(string(name), "xattr")
		}
		return tx.Delete(key)
	})
}

// changeXattr runs one xattr change and stamps the file's Change and
// Version with it.
func (f Files) changeXattr(ctx context.Context, id entity.Identity, h entity.Handle, change func(kv.Txn, entity.File) error) error {
	return f.s.kv.Update(ctx, func(tx kv.Txn) error {
		share, _, err := h.Parse()
		if err != nil {
			return err
		}
		_, file, err := resolveIn(tx, share, h)
		if err != nil {
			return err
		}
		if err := authorize(tx, id, file, entity.WriteNamedAttrs); err != nil {
			return err
		}
		if err := change(tx, file); err != nil {
			return err
		}
		was := file.Version
		changed(tx, &file)
		return putFile(tx, was, file)
	})
}

package vfs

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/cubbit/dittofs/internal/vfs/lockorder"
	"github.com/cubbit/dittofs/internal/vfs/quota"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/openstate"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// The Attributes area: a file's attributes, ACL, xattrs and streams, read
// from the store and, for its size, times and version, joined with the
// engine's overlay; each change made under the file's admission and accept
// locks, once open state admits it, and answered with the WCC read in that
// one hold.
//
// ponytail: no change draws a version from the engine: the store's SetAttrs,
// SetACL and xattr changes each draw their own. Draw one under the accept
// lock and hand it to the store once the store takes a version drawn by its
// caller.

// pseudoRights is what a caller may do in a pseudo directory, a read-only
// directory of mode 0555.
const pseudoRights = entity.ReadAttributes | entity.ReadACL | entity.Synchronize | entity.ListDirectory | entity.Execute

// GetAttr is h's attributes: the store's record joined with the engine's
// overlay of what is staged and not yet committed. A pseudo directory
// answers its own.
//
// ponytail: GetAttr decides read attributes through authz, so the decision is
// audited, and Files.Get decides it again inside: two reads of the file where
// one would do. Drop the outer decision once the store reports its own for
// audit, when GETATTR shows in a profile.
func (fs *Filesystem) GetAttr(ctx context.Context, c vfs.Call, h vfs.Handle) (vfs.File, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return vfs.File{}, err
	}
	defer done()
	a, release, err := fs.admitHandle(ctx, c, h)
	if err != nil {
		return vfs.File{}, err
	}
	defer release()
	if a == nil {
		t, err := fs.tree(ctx)
		if err != nil {
			return vfs.File{}, err
		}
		n, ok := t.Node(h)
		if !ok {
			return vfs.File{}, vfs.ErrStale
		}
		return pseudoFile(t, n), nil
	}
	if err := fs.authz.Decide(ctx, a.caller, vfs.OpGetAttr, h, nil, nil, entity.ReadAttributes); err != nil {
		return vfs.File{}, storeErr(err)
	}
	return fs.file(ctx, a, h)
}

// file is h's record joined with the engine's overlay.
func (fs *Filesystem) file(ctx context.Context, a *admitted, h vfs.Handle) (vfs.File, error) {
	f, err := fs.meta.Files().Get(ctx, a.caller.Identity, h)
	if err != nil {
		return vfs.File{}, storeErr(err)
	}
	return fs.joined(ctx, f)
}

// joined is f with the engine's overlay applied: with nothing pending, the
// record alone; otherwise the overlay's size when a truncate is pending, else
// the larger of it and the record's.
//
// decision: the times and the version are the later of the overlay's and the
// record's, not the overlay's alone: a metadata change committed after a
// staged write stamps the record later than the overlay, and the overlay's
// would move ctime and the change attribute backward. So an explicit mtime
// older than a staged write's reads as the write's until that write commits.
// Overturned once metadata changes draw from the engine's version counter and
// are replayed by the overlay in order.
//
// ponytail: the record and the overlay are read one after the other, not
// under the file's commit serialisation retried as a sequence lock, so a
// commit landing between them can answer a size or version lower than one
// already returned. Read them under it once the engine commits.
func (fs *Filesystem) joined(ctx context.Context, f vfs.File) (vfs.File, error) {
	if !f.IsRegular() && !f.IsStream() {
		return f, nil
	}
	o, pending, err := fs.engine.Overlay(ctx, f.Share, f.ID)
	if err != nil || !pending {
		return f, storeErr(err)
	}
	if o.SizeSet {
		f.Size = o.Size
	} else {
		f.Size = max(f.Size, o.Size)
	}
	f.Modify, f.Change = later(f.Modify, o.Mtime), later(f.Change, o.Ctime)
	if f.Version.Less(o.Version) {
		f.Version = o.Version
	}
	return f, nil
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// errGuard answers a SetAttr whose ctime guard no longer holds.
//
// decision: a failed guard is ErrInvalid, as the service has no sentinel of
// its own for it; the NFSv3 adapter, the one caller that sends a guard, maps
// ErrInvalid on a guarded SETATTR to NOT_SYNC. Overturned by a second cause
// of ErrInvalid on a guarded SetAttr that the adapter must tell apart.
var errGuard = fmt.Errorf("vfs: the ctime guard no longer holds: %w", vfs.ErrInvalid)

// SetAttr changes t's attributes as ch names them, under t's admission and
// accept locks, once an owner or group change has committed the file's
// pending existence: the ctime guard first, then the rights the change asks,
// decided through authz, then open state, then, for an owner or group
// change, the file's charge reserved against the new owner and group, then
// the change, whose value the store's ownership rules decide. A size change is the engine's Truncate,
// after an unprivileged caller's change has cleared setuid and setgid, in a
// change committed before the truncate is staged. DeleteOnClose, Suspend and
// Resume go to the open t names, and nowhere else.
//
// decision: a change through an open is decided against the file, not the
// open's stored grant, which the store cannot be handed: a truncate through
// an open whose caller has since lost write on the file is refused. A time
// set through an open by a caller the owner rule refuses, which RFC 7 §7.7
// lets an open holding write attributes make whoever owns the file, is
// decided the same way, admitted by open state only when the open's grant
// holds write attributes, then written as privileged, since the store's
// owner rule cannot see the open; a change setting anything beside times
// keeps that rule. Overturned once
// Authorize takes an open's grant in place of the walk and SetAttrs takes
// the open.
func (fs *Filesystem) SetAttr(ctx context.Context, c vfs.Call, t vfs.Target, ch vfs.AttrChange) (vfs.WCC, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return vfs.WCC{}, err
	}
	defer done()
	// An owner or group change first commits the file's pending existence,
	// so the file's Charged holds every write staged before it rather than
	// leaving them to charge the new owner at their own commit. The commit
	// joins the group commit, so it runs before the admission lock is taken.
	//
	// decision: the commit runs for any change naming the owner or group,
	// before the change is authorised and whether or not it moves the
	// charge, as the file is read only under the locks. It writes nothing a
	// caller's COMMIT could not, so it grants nothing. Overturned if an
	// existence commit is ever found to be visible to a client.
	if ch.Mask&(entity.AttrOwner|entity.AttrGroup) != 0 {
		if err := fs.engine.CommitExistence(ctx, fileOf(t.H)); err != nil {
			return vfs.WCC{}, storeErr(err)
		}
	}
	a, f, release, err := fs.hold(ctx, c, t)
	if err != nil {
		return vfs.WCC{}, err
	}
	defer release()
	if ch.Guard != nil && !ch.Guard.Equal(f.Change) {
		return vfs.WCC{}, errGuard
	}
	viaOpen := ch.DeleteOnClose != nil || ch.Suspend|ch.Resume != 0
	if viaOpen && (t.Open == nil || t.Open.Open == 0) {
		return vfs.WCC{}, vfs.ErrInvalid
	}
	size := ch.Changes(entity.AttrSize)
	if size {
		switch {
		case f.IsDir():
			return vfs.WCC{}, vfs.ErrIsDir
		case !f.IsRegular() && !f.IsStream(), ch.Size < 0:
			return vfs.WCC{}, vfs.ErrInvalid
		}
	}
	if want := rightsOf(a.caller.Identity, f, ch.Attrs); want != 0 {
		if err := fs.authz.Decide(ctx, a.caller, vfs.OpSetAttr, t.H, nil, nil, want); err != nil {
			return vfs.WCC{}, storeErr(err)
		}
	}
	who, key := a.who(), keyOf(f)
	// onOpen: times set by a caller other than the owner, through an open,
	// which open state admits only on a grant holding write attributes; any
	// other change keeps the caller's own rights.
	//
	// decision: a non-owner whose ACL entry grants write attributes is judged
	// on the open too, so its time set through an open lacking them is refused
	// though RFC 7 §7.7 lets its own rights make it, and the same set on no
	// open succeeds: the store's owner rule is not visible here. It fails
	// closed. Overturned once the store's owner rules are a read-only check
	// this can call.
	id := a.caller.Identity
	onOpen := t.Open != nil && ch.Mask&^(attrTimes|entity.AttrSize) == 0 && ch.Mask&attrTimes != 0 &&
		!id.Privileged && id.Principal != f.Owner
	// decision: open state is asked, and a delete-on-close mark is set,
	// before the store's ownership rules judge the change's values, so a
	// chown or chgrp the store then refuses has still recalled other
	// clients' grants, and a delete-on-close sent with it stays marked
	// though the call answers the refusal. The recall costs a later grant
	// only; no protocol sends a disposition with an owner change in one
	// call. Judge the values first, through a read-only owner check the
	// store exposes, once a client is seen sending both together or a
	// recall storm is traced to refused chowns.
	if what := changeOf(ch.Attrs); what != 0 {
		if !onOpen {
			what &^= openstate.ChangeTimes
		}
		if err := fs.checks.CheckChange(ctx, who, key, t.Open, what); err != nil {
			return vfs.WCC{}, openErr(err)
		}
	}
	if size {
		ref := openstate.OpenRef{Kind: openstate.RefAnonymous, Handle: t.H}
		if t.Open != nil {
			ref = *t.Open
		}
		cut := openstate.ByteRange{Offset: uint64(min(f.Size, ch.Size)), Length: ^uint64(0)}
		if err := fs.checks.CheckIO(ctx, who, key, ref, cut, entity.WriteData); err != nil {
			return vfs.WCC{}, openErr(err)
		}
	}
	// An owner or group change moves the file's charge, its bytes and one
	// inode, in the store's transaction. Under the admission lock the charge
	// is rehomed: Charged, which the existence commit above brought up to
	// date, and one inode are reserved against the owner and group the change
	// leaves, against only the scopes it moves into, together with every
	// write reservation the file still holds, which moves there. A write
	// staged since the commit therefore charges the new owner checked,
	// whatever its bytes fill. The lock is held to the store's apply, so no
	// write is staged between; the reservation is released once the store
	// has applied the change, and reverted, the writes moved back, when it
	// did not. A privileged caller is held to the limit as any other.
	applied := false
	if mv, scopes := moved(f, ch.Attrs); len(scopes) > 0 {
		res, err := fs.rehome(ctx, a, mv, entity.Usage{Bytes: f.Charged, Files: 1}, scopes...)
		if err != nil {
			return vfs.WCC{}, err
		}
		defer func() {
			if applied {
				res.Release()
			} else {
				res.Revert()
			}
		}()
	}
	if viaOpen {
		u := openstate.OpenUpdate{Delete: ch.DeleteOnClose, Suspend: ch.Suspend, Resume: ch.Resume}
		if err := fs.opens.Update(ctx, who, t.Open.Open, u); err != nil {
			return vfs.WCC{}, openErr(err)
		}
	}
	rest := ch.Attrs
	rest.Mask &^= entity.AttrSize
	if rest.Mask != 0 {
		if onOpen {
			id.Privileged = true
		}
		if _, err := fs.meta.Files().SetAttrs(ctx, id, t.H, rest); err != nil {
			return vfs.WCC{}, storeErr(err)
		}
		applied = true
	}
	if size {
		if err := fs.truncate(ctx, a, t.H, f, ch.Attrs); err != nil {
			return vfs.WCC{}, err
		}
	}
	return fs.wcc(ctx, c, a, t.H, f), nil
}

// moved is whom a change of f's owner or group moves its charge to, the
// owner and group it leaves, each f's own where ch does not change it, and
// the scopes it moves into: the user scope for a new owner and the group
// scope for a new group; none when it changes neither.
func moved(f vfs.File, ch vfs.Attrs) (c quota.Charge, scopes []vfs.QuotaScope) {
	c = charged(f)
	if ch.Changes(entity.AttrOwner) && ch.Owner != f.Owner {
		c.Owner = ch.Owner
		scopes = append(scopes, vfs.QuotaUser)
	}
	if ch.Changes(entity.AttrGroup) && ch.Group != f.Group {
		c.Group = ch.Group
		scopes = append(scopes, vfs.QuotaGroup)
	}
	return c, scopes
}

// truncate stages f's size change in the engine, once an unprivileged
// caller's change has cleared setuid and setgid in a committed change, as a
// write's does, which changes the mode alone; a change naming the mode sets
// it explicitly instead.
func (fs *Filesystem) truncate(ctx context.Context, a *admitted, h vfs.Handle, f vfs.File, ch vfs.Attrs) error {
	if !ch.Changes(entity.AttrMode) {
		if _, err := fs.clearSetID(ctx, a, h, f, false); err != nil {
			return err
		}
	}
	return storeErr(fs.engine.Truncate(ctx, f.Share, f.ID, ch.Size))
}

// attrTimes are the attributes that are times.
const attrTimes = entity.AttrBirth | entity.AttrAccess | entity.AttrModify

// rightsOf is what an attribute change asks of the chokepoint, each
// attribute its own right: a size write data; a mode write the ACL; an owner
// change the owner; a group change the owner, or write the ACL for the
// file's owner; anything else write attributes.
func rightsOf(id entity.Identity, f vfs.File, a vfs.Attrs) vfs.Access {
	var want vfs.Access
	if a.Changes(entity.AttrSize) {
		want |= entity.WriteData
	}
	if a.Changes(entity.AttrMode) {
		want |= entity.WriteACL
	}
	if a.Changes(entity.AttrOwner) {
		want |= entity.WriteOwner
	}
	if a.Changes(entity.AttrGroup) {
		if id.Principal == f.Owner {
			want |= entity.WriteACL
		} else {
			want |= entity.WriteOwner
		}
	}
	if a.Mask&^(entity.AttrSize|entity.AttrMode|entity.AttrOwner|entity.AttrGroup) != 0 {
		want |= entity.WriteAttributes
	}
	return want
}

// changeOf is what open state judges of an attribute change: nothing for
// none.
func changeOf(a vfs.Attrs) openstate.ChangeMask {
	if a.Mask == 0 {
		return 0
	}
	what := openstate.ChangeAttrs
	if a.Changes(entity.AttrSize) {
		what |= openstate.ChangeSize
	}
	if a.Mask&attrTimes != 0 {
		what |= openstate.ChangeTimes
	}
	if a.Mask&(entity.AttrOwner|entity.AttrGroup|entity.AttrMode) != 0 {
		what |= openstate.ChangeSecurity
	}
	if a.Changes(entity.AttrMode) && a.Mode&(entity.ModeSetuid|entity.ModeSetgid) != 0 {
		what |= openstate.ChangeSetID
	}
	return what
}

// GetACL is h's ACL: the stored one, or the one its mode implies, its audit
// list left out for a caller other than an admin.
func (fs *Filesystem) GetACL(ctx context.Context, c vfs.Call, h vfs.Handle) (vfs.ACL, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return vfs.ACL{}, err
	}
	defer done()
	a, release, err := fs.admitHandle(ctx, c, h)
	if err != nil {
		return vfs.ACL{}, err
	}
	defer release()
	if a == nil {
		if _, err := fs.pseudoNode(ctx, h); err != nil {
			return vfs.ACL{}, err
		}
		return entity.ACLFromMode(0o555), nil
	}
	if err := fs.authz.Decide(ctx, a.caller, vfs.OpGetACL, h, nil, nil, entity.ReadACL); err != nil {
		return vfs.ACL{}, storeErr(err)
	}
	acl, err := fs.meta.Files().ACL(ctx, a.caller.Identity, h)
	if !a.caller.Admin {
		// Reading the audit list needs system security, which only an admin
		// holds; another caller's read-edit-write then sets the access
		// entries alone, which SetACL takes and which keeps the stored list.
		acl.Audit = nil
	}
	return acl, storeErr(err)
}

// SetACL stores acl on t's file and the mode it implies. Only an admin
// writes the audit list: another caller's set carrying audit entries is
// ErrAccess, and one carrying none keeps the file's.
//
// decision: the audit list is guarded here, not by the chokepoint, which has
// no system-security right to be asked for, so its refusal is not audited. A
// set carrying no audit entries keeps the stored ones rather than erasing
// them, as a set of the access list alone does over SMB. Move the guard into
// Authorize once the store takes that right.
func (fs *Filesystem) SetACL(ctx context.Context, c vfs.Call, t vfs.Target, acl vfs.ACL) (vfs.WCC, error) {
	return fs.change(ctx, c, t, vfs.OpSetACL, entity.WriteACL, openstate.ChangeAttrs|openstate.ChangeSecurity,
		func(a *admitted, _ vfs.File) error {
			if !a.caller.Admin {
				if len(acl.Audit) > 0 {
					return vfs.ErrAccess
				}
				sys := a.caller.Identity
				sys.Privileged = true
				stored, err := fs.meta.Files().ACL(ctx, sys, t.H)
				if err != nil {
					return storeErr(err)
				}
				acl.Audit = stored.Audit
			}
			_, err := fs.meta.Files().SetACL(ctx, a.caller.Identity, t.H, acl)
			return storeErr(err)
		})
}

// Xattrs is h's xattrs, in name order; a pseudo directory holds none.
func (fs *Filesystem) Xattrs(ctx context.Context, c vfs.Call, h vfs.Handle) ([]vfs.Xattr, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	a, release, err := fs.admitHandle(ctx, c, h)
	if err != nil || a == nil {
		return nil, fs.pseudoOnly(ctx, h, err)
	}
	defer release()
	if err := fs.authz.Decide(ctx, a.caller, vfs.OpXattrs, h, nil, nil, entity.ReadNamedAttrs); err != nil {
		return nil, storeErr(err)
	}
	return fs.xattrs(ctx, a, h)
}

func (fs *Filesystem) xattrs(ctx context.Context, a *admitted, h vfs.Handle) ([]vfs.Xattr, error) {
	var xs []vfs.Xattr
	for x, err := range fs.meta.Files().Xattrs(ctx, a.caller.Identity, h) {
		if err != nil {
			return nil, storeErr(err)
		}
		xs = append(xs, x)
	}
	return xs, nil
}

// SetXattr sets one xattr of t's file: XattrCreate refuses a name that
// exists, ErrExist, and XattrReplace one that does not, ErrNotFound. Both are
// judged under the file's accept lock, which every change of its xattrs
// holds.
func (fs *Filesystem) SetXattr(ctx context.Context, c vfs.Call, t vfs.Target, x vfs.Xattr, m vfs.XattrMode) (vfs.WCC, error) {
	return fs.change(ctx, c, t, vfs.OpSetXattr, entity.WriteNamedAttrs, openstate.ChangeAttrs,
		func(a *admitted, _ vfs.File) error {
			if m != vfs.XattrEither {
				xs, err := fs.xattrs(ctx, a, t.H)
				if err != nil {
					return err
				}
				has := false
				for _, y := range xs {
					has = has || bytes.Equal(y.Name, x.Name)
				}
				switch {
				case m == vfs.XattrCreate && has:
					return vfs.ErrExist
				case m == vfs.XattrReplace && !has:
					return vfs.ErrNotFound
				}
			}
			return storeErr(fs.meta.Files().SetXattr(ctx, a.caller.Identity, t.H, x))
		})
}

// RemoveXattr removes one xattr of t's file; one not there is ErrNotFound.
func (fs *Filesystem) RemoveXattr(ctx context.Context, c vfs.Call, t vfs.Target, name []byte) (vfs.WCC, error) {
	return fs.change(ctx, c, t, vfs.OpRemoveXattr, entity.WriteNamedAttrs, openstate.ChangeAttrs,
		func(a *admitted, _ vfs.File) error {
			return storeErr(fs.meta.Files().RemoveXattr(ctx, a.caller.Identity, t.H, name))
		})
}

// Streams is h's named streams.
//
// ponytail: answers none once admitted and decided: the store declares no
// Files.Streams and nothing creates a named stream yet. List them through
// Files.Streams, each joined with the overlay, once the store declares it.
func (fs *Filesystem) Streams(ctx context.Context, c vfs.Call, h vfs.Handle) ([]vfs.File, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	a, release, err := fs.admitHandle(ctx, c, h)
	if err != nil || a == nil {
		return nil, fs.pseudoOnly(ctx, h, err)
	}
	defer release()
	return nil, storeErr(fs.authz.Decide(ctx, a.caller, vfs.OpStreams, h, nil, nil, entity.ReadAttributes))
}

// Access is the part of want the caller is granted on h, delete through
// parent included; parent is nil when unknown. It refuses nothing: a right
// denied is left out. A pseudo directory grants reading and traversing it.
func (fs *Filesystem) Access(ctx context.Context, c vfs.Call, h, parent vfs.Handle, want vfs.Access) (vfs.Access, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return 0, err
	}
	defer done()
	a, release, err := fs.admitHandle(ctx, c, h)
	if err != nil {
		return 0, err
	}
	defer release()
	if a == nil {
		if _, err := fs.pseudoNode(ctx, h); err != nil {
			return 0, err
		}
		return want & pseudoRights, nil
	}
	got, err := fs.authz.Rights(ctx, a.caller, h, parent, want)
	return got & want, storeErr(err)
}

// change is one change to t's file other than SetAttr's: held as hold holds
// it, want decided for op, what admitted by open state, then do, answered
// with the file's WCC.
func (fs *Filesystem) change(ctx context.Context, c vfs.Call, t vfs.Target, op vfs.Op, want vfs.Access, what openstate.ChangeMask, do func(*admitted, vfs.File) error) (vfs.WCC, error) {
	ctx, done, err := fs.enter(ctx)
	if err != nil {
		return vfs.WCC{}, err
	}
	defer done()
	a, f, release, err := fs.hold(ctx, c, t)
	if err != nil {
		return vfs.WCC{}, err
	}
	defer release()
	if err := fs.authz.Decide(ctx, a.caller, op, t.H, nil, nil, want); err != nil {
		return vfs.WCC{}, storeErr(err)
	}
	if err := fs.checks.CheckChange(ctx, a.who(), keyOf(f), t.Open, what); err != nil {
		return vfs.WCC{}, openErr(err)
	}
	if err := do(a, f); err != nil {
		return vfs.WCC{}, err
	}
	return fs.wcc(ctx, c, a, t.H, f), nil
}

// hold admits a change to t's file, takes the file's admission and accept
// locks, and reads its attributes under them: what a WCC's Before is. Its
// release drops both locks and uncounts the call.
func (fs *Filesystem) hold(ctx context.Context, c vfs.Call, t vfs.Target) (*admitted, vfs.File, func(), error) {
	if err := onSession(c, t.Open); err != nil {
		return nil, vfs.File{}, nil, err
	}
	a, release, err := fs.admitChange(ctx, c, t.H)
	if err != nil {
		release()
		return nil, vfs.File{}, nil, err
	}
	k := lockorder.Key(fileOf(t.H))
	unlock, err := fs.locks.Take(ctx, lockorder.Set{Admit: []lockorder.Key{k}, Accept: []lockorder.Key{k}})
	if err != nil {
		release()
		return nil, vfs.File{}, nil, errContended
	}
	f, err := fs.file(ctx, a, t.H)
	if err != nil {
		unlock()
		release()
		return nil, vfs.File{}, nil, err
	}
	return a, f, func() { unlock(); release() }, nil
}

// wcc is h's WCC once a change has committed, from before, read in the same
// hold: no Before for a directory over NFSv3, which never reports one.
//
// decision: an After the store fails to read is left zero and the change
// answered as done, since it has committed. Overturned if a reply is found
// that must refuse rather than omit its attributes.
func (fs *Filesystem) wcc(ctx context.Context, c vfs.Call, a *admitted, h vfs.Handle, before vfs.File) vfs.WCC {
	w := vfs.WCC{}
	if !before.IsDir() || c.Protocol != entity.ProtocolNFSv3 {
		w.Before = &before
	}
	w.After, _ = fs.file(ctx, a, h)
	return w
}

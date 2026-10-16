package kvstore

import (
	"context"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
)

// The rights a read-only share grant withholds.
const writeRights = entity.WriteData | entity.AppendData | entity.WriteNamedAttrs | entity.DeleteChild |
	entity.WriteAttributes | entity.Delete | entity.WriteACL | entity.WriteOwner

// The rights the mode decides, by the bit that grants them; the rest are
// always granted or the owner's. A write bit grants write attributes to
// whoever it applies to, as the mapped GENERIC_WRITE set needs at open; what
// a non-owner may then set through it is mayChange's.
const (
	readRights  = entity.ReadData | entity.ReadNamedAttrs
	writeBits   = entity.WriteData | entity.AppendData | entity.WriteNamedAttrs | entity.DeleteChild | entity.WriteAttributes
	alwaysRight = entity.ReadAttributes | entity.ReadACL | entity.Synchronize
	ownerRights = entity.WriteAttributes | entity.WriteACL | entity.WriteOwner
	// An owner keeps the right to change attributes and the ACL whatever an
	// ACL says, so no ACL locks its owner out of repairing it.
	ownerImplicit = entity.WriteAttributes | entity.WriteACL
)

// Authorize answers whether the caller may do want to the file: the share
// grant on every call, then the immutable and append-only flags, then the
// file's ACL, or its mode when it has none.
func (f Files) Authorize(ctx context.Context, id entity.Identity, h entity.Handle, want entity.AccessMask) error {
	return f.s.kv.View(ctx, func(r kv.Reader) error {
		share, _, err := h.Parse()
		if err != nil {
			return err
		}
		_, file, err := resolveIn(r, share, h)
		if err != nil {
			return err
		}
		return authorize(r, id, file, want)
	})
}

// authorize is the one check every operation makes against the file it acts
// on.
func authorize(r kv.Reader, id entity.Identity, f entity.File, want entity.AccessMask) error {
	if err := grantAllows(r, id, f.Share, want); err != nil {
		return err
	}
	if err := flagsAllow(f, want); err != nil {
		return err
	}
	ok, err := allows(r, id, f, want)
	if err != nil {
		return err
	}
	if !ok {
		return merrs.NewAccessDeniedError("the file's mode or ACL denies the access asked")
	}
	return nil
}

// grantAllows is the share grant's part of a check, which privilege never
// skips: no grant refuses everything, and a read-only one every write right.
func grantAllows(r kv.Reader, id entity.Identity, share entity.ShareID, want entity.AccessMask) error {
	access, err := grantFor(r, share, id)
	if err != nil {
		return err
	}
	switch {
	case access == entity.AccessNone:
		return merrs.NewAccessDeniedError("no grant on the share")
	case access == entity.AccessRead && want&writeRights != 0:
		return merrs.NewAccessDeniedError("the share grant is read-only")
	}
	return nil
}

// flagsAllow enforces immutable and append-only for every caller, privileged
// or not. An append-only file takes appends and no overwrite; an append-only
// directory takes new entries and loses none.
func flagsAllow(f entity.File, want entity.AccessMask) error {
	denied := entity.AccessMask(0)
	switch {
	case f.IsImmutable():
		denied = writeRights
	case f.IsAppendOnly():
		denied = entity.Delete | entity.DeleteChild | entity.WriteAttributes | entity.WriteACL |
			entity.WriteOwner | entity.WriteNamedAttrs
		if !f.IsDir() {
			denied |= entity.WriteData
		}
	}
	if want&denied != 0 {
		return merrs.NewPermissionDeniedError("immutable or append-only " + f.ID.String())
	}
	return nil
}

// allows is the file's own answer, after the grant and the flags: whether
// its ACL, or its mode when no ACL is stored, gives the caller every right in
// want.
//
// ponytail: reads the ACL key on each check that the always-granted rights
// do not answer, so a lookup costs one read more than its entry and file.
// Carry a has-ACL bit in the file record if path walks show it.
func allows(r kv.Reader, id entity.Identity, f entity.File, want entity.AccessMask) (bool, error) {
	if id.Privileged {
		// Privilege skips the walk but runs nothing that no x bit makes
		// executable, as root's execute does.
		return want&entity.Execute == 0 || f.IsDir() || f.Mode&0o111 != 0, nil
	}
	want &^= alwaysRight
	if want == 0 {
		return true, nil
	}
	// The owner's implicit rights hold unless the ACL names owner rights,
	// so they are taken away only once it is read.
	owner := id.Principal == f.Owner
	b, ok, err := get(r, aclKey(f.Share, f.ID))
	if err != nil {
		return false, err
	}
	if !ok {
		if owner {
			want &^= ownerImplicit
		}
		return want&^modeAllows(id, f) == 0, nil
	}
	acl, err := decodeACL(b)
	if err != nil {
		return false, err
	}
	if owner && !acl.LimitsOwner() {
		want &^= ownerImplicit
	}
	if want == 0 {
		return true, nil
	}
	return aclAllows(id, f, acl, want), nil
}

// aclGrants reports whether the file's stored ACL gives the caller every
// right in want; a file with no stored ACL grants nothing this way.
func aclGrants(r kv.Reader, id entity.Identity, f entity.File, want entity.AccessMask) (bool, error) {
	b, ok, err := get(r, aclKey(f.Share, f.ID))
	if err != nil || !ok {
		return false, err
	}
	acl, err := decodeACL(b)
	if err != nil {
		return false, err
	}
	return aclAllows(id, f, acl, want), nil
}

// aclAllows walks the ACL's effective entries in order: each right is
// decided by the first entry that names it for the caller, and a right no
// entry grants is denied.
//
// decision: the evaluation is RFC 8881 §6.2.1's ordered allow and deny walk.
// The RFC that will own ACL semantics is not written; replace this with its
// rule once it is.
func aclAllows(id entity.Identity, f entity.File, acl entity.ACL, want entity.AccessMask) bool {
	undecided := want
	for _, e := range acl.Entries {
		if !e.Effective() || !matches(id, f, e.Who) {
			continue
		}
		hit := e.Mask & undecided
		if hit == 0 {
			continue
		}
		if e.Type == entity.ACEDeny {
			return false
		}
		if undecided &^= hit; undecided == 0 {
			return true
		}
	}
	return false
}

// matches reports whether an entry naming who applies to the caller. The
// well-known principals are matched by the identity's fields, never by
// privilege: a privileged caller skips the walk. SYSTEM, CREATOR OWNER and
// CREATOR GROUP match no caller, nor does a special principal with no number
// above, whose allow grants nothing and whose deny is refused when stored.
func matches(id entity.Identity, f entity.File, who entity.Principal) bool {
	switch who {
	case entity.Owner, entity.OwnerRights:
		return id.Principal == f.Owner
	case entity.OwnerGroup:
		return id.InGroup(f.Group)
	case entity.Everyone, entity.Network:
		return true
	case entity.AuthenticatedUsers, entity.BuiltinUsers:
		return id.Authenticated
	case entity.Administrators:
		return id.Admin
	}
	switch who.Kind {
	case entity.PrincipalGroup:
		return id.InGroup(who)
	case entity.PrincipalSpecial:
		return false
	}
	return id.Principal == who
}

// modeAllows is what the file's mode grants this caller: the owner's bits to
// its owner, the group's to a member of its group, the others' to the rest.
func modeAllows(id entity.Identity, f entity.File) entity.AccessMask {
	shift := uint(0)
	switch {
	case id.Principal == f.Owner:
		shift = 6
	case id.InGroup(f.Group):
		shift = 3
	}
	b := f.Mode >> shift
	m := alwaysRight
	if b&4 != 0 {
		m |= readRights
	}
	if b&2 != 0 {
		m |= writeBits
	}
	if b&1 != 0 {
		m |= entity.Execute
	}
	if id.Principal == f.Owner {
		m |= ownerRights
	}
	return m
}

// mayRemove decides whether the caller may remove the entry naming child
// from d, a decision about two files: delete on the file or delete-a-child
// on the directory, as RFC 8881 has it. In a sticky directory governed by its
// mode, only the file's owner, the directory's owner or a privileged caller
// may. Neither an immutable or append-only file nor one in such a directory
// loses a name, whoever asks. Like every change to a directory's entries,
// it needs search on the directory too, as POSIX asks both w and x.
//
// decision: search is required of an SMB caller as well, though Windows
// grants every user the privilege to bypass traverse checking. A share's
// default ACLs grant traverse wherever they grant a change, so the rule
// costs nothing there; revisit if an SMB client is refused a change it
// could make on Windows.
func mayRemove(r kv.Reader, id entity.Identity, d, child entity.File) error {
	access, err := grantFor(r, d.Share, id)
	if err != nil {
		return err
	}
	if access < entity.AccessReadWrite {
		return merrs.NewAccessDeniedError("the share grant does not allow removal")
	}
	for _, f := range []entity.File{d, child} {
		if f.IsImmutable() || f.IsAppendOnly() {
			return merrs.NewPermissionDeniedError("immutable or append-only " + f.ID.String())
		}
	}
	if ok, err := allows(r, id, d, entity.Execute); err != nil {
		return err
	} else if !ok {
		return merrs.NewAccessDeniedError("no search permission on the directory")
	}
	onDir, err := allows(r, id, d, entity.DeleteChild)
	if err != nil {
		return err
	}
	onFile := false
	if !onDir {
		if onFile, err = allows(r, id, child, entity.Delete); err != nil {
			return err
		}
	}
	if !onDir && !onFile {
		return merrs.NewAccessDeniedError("neither delete on the file nor delete-a-child on the directory")
	}
	if d.Mode&entity.ModeSticky != 0 && !id.Privileged && id.Principal != child.Owner && id.Principal != d.Owner {
		if _, stored, err := get(r, aclKey(d.Share, d.ID)); err != nil {
			return err
		} else if !stored {
			return merrs.NewAccessDeniedError("sticky directory: only an owner removes")
		}
	}
	return nil
}

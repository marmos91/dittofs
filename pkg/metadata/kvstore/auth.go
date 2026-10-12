package kvstore

import (
	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
)

// The rights a read-only share grant withholds.
const writeRights = entity.WriteData | entity.AppendData | entity.WriteNamedAttrs | entity.DeleteChild |
	entity.WriteAttributes | entity.Delete | entity.WriteACL | entity.WriteOwner

// The rights the mode decides, by the bit that grants them; the rest are
// always granted or the owner's.
const (
	readRights  = entity.ReadData | entity.ReadNamedAttrs
	writeBits   = entity.WriteData | entity.AppendData | entity.WriteNamedAttrs | entity.DeleteChild
	alwaysRight = entity.ReadAttributes | entity.ReadACL | entity.Synchronize
	ownerRights = entity.WriteAttributes | entity.WriteACL | entity.WriteOwner
)

// authorize is the one check every operation makes against the file it acts
// on: the share grant first, on every call, then the file's mode.
func authorize(r kv.Reader, id entity.Identity, f entity.File, want entity.AccessMask) error {
	access, err := grantFor(r, f.Share, id)
	if err != nil {
		return err
	}
	switch {
	case access == entity.AccessNone:
		return merrs.NewAccessDeniedError("no grant on the share")
	case access == entity.AccessRead && want&writeRights != 0:
		return merrs.NewAccessDeniedError("the share grant is read-only")
	}
	if !allows(id, f, want) {
		return merrs.NewAccessDeniedError("mode denies the access asked")
	}
	return nil
}

// allows is the file's own answer, after the share grant: whether its mode
// gives the caller every right in want.
func allows(id entity.Identity, f entity.File, want entity.AccessMask) bool {
	return id.Privileged || want&^modeAllows(id, f) == 0
}

// mayRemove decides whether the caller may remove the entry naming child
// from d, a decision about two files: delete on the file or delete-a-child
// on the directory, as RFC 8881 has it. In a sticky directory only the
// file's owner, the directory's owner or a privileged caller may.
func mayRemove(r kv.Reader, id entity.Identity, d, child entity.File) error {
	access, err := grantFor(r, d.Share, id)
	if err != nil {
		return err
	}
	if access < entity.AccessReadWrite {
		return merrs.NewAccessDeniedError("the share grant does not allow removal")
	}
	if !allows(id, d, entity.DeleteChild) && !allows(id, child, entity.Delete) {
		return merrs.NewAccessDeniedError("neither delete on the file nor delete-a-child on the directory")
	}
	if d.Mode&entity.ModeSticky != 0 && !id.Privileged && id.Principal != child.Owner && id.Principal != d.Owner {
		return merrs.NewAccessDeniedError("sticky directory: only an owner removes")
	}
	return nil
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

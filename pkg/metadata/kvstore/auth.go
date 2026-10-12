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
	if id.Privileged {
		return nil
	}
	if missing := want &^ modeAllows(id, f); missing != 0 {
		return merrs.NewAccessDeniedError("mode denies the access asked")
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

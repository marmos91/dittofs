// Package conflict is the access × deny matrix: whether two opens' modes can
// be held on one file together. It is a pure leaf of open state.
package conflict

import "github.com/marmos91/dittofs/pkg/metadata/entity"

// Class is what a deny mode is judged on: read, write and delete.
type Class uint8

const (
	Read Class = 1 << iota
	Write
	Delete
)

// Of is the classes an access mask touches: reading data or executing is
// read, writing or appending data is write, and delete is delete. No other
// right, attributes, ACLs and synchronize among them, is in any class, so an
// open asking only for those is refused by no deny mode.
func Of(a entity.AccessMask) Class {
	var c Class
	if a&(entity.ReadData|entity.Execute) != 0 {
		c |= Read
	}
	if a&(entity.WriteData|entity.AppendData) != 0 {
		c |= Write
	}
	if a&entity.Delete != 0 {
		c |= Delete
	}
	return c
}

// Shares reports whether an open asking access and denying deny can be held
// beside one holding heldAccess and denying heldDeny. The check is two-way:
// neither's access may be in the other's deny. An anonymous read or write is
// judged as an open asking for it and denying nothing.
func Shares(access, deny, heldAccess, heldDeny entity.AccessMask) bool {
	return Of(access)&Of(heldDeny) == 0 && Of(heldAccess)&Of(deny) == 0
}

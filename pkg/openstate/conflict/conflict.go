// Package conflict is the access × deny matrix, whether two opens' modes can
// be held on one file together, and what a caching grant keeps beside another
// holder's open. It is a pure leaf of open state.
package conflict

import (
	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/openstate/grantfsm"
)

// Class is what a deny mode is judged on: read, write and delete.
type Class uint8

const (
	Read Class = 1 << iota
	Write
	Delete
)

// Of is the classes an access mask touches: reading data or executing is
// read, writing or appending data is write, and delete is delete. No other
// right, attributes, ACLs and synchronize among them, is in any class.
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
// neither's access may be in the other's deny. An open whose access is in
// no class is outside it both ways, as MS-FSA's sharing check runs only
// between opens asking data access: it is refused by no deny mode, and its
// own deny refuses nothing. An anonymous read or write is judged as an open
// asking for it and denying nothing.
func Shares(access, deny, heldAccess, heldDeny entity.AccessMask) bool {
	a, h := Of(access), Of(heldAccess)
	if a == 0 || h == 0 {
		return true
	}
	return a&Of(heldDeny) == 0 && h&Of(deny) == 0
}

// GrantKeeps is the caching a grant keeps beside another holder's open asking
// access and denying deny: never write caching, whatever the open asks; read
// caching only while the open neither writes, deletes nor denies read; and
// handle caching, which only a deny-mode conflict over an open the grant
// covers breaks.
func GrantKeeps(access, deny entity.AccessMask) grantfsm.Kind {
	k := grantfsm.Read | grantfsm.Handle
	if Of(access)&(Write|Delete) != 0 || Of(deny)&Read != 0 {
		k &^= grantfsm.Read
	}
	return k
}

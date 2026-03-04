// Package rpc implements DCE/RPC protocol for SMB named pipes.
//
// This file implements the StoreIdentityResolver that bridges the control
// plane store (users/groups) to the LSARPC SID-to-name resolution.
package rpc

// StoreIdentityResolver resolves UIDs/GIDs to real usernames/group names
// from the control plane database. Implements IdentityResolver.
//
// The lookup functions are injected by the SMB adapter during SetRuntime()
// to avoid coupling the rpc package to control plane model types.
type StoreIdentityResolver struct {
	LookupUser  func(uid uint32) (string, bool)
	LookupGroup func(gid uint32) (string, bool)
}

// LookupUsernameByUID resolves a Unix UID to the corresponding username.
func (r *StoreIdentityResolver) LookupUsernameByUID(uid uint32) (string, bool) {
	if r.LookupUser == nil {
		return "", false
	}
	return r.LookupUser(uid)
}

// LookupGroupNameByGID resolves a Unix GID to the corresponding group name.
func (r *StoreIdentityResolver) LookupGroupNameByGID(gid uint32) (string, bool) {
	if r.LookupGroup == nil {
		return "", false
	}
	return r.LookupGroup(gid)
}

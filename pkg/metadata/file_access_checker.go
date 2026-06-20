package metadata

import "github.com/marmos91/dittofs/pkg/metadata/acl"

// FileAccessChecker is the single protocol-agnostic entry point every protocol
// adapter (NFSv3 / NFSv4.0 / NFSv4.1 / SMB2 / SMB3) funnels permission
// decisions through. Consolidating the checks behind one interface keeps the
// "handlers do protocol only" invariant: adapters translate their wire access
// bits to and from the canonical vocabularies below and never evaluate ACLs,
// DENY ACEs, or SID-based grants themselves.
//
// Two request vocabularies are supported because the two protocol families
// arrive with different access-bit models, but both resolve to the same
// underlying acl.Evaluate / acl.EvaluateGranted core over one EvaluateContext
// (UID/GID/GIDs + SID/GroupSIDs):
//
//   - CheckPermissions takes the generic metadata.Permission flag set. NFSv3 /
//     NFSv4 ACCESS and NFSv4 OPEN translate their RFC 1813 / RFC 7530 ACCESS
//     bits into Permission flags and call this.
//   - CheckFileAccess / CheckFileAccessWithParent take a raw MS-DTYP access
//     mask. SMB CREATE evaluates DesiredAccess against the file's DACL through
//     these and freezes the result onto the handle's GrantedAccess.
//
// CheckAttrReadAccess is the attr-based read probe used by SMB access-based
// enumeration, where only a directory entry's FileAttr (not a full File with a
// handle) is in scope. It centralizes the ACL+POSIX read evaluation the SMB
// query_directory handler previously inlined via a direct acl.Evaluate call.
//
// *Service is the production implementation; the interface exists so the
// permission core has one named contract and so the cross-protocol conformance
// matrix can assert every protocol's translation lands on identical decisions.
type FileAccessChecker interface {
	// CheckPermissions evaluates a generic-flag request against a file handle
	// and returns the granted subset (NFS path).
	CheckPermissions(ctx *AuthContext, handle FileHandle, requested Permission) (Permission, error)

	// CheckFileAccess evaluates an MS-DTYP DesiredAccess mask against a file's
	// stored DACL and returns the granted mask (SMB CREATE path). Returns
	// ErrAccessDenied as a *StoreError when a non-MAXIMUM_ALLOWED bit is denied.
	CheckFileAccess(file *File, authCtx *AuthContext, desiredAccess uint32) (uint32, error)

	// CheckFileAccessWithParent extends CheckFileAccess with the MS-FSA
	// delete-via-parent override (FILE_DELETE_CHILD on the parent grants DELETE
	// on the child even when the child's own DACL denies it).
	CheckFileAccessWithParent(file *File, parent *File, authCtx *AuthContext, desiredAccess uint32) (uint32, error)

	// CheckAttrReadAccess reports whether the requester may read the entry
	// described by attr. Used by SMB access-based enumeration. requestedMask is
	// the set of MS-DTYP / NFSv4 read rights the caller requires (these bit
	// positions are shared between the two models per RFC 7530 §6.2.1).
	CheckAttrReadAccess(attr *FileAttr, authCtx *AuthContext, requestedMask uint32) bool
}

// Static assertion that *Service satisfies the consolidated checker contract.
var _ FileAccessChecker = (*Service)(nil)

// CheckAttrReadAccess reports whether authCtx may read the entry described by
// attr, evaluating its ACL when present and falling back to POSIX mode bits
// otherwise. It is the centralized form of the access-based-enumeration read
// probe SMB query_directory previously implemented with an inline acl.Evaluate
// call, so the ABE visibility decision now shares the exact EvaluateContext
// shape used by CheckFileAccess / CheckPermissions.
//
// requestedMask carries the MS-DTYP / NFSv4 read rights the caller requires
// (e.g. READ_DATA | READ_NAMED_ATTRS | READ_ATTRIBUTES | READ_ACL for ABE).
// The ACE bit positions are shared between the Windows and NFSv4 models per
// RFC 7530 §6.2.1, so a single mask drives both files and directories.
func (s *Service) CheckAttrReadAccess(attr *FileAttr, authCtx *AuthContext, requestedMask uint32) bool {
	if attr == nil {
		// No attributes resolved: fail closed. The caller (ABE) must not leak
		// an entry it cannot prove the requester may read.
		return false
	}

	// Root bypass mirrors the rest of the metadata layer: UID 0 reads
	// everything regardless of per-file DACL / mode.
	if authCtx != nil && authCtx.Identity != nil && authCtx.Identity.UID != nil && *authCtx.Identity.UID == 0 {
		return true
	}

	if attr.ACL != nil {
		evalCtx := buildAttrEvalContext(attr, authCtx)
		return acl.Evaluate(attr.ACL, evalCtx, requestedMask)
	}

	// POSIX fallback — same read-bit selection as
	// auth_permissions.go::calculatePermissions for read.
	return attrPosixCanRead(attr, authCtx)
}

// buildAttrEvalContext constructs an acl.EvaluateContext from a FileAttr +
// AuthContext pair. It mirrors buildFileAccessEvalContext but takes the bare
// FileAttr (no enclosing File / handle), which is all an access-based
// enumeration decision has in scope.
func buildAttrEvalContext(attr *FileAttr, authCtx *AuthContext) *acl.EvaluateContext {
	if authCtx == nil || authCtx.Identity == nil || authCtx.Identity.UID == nil {
		// Anonymous: force FileOwnerUID to the AnonymousFileOwnerUID sentinel so
		// the requester's zero-valued UID cannot collapse onto a root-owned
		// entry's owner and pick up OWNER@ ACEs plus the MS-DTYP §2.5.3.2
		// owner-implicit grant. See #540.
		return &acl.EvaluateContext{
			FileOwnerUID: acl.AnonymousFileOwnerUID,
			FileOwnerGID: attr.GID,
		}
	}

	identity := authCtx.Identity
	evalCtx := &acl.EvaluateContext{
		UID:          *identity.UID,
		GIDs:         identity.GIDs,
		FileOwnerUID: attr.UID,
		FileOwnerGID: attr.GID,
	}
	if identity.GID != nil {
		evalCtx.GID = *identity.GID
	}

	switch {
	case identity.Username != "" && identity.Domain != "":
		evalCtx.Who = identity.Username + "@" + identity.Domain
	case identity.Username != "":
		evalCtx.Who = identity.Username
	}

	if identity.SID != nil {
		evalCtx.SID = *identity.SID
	}
	evalCtx.GroupSIDs = identity.GroupSIDs
	// MS-DTYP §2.5.3.2: owner-implicit WRITE_OWNER requires
	// SeTakeOwnershipPrivilege (admins only). See acl.Evaluate.
	evalCtx.RequesterHasTakeOwnership = acl.HasTakeOwnershipPrivilege(evalCtx.SID, evalCtx.GroupSIDs)

	return evalCtx
}

// attrPosixCanRead implements the read-bit selection used when an entry has no
// ACL: owner/group/other read bit based on the requester's effective identity.
func attrPosixCanRead(attr *FileAttr, authCtx *AuthContext) bool {
	if authCtx == nil || authCtx.Identity == nil || authCtx.Identity.UID == nil {
		// Anonymous → only the "other" read bit applies.
		return attr.Mode&0o004 != 0
	}
	uid := *authCtx.Identity.UID
	switch {
	case uid == attr.UID:
		return attr.Mode&0o400 != 0
	case authCtx.Identity.HasGID(attr.GID):
		return attr.Mode&0o040 != 0
	default:
		return attr.Mode&0o004 != 0
	}
}

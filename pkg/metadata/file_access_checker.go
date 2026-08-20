package metadata

import "github.com/marmos91/dittofs/pkg/metadata/acl"

// Service's permission-check methods are the single protocol-agnostic entry
// point every protocol adapter (NFSv3 / NFSv4.0 / NFSv4.1 / SMB2 / SMB3)
// funnels permission decisions through. Consolidating the checks in one place keeps the "handlers do
// protocol only" invariant: adapters translate their wire access bits to and
// from the canonical vocabularies below and never evaluate ACLs, DENY ACEs, or
// SID-based grants themselves.
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
// AuthContext pair, taking the file owner from the attr rather than an
// enclosing File row: a bare FileAttr is all an access-based enumeration
// decision has in scope. The construction itself is buildEvalContext's, so the
// enumeration path cannot drift from the NFS and SMB permission paths.
func buildAttrEvalContext(attr *FileAttr, authCtx *AuthContext) *acl.EvaluateContext {
	var identity *Identity
	if authCtx != nil {
		identity = authCtx.Identity
	}
	return buildEvalContext(attr.UID, attr.GID, identity)
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

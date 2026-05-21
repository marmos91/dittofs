package metadata

import (
	"context"
	"errors"

	"github.com/marmos91/dittofs/pkg/metadata/acl"
)

// AccessDecision contains the result of a share-level access control check.
//
// This is returned by CheckShareAccess to inform the protocol handler whether
// access is allowed and what restrictions apply.
type AccessDecision struct {
	// Allowed indicates whether access is granted
	Allowed bool

	// Reason provides a human-readable explanation for denial
	// Examples: "Client IP not in allowed list", "Authentication required"
	// Empty when Allowed is true
	Reason string

	// AllowedAuthMethods lists authentication methods the client may use
	// Only populated when access is allowed or when suggesting alternatives
	AllowedAuthMethods []string

	// ReadOnly indicates whether the client has read-only access
	// When true, all write operations should be denied
	ReadOnly bool
}

// CheckShareAccess verifies if a client can access a share and returns effective credentials.
//
// This implements share-level access control including:
//   - Authentication method validation
//   - IP-based access control (allowed/denied clients)
//   - Identity mapping (squashing, anonymous access)
func (s *MetadataService) CheckShareAccess(ctx context.Context, shareName, clientAddr, authMethod string, identity *Identity) (*AccessDecision, *AuthContext, error) {
	store, err := s.GetStoreForShare(shareName)
	if err != nil {
		return nil, nil, err
	}

	// Check context cancellation
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	// Get share options using CRUD operation
	opts, err := store.GetShareOptions(ctx, shareName)
	if err != nil {
		return nil, nil, err
	}

	// Step 1: Check authentication requirements
	if opts.RequireAuth && authMethod == "anonymous" {
		return &AccessDecision{
			Allowed: false,
			Reason:  "authentication required but anonymous access attempted",
		}, nil, nil
	}

	// Step 2: Validate authentication method
	if len(opts.AllowedAuthMethods) > 0 {
		methodAllowed := false
		for _, allowed := range opts.AllowedAuthMethods {
			if authMethod == allowed {
				methodAllowed = true
				break
			}
		}
		if !methodAllowed {
			return &AccessDecision{
				Allowed:            false,
				Reason:             "authentication method '" + authMethod + "' not allowed",
				AllowedAuthMethods: opts.AllowedAuthMethods,
			}, nil, nil
		}
	}

	// Step 3: Check denied list first (deny takes precedence)
	for _, denied := range opts.DeniedClients {
		// Check context during iteration for large lists
		if len(opts.DeniedClients) > 10 {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
		}

		if MatchesIPPattern(clientAddr, denied) {
			return &AccessDecision{
				Allowed: false,
				Reason:  "client " + clientAddr + " is explicitly denied",
			}, nil, nil
		}
	}

	// Step 4: Check allowed list (if specified)
	if len(opts.AllowedClients) > 0 {
		allowed := false
		for _, allowedPattern := range opts.AllowedClients {
			// Check context during iteration for large lists
			if len(opts.AllowedClients) > 10 {
				if err := ctx.Err(); err != nil {
					return nil, nil, err
				}
			}

			if MatchesIPPattern(clientAddr, allowedPattern) {
				allowed = true
				break
			}
		}
		if !allowed {
			return &AccessDecision{
				Allowed: false,
				Reason:  "client " + clientAddr + " not in allowed list",
			}, nil, nil
		}
	}

	// Step 5: Apply identity mapping
	effectiveIdentity := identity
	if identity != nil && opts.IdentityMapping != nil {
		effectiveIdentity = ApplyIdentityMapping(identity, opts.IdentityMapping)
	}

	// Step 6: Build successful access decision
	decision := &AccessDecision{
		Allowed:            true,
		Reason:             "",
		AllowedAuthMethods: opts.AllowedAuthMethods,
		ReadOnly:           opts.ReadOnly,
	}

	authCtx := &AuthContext{
		Context:    ctx,
		AuthMethod: authMethod,
		Identity:   effectiveIdentity,
		ClientAddr: clientAddr,
	}

	return decision, authCtx, nil
}

// Permission represents filesystem permission flags.
//
// These are generic permission flags that map to different protocol-specific
// permission models. Protocol handlers translate between Permission and
// protocol-specific permission bits (e.g., NFS ACCESS bits, SMB access masks).
type Permission uint32

const (
	// PermissionRead allows reading file data or listing directory contents
	PermissionRead Permission = 1 << iota

	// PermissionWrite allows modifying file data or directory contents
	PermissionWrite

	// PermissionExecute allows executing files or traversing directories
	PermissionExecute

	// PermissionDelete allows deleting files or directories
	PermissionDelete

	// PermissionListDirectory allows listing directory entries (read for directories)
	PermissionListDirectory

	// PermissionTraverse allows searching/traversing directories (execute for directories)
	PermissionTraverse

	// PermissionChangePermissions allows changing file/directory permissions (chmod)
	PermissionChangePermissions

	// PermissionChangeOwnership allows changing file/directory ownership (chown)
	PermissionChangeOwnership
)

// CalculatePermissionsFromBits converts Unix permission bits (rwx) to Permission flags.
//
// Maps the 3-bit Unix permission pattern to the internal Permission type:
//   - Bit 2 (0x4): Read -> PermissionRead | PermissionListDirectory
//   - Bit 1 (0x2): Write -> PermissionWrite | PermissionDelete
//   - Bit 0 (0x1): Execute -> PermissionExecute | PermissionTraverse
func CalculatePermissionsFromBits(bits uint32) Permission {
	var granted Permission

	if bits&0x4 != 0 { // Read bit
		granted |= PermissionRead | PermissionListDirectory
	}
	if bits&0x2 != 0 { // Write bit
		granted |= PermissionWrite | PermissionDelete
	}
	if bits&0x1 != 0 { // Execute bit
		granted |= PermissionExecute | PermissionTraverse
	}

	return granted
}

// CheckOtherPermissions extracts "other" permission bits from mode and returns granted permissions.
//
// Used for anonymous users who only get world-readable/writable/executable
// permissions (the "other" bits in Unix mode).
func CheckOtherPermissions(mode uint32, requested Permission) Permission {
	// Other bits are bits 0-2 (0o007)
	otherBits := mode & 0x7
	granted := CalculatePermissionsFromBits(otherBits)
	return granted & requested
}

// checkFilePermissions performs Unix-style permission checking on a file.
//
// This implements the standard Unix permission model:
//   - Root (UID 0): Bypass all checks (all permissions granted), except on read-only shares
//   - Owner: Check owner permission bits (mode >> 6 & 0x7)
//   - Group member: Check group permission bits (mode >> 3 & 0x7)
//   - Other: Check other permission bits (mode & 0x7)
//   - Anonymous: Only world permissions
func (s *MetadataService) checkFilePermissions(ctx *AuthContext, handle FileHandle, requested Permission) (Permission, error) {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return 0, err
	}

	// Check context
	if err := ctx.Context.Err(); err != nil {
		return 0, err
	}

	// Get file data using CRUD method
	file, err := store.GetFile(ctx.Context, handle)
	if err != nil {
		return 0, err
	}

	// Get share options for read-only check
	shareOpts, err := store.GetShareOptions(ctx.Context, file.ShareName)
	if err != nil {
		// If we can't get share options, continue without read-only check
		shareOpts = nil
	}

	// Share-level write permission bypass:
	// If the user has share-level write permission (ctx.ShareWritable) and the
	// file's ACL does not contain an explicit DENY ACE, grant write-related
	// permissions, bypassing file-level Unix permission checks.
	//
	// Bypass remains in effect when:
	//   - the file has no ACL at all, or
	//   - the file's ACL is allow-only (no DENY ACE present).
	// Allow-only ACLs are additive: they only add grants on top of POSIX bits,
	// so the share-level write grant should still apply. Concretely this keeps
	// smbtorture stream-inherit-perms (which appends one ALLOW ACE to the
	// synthesized DACL) and create.multi (which works against allow-only
	// share-root SDs) passing.
	//
	// Bypass disabled only when an explicit DENY ACE is present: a DENY ACE
	// encodes intent that POSIX bits / share-level grants cannot express and
	// must take precedence (load-bearing for smbtorture acls.DENY1 and
	// delete-on-close-perms.*).
	//
	// Note: ShareReadOnly takes precedence - if the share is read-only for this
	// user, write permission is denied regardless of ShareWritable.
	if ctx.ShareWritable && !ctx.ShareReadOnly && !acl.HasExplicitDeny(file.ACL) {
		// Only grant write-related permissions via the share-level bypass.
		// Read permissions still go through normal calculatePermissions checks.
		writePerms := requested & (PermissionWrite | PermissionDelete)
		if writePerms != 0 {
			// For write requests, grant what was requested
			return writePerms, nil
		}
		// For non-write requests (read-only), fall through to normal permission check
	}

	return calculatePermissions(file, ctx.Identity, shareOpts, requested), nil
}

// calculatePermissions computes granted permissions based on file attributes and identity.
func calculatePermissions(
	file *File,
	identity *Identity,
	shareOpts *ShareOptions,
	requested Permission,
) Permission {
	attr := &file.FileAttr

	// ACL evaluation takes precedence when ACL is present
	if attr.ACL != nil {
		return evaluateACLPermissions(file, identity, shareOpts, requested)
	}

	// No ACL = classic Unix permission check

	// Handle anonymous/no identity case
	if identity == nil || identity.UID == nil {
		// Only grant "other" permissions for anonymous users
		return CheckOtherPermissions(attr.Mode, requested)
	}

	uid := *identity.UID
	gid := identity.GID

	// Root bypass: UID 0 gets all permissions EXCEPT on read-only shares
	if uid == 0 {
		if shareOpts != nil && shareOpts.ReadOnly {
			// Root gets all permissions except write on read-only shares
			return requested &^ (PermissionWrite | PermissionDelete)
		}
		// Root gets all permissions on normal shares
		return requested
	}

	// Determine which permission bits apply
	var permBits uint32

	if uid == attr.UID {
		// Owner permissions (bits 6-8)
		permBits = (attr.Mode >> 6) & 0x7
	} else if gid != nil && (*gid == attr.GID || identity.HasGID(attr.GID)) {
		// Group permissions (bits 3-5)
		permBits = (attr.Mode >> 3) & 0x7
	} else {
		// Other permissions (bits 0-2)
		permBits = attr.Mode & 0x7
	}

	// Map Unix permission bits to Permission flags
	granted := CalculatePermissionsFromBits(permBits)

	// Owner gets additional privileges
	if uid == attr.UID {
		granted |= PermissionChangePermissions | PermissionChangeOwnership
	}

	// Apply read-only share restriction for all non-root users
	if shareOpts != nil && shareOpts.ReadOnly {
		granted &= ^(PermissionWrite | PermissionDelete)
	}

	return granted & requested
}

// evaluateACLPermissions handles permission checking when a file has an ACL.
// It maps the internal Permission flags to NFSv4 ACE mask bits and evaluates
// the ACL for each requested permission type.
func evaluateACLPermissions(
	file *File,
	identity *Identity,
	shareOpts *ShareOptions,
	requested Permission,
) Permission {
	// Handle anonymous/no identity
	if identity == nil || identity.UID == nil {
		// Evaluate as EVERYONE@ only
		evalCtx := &acl.EvaluateContext{
			FileOwnerUID: file.UID,
			FileOwnerGID: file.GID,
		}
		return evaluateWithACL(file.ACL, evalCtx, requested, shareOpts)
	}

	uid := *identity.UID

	// Root bypass: UID 0 gets all permissions except write on read-only shares
	if uid == 0 {
		if shareOpts != nil && shareOpts.ReadOnly {
			return requested &^ (PermissionWrite | PermissionDelete)
		}
		return requested
	}

	// Build evaluation context
	evalCtx := &acl.EvaluateContext{
		UID:          uid,
		GIDs:         identity.GIDs,
		FileOwnerUID: file.UID,
		FileOwnerGID: file.GID,
	}
	if identity.GID != nil {
		evalCtx.GID = *identity.GID
	}

	// Set Who to "username@domain" if available for named principal matching
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

	return evaluateWithACL(file.ACL, evalCtx, requested, shareOpts)
}

// permToACLMask maps each Permission flag to its corresponding NFSv4 ACE mask bits.
// Declared at package level to avoid allocating a map on every call.
var permToACLMask = [...]struct {
	perm Permission
	mask uint32
}{
	{PermissionRead, acl.ACE4_READ_DATA},
	{PermissionWrite, acl.ACE4_WRITE_DATA | acl.ACE4_APPEND_DATA},
	{PermissionExecute, acl.ACE4_EXECUTE},
	{PermissionDelete, acl.ACE4_DELETE},
	{PermissionListDirectory, acl.ACE4_LIST_DIRECTORY},
	{PermissionTraverse, acl.ACE4_EXECUTE},
	{PermissionChangePermissions, acl.ACE4_WRITE_ACL},
	{PermissionChangeOwnership, acl.ACE4_WRITE_OWNER},
}

// evaluateWithACL maps Permission flags to ACL mask bits and evaluates the ACL.
// Each permission type is checked individually because ACL evaluation is per-operation.
func evaluateWithACL(fileACL *acl.ACL, evalCtx *acl.EvaluateContext, requested Permission, shareOpts *ShareOptions) Permission {
	var granted Permission

	for _, pm := range permToACLMask {
		if requested&pm.perm != 0 && acl.Evaluate(fileACL, evalCtx, pm.mask) {
			granted |= pm.perm
		}
	}

	// Apply read-only share restriction
	if shareOpts != nil && shareOpts.ReadOnly {
		granted &^= PermissionWrite | PermissionDelete
	}

	return granted & requested
}

// checkPermission checks a single permission flag on a file.
func (s *MetadataService) checkPermission(ctx *AuthContext, handle FileHandle, perm Permission, msg string) error {
	granted, err := s.checkFilePermissions(ctx, handle, perm)
	if err != nil {
		return err
	}
	if granted&perm == 0 {
		return &StoreError{
			Code:    ErrAccessDenied,
			Message: msg,
		}
	}
	return nil
}

// checkWritePermission checks write permission on a file.
func (s *MetadataService) checkWritePermission(ctx *AuthContext, handle FileHandle) error {
	return s.checkPermission(ctx, handle, PermissionWrite, "write permission denied")
}

// CheckParentWriteAccess verifies the caller may add or remove a child entry
// in the given directory. It is the public, protocol-facing entry point that
// SMB / NFS handlers call before attempting an entry-creating or
// entry-replacing CREATE so that ACL-based denial surfaces as ACCESS_DENIED
// rather than OBJECT_NAME_COLLISION / DELETE_PENDING.
//
// The check is exactly POSIX WRITE on the parent directory; ACL evaluation
// runs through the same code path as in-process write checks.
func (s *MetadataService) CheckParentWriteAccess(ctx *AuthContext, parentHandle FileHandle) error {
	return s.checkWritePermission(ctx, parentHandle)
}

// checkDeletePermission checks permission to unlink an entry from a parent directory.
//
// Two rules, in order:
//
//  1. If the protocol handler set ctx.HasDeleteAccess, DELETE access was
//     already authorized upstream (e.g. SMB CREATE with
//     FILE_DELETE_ON_CLOSE + desiredAccess=DELETE or SET_INFO
//     FileDispositionInformation, both of which verify the caller's grant
//     at open time). Per MS-FSA 2.1.5.4, DELETE_ON_CLOSE honors the handle's
//     frozen authorization regardless of the current identity — critical for
//     SMB reauth flows where the session's UID/GID may shift between open
//     and close for the same Kerberos principal (issue #388). Read-only
//     shares still block this path as defense in depth.
//  2. Otherwise, fall back to POSIX unlink(2): require WRITE on the parent
//     directory. Keeps NFS strict.
//
// Sticky-bit semantics are enforced separately by CheckStickyBitRestriction,
// which the caller must invoke after this check on the resolved file entry.
// The `file` parameter is currently unused but reserved for future rule
// extensions (e.g. explicit DELETE ACE evaluation) and kept for signature
// stability across call sites.
func (s *MetadataService) checkDeletePermission(ctx *AuthContext, parentHandle FileHandle, _ *File) error {
	// Rule 1: upstream DELETE-access grant.
	if ctx.HasDeleteAccess && !ctx.ShareReadOnly {
		return nil
	}

	// Rule 2: POSIX WRITE on parent.
	if err := s.checkWritePermission(ctx, parentHandle); err != nil {
		var storeErr *StoreError
		if errors.As(err, &storeErr) && storeErr.Code == ErrAccessDenied {
			return &StoreError{
				Code:    ErrAccessDenied,
				Message: "delete permission denied",
			}
		}
		return err
	}
	return nil
}

// checkReadPermission checks read permission on a file.
func (s *MetadataService) checkReadPermission(ctx *AuthContext, handle FileHandle) error {
	return s.checkPermission(ctx, handle, PermissionRead, "read permission denied")
}

// checkExecutePermission checks execute/traverse permission on a file.
func (s *MetadataService) checkExecutePermission(ctx *AuthContext, handle FileHandle) error {
	return s.checkPermission(ctx, handle, PermissionExecute, "execute permission denied")
}

// MS-DTYP access-right bits used by CheckFileAccess.
//
// Kept here (rather than importing the SMB types package) because permission
// enforcement is protocol-agnostic and importing protocol packages from
// metadata would invert the dependency. The numeric values are spec-fixed.
const (
	// accessMaskMaximumAllowed is MS-DTYP §2.4.3 MAXIMUM_ALLOWED.
	// When set on a CREATE DesiredAccess request, the server MUST NOT deny
	// the open — it computes and returns the bits the requester is actually
	// allowed and uses those as the granted access.
	accessMaskMaximumAllowed uint32 = 0x02000000

	// POSIX-fallback access masks, mirroring computeMaximalAccess in
	// internal/adapter/smb/v2/handlers/create.go. Kept consistent so DACL
	// nil → enforcement and MxAc reply agree. See PR #528 + issue #525 for
	// the rationale on POSIX fallback over Windows-default synthesis at
	// the enforcement layer.
	accessMaskPosixGenericAll uint32 = 0x001F01FF
	accessMaskPosixRead       uint32 = 0x00120089
	accessMaskPosixWrite      uint32 = 0x00120116
	accessMaskPosixExecute    uint32 = 0x001200A0

	// accessMaskMinAuthenticated is READ_CONTROL | SYNCHRONIZE — the floor
	// granted to any authenticated requester even when POSIX mode bits would
	// otherwise grant nothing. Mirrors computeMaximalAccess.
	accessMaskMinAuthenticated uint32 = 0x00100000 | 0x00020000
)

// CheckFileAccess validates a CREATE DesiredAccess request against an
// existing file's stored DACL (or POSIX mode bits when ACL == nil) per
// MS-SMB2 §3.3.5.9 and MS-FSA §2.1.5.1.2.1.
//
// Returns the granted access mask. Behavior:
//
//   - Root bypass (UID 0): returns desiredAccess as granted.
//   - file.ACL == nil: POSIX fallback (owner=ALL, group/other from mode bits),
//     identical to computeMaximalAccess in create.go so the MxAc reply and the
//     enforcement gate agree. AND'd with desiredAccess.
//   - file.ACL != nil: per-requested-bit acl.Evaluate; only the bits the DACL
//     explicitly allows are granted. EVERYONE@/OWNER@/GROUP@/SID-form ACEs
//     resolve via the same EvaluateContext shape as evaluateACLPermissions.
//   - MAXIMUM_ALLOWED (0x02000000) in desiredAccess: never deny. The returned
//     granted mask reflects what the DACL actually allows; the caller is
//     expected to use it as the handle's effective access rights
//     (MS-SMB2 §2.2.13.1 / §3.3.5.9 paragraph 8).
//
// Returns ErrAccessDenied as a *StoreError when MAXIMUM_ALLOWED is NOT set
// and any requested bit is denied. Granted is always populated (even on
// error) so callers can log what was/wasn't granted.
//
// GENERIC_*, OWNER_RIGHTS, and ACCESSBASED enumeration are intentionally
// out of scope here — those are tracked separately under #530/#531/#532.
func (s *MetadataService) CheckFileAccess(file *File, authCtx *AuthContext, desiredAccess uint32) (uint32, error) {
	maximumAllowed := desiredAccess&accessMaskMaximumAllowed != 0

	// Root bypass: identical semantics to computeMaximalAccess and
	// evaluateACLPermissions. UID 0 gets everything; MAXIMUM_ALLOWED resolves
	// to GENERIC_ALL.
	if authCtx != nil && authCtx.Identity != nil && authCtx.Identity.UID != nil && *authCtx.Identity.UID == 0 {
		if maximumAllowed {
			return accessMaskPosixGenericAll, nil
		}
		return desiredAccess, nil
	}

	var granted uint32
	if file.ACL != nil {
		evalCtx := buildFileAccessEvalContext(file, authCtx)
		// Per-bit probe: acl.Evaluate(mask, …) returns true only when ALL bits
		// in mask are allowed, so we must probe each requested bit
		// individually — anything else conflates per-bit DENY semantics.
		probe := desiredAccess &^ accessMaskMaximumAllowed
		for bit := uint32(1); bit != 0 && probe != 0; bit <<= 1 {
			if probe&bit == 0 {
				continue
			}
			if acl.Evaluate(file.ACL, evalCtx, bit) {
				granted |= bit
			}
			probe &^= bit
		}
	} else {
		granted = posixFallbackAccessMask(file, authCtx) & (desiredAccess &^ accessMaskMaximumAllowed)
	}

	if maximumAllowed {
		// MAXIMUM_ALLOWED never denies. Compute the full set of bits the
		// requester is allowed (independent of what they asked for, minus the
		// MAXIMUM_ALLOWED bit itself) and return that as the granted mask.
		// Mirrors computeMaximalAccess on the ACL path and the POSIX-fallback
		// path so the handle's effective access matches the MxAc reply.
		var effective uint32
		if file.ACL != nil {
			evalCtx := buildFileAccessEvalContext(file, authCtx)
			for _, bit := range maxAccessProbeBits {
				if acl.Evaluate(file.ACL, evalCtx, bit) {
					effective |= bit
				}
			}
		} else {
			effective = posixFallbackAccessMask(file, authCtx)
		}
		return effective | granted, nil
	}

	// Strict mode: any requested non-MAXIMUM_ALLOWED bit not granted = deny.
	requestedExplicit := desiredAccess &^ accessMaskMaximumAllowed
	if requestedExplicit != 0 && granted&requestedExplicit != requestedExplicit {
		return granted, &StoreError{
			Code:    ErrAccessDenied,
			Message: "desired access denied by file DACL",
		}
	}
	return granted, nil
}

// maxAccessProbeBits is the set of MS-DTYP access-right bits probed against a
// file's DACL when MAXIMUM_ALLOWED is requested. Mirrors create.go's identical
// constant — kept in sync so CheckFileAccess and computeMaximalAccess agree on
// the effective access reported for a handle.
var maxAccessProbeBits = [...]uint32{
	acl.ACE4_READ_DATA, // == ACE4_LIST_DIRECTORY
	acl.ACE4_WRITE_DATA,
	acl.ACE4_APPEND_DATA,
	acl.ACE4_READ_NAMED_ATTRS,
	acl.ACE4_WRITE_NAMED_ATTRS,
	acl.ACE4_EXECUTE,
	acl.ACE4_DELETE_CHILD,
	acl.ACE4_READ_ATTRIBUTES,
	acl.ACE4_WRITE_ATTRIBUTES,
	acl.ACE4_DELETE,
	acl.ACE4_READ_ACL,
	acl.ACE4_WRITE_ACL,
	acl.ACE4_WRITE_OWNER,
	acl.ACE4_SYNCHRONIZE,
}

// buildFileAccessEvalContext mirrors evaluateACLPermissions's EvaluateContext
// construction so per-bit ACL evaluation in CheckFileAccess produces the same
// allow/deny decisions a downstream read/write permission check would later
// produce against the same file. Kept private to the metadata package.
func buildFileAccessEvalContext(file *File, authCtx *AuthContext) *acl.EvaluateContext {
	if authCtx == nil || authCtx.Identity == nil || authCtx.Identity.UID == nil {
		return &acl.EvaluateContext{
			FileOwnerUID: file.UID,
			FileOwnerGID: file.GID,
		}
	}

	identity := authCtx.Identity
	evalCtx := &acl.EvaluateContext{
		UID:          *identity.UID,
		GIDs:         identity.GIDs,
		FileOwnerUID: file.UID,
		FileOwnerGID: file.GID,
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

	return evalCtx
}

// posixFallbackAccessMask computes the granted MS-DTYP access mask for a file
// whose ACL is nil, using POSIX mode bits. Mirrors the POSIX branch of
// computeMaximalAccess in internal/adapter/smb/v2/handlers/create.go so the
// enforcement gate and the MxAc reply stay consistent.
//
// Owner gets GENERIC_ALL. Non-owners get a mask built from their applicable
// permission bits (group or other), with a floor of READ_CONTROL | SYNCHRONIZE
// for any authenticated user.
func posixFallbackAccessMask(file *File, authCtx *AuthContext) uint32 {
	if authCtx != nil && authCtx.Identity != nil && authCtx.Identity.UID != nil && *authCtx.Identity.UID == file.UID {
		return accessMaskPosixGenericAll
	}

	isGroupMember := false
	if authCtx != nil && authCtx.Identity != nil {
		if authCtx.Identity.GID != nil && *authCtx.Identity.GID == file.GID {
			isGroupMember = true
		}
		if !isGroupMember {
			for _, gid := range authCtx.Identity.GIDs {
				if gid == file.GID {
					isGroupMember = true
					break
				}
			}
		}
	}

	var permBits uint32
	if isGroupMember {
		permBits = (file.Mode >> 3) & 0x7
	} else {
		permBits = file.Mode & 0x7
	}

	var access uint32
	if permBits&0x4 != 0 {
		access |= accessMaskPosixRead
	}
	if permBits&0x2 != 0 {
		access |= accessMaskPosixWrite
	}
	if permBits&0x1 != 0 {
		access |= accessMaskPosixExecute
	}

	if access == 0 && authCtx != nil && authCtx.Identity != nil && authCtx.Identity.UID != nil {
		// Authenticated requester: floor at READ_CONTROL | SYNCHRONIZE.
		access = accessMaskMinAuthenticated
	}

	return access
}

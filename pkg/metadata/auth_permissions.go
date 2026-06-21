package metadata

import (
	"context"
	"errors"
	"net"

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
func (s *Service) CheckShareAccess(ctx context.Context, shareName, clientAddr, authMethod string, identity *Identity) (*AccessDecision, *AuthContext, error) {
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

	// Strip port from clientAddr so IP ACL patterns match correctly.
	// ClientAddr may arrive as "IP:port" from protocol handlers; pattern
	// entries are bare IPs or CIDR ranges that net.ParseIP / net.ParseCIDR
	// cannot match against a host:port string.
	if host, _, err := net.SplitHostPort(clientAddr); err == nil {
		clientAddr = host
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
func (s *Service) checkFilePermissions(ctx *AuthContext, handle FileHandle, requested Permission) (Permission, error) {
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

	// Handle-based SMB write authorization:
	// SMB is handle-based — a handle granted FILE_WRITE_DATA / FILE_APPEND_DATA
	// at open (by the DACL-honoring open-time gate Service.CheckFileAccess) must
	// be able to write regardless of the file's current POSIX mode or
	// DOS-READONLY attribute. The smbtorture smb2.durable-open.read-only test
	// CREATEs a FILE_ATTRIBUTE_READONLY file (granted full access at open under a
	// NULL DACL) and then WRITEs through that handle. When ctx.WriteAuthorizedByHandle
	// is set, the SMB op handler has confirmed the open carried write access
	// (derived from OpenFile.GrantedAccess), so the metadata layer must not
	// re-deny on file-level POSIX bits / DOS-READONLY.
	//
	// Defense-in-depth on explicit DENY: the open-time DACL gate already strips
	// FILE_WRITE_DATA when an explicit DENY-write ACE is present, so a write-
	// authorized handle can never be minted on such a file and this branch would
	// not be reached for it in production. We nonetheless keep the
	// !acl.HasExplicitDeny guard so that even a mis-set flag can never let a
	// caller past an explicit DENY ACE at the metadata layer — a DENY ACE encodes
	// intent POSIX bits cannot express and must always win (the same invariant
	// the former share-level floor pinned for smbtorture acls.DENY1 /
	// delete-on-close-perms.*).
	//
	// NFS leaves WriteAuthorizedByHandle false (it has no handle), so NFS writes
	// continue through the normal calculatePermissions path below.
	//
	// Note: ShareReadOnly takes precedence — if the share is read-only for this
	// user, write permission is denied regardless of this flag.
	if ctx.WriteAuthorizedByHandle && !ctx.ShareReadOnly && !acl.HasExplicitDeny(file.ACL) {
		// Only grant write-related permissions via the handle authorization.
		// Read permissions still go through normal calculatePermissions checks.
		writePerms := requested & (PermissionWrite | PermissionDelete)
		if writePerms != 0 {
			// For write requests, grant what was requested
			return writePerms, nil
		}
		// For non-write requests (read-only), fall through to normal permission check
	}

	granted := calculatePermissions(file, ctx.Identity, shareOpts, requested)

	// Per-user read-only ceiling. ctx.ShareReadOnly is the user's resolved share
	// permission and is independent of the store-level shareOpts.ReadOnly already
	// applied inside calculatePermissions: a user can be read-only on a share
	// whose stored options are read-write. Strip write+delete here so the ceiling
	// is enforced at the single metadata funnel both protocols traverse — NFS
	// write handlers have no compensating gate of their own, and the SMB
	// handle-based bypass above is unreachable when ShareReadOnly is set. #1276.
	if ctx.ShareReadOnly {
		granted &^= PermissionWrite | PermissionDelete
	}
	return granted, nil
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

	// DOS READONLY enforcement across protocols (NFS + SMB). modeDOSReadonly
	// (0x100000) is the persistent SMB-set READONLY bit (preserved by
	// modeMask in ApplyModeDefault). Gating on this bit alone is correct;
	// modeDOSExplicit is masked off by ApplyModeDefault so cannot be relied
	// upon at enforcement time. Without this, NFS clients bypass the
	// READONLY semantics that SMB clients see via the DACL path.
	if attr.Mode&0x100000 != 0 {
		permBits &^= 0x2
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
		// Anonymous: suppress OWNER@ matching and the MS-DTYP §2.5.3.2
		// owner-implicit RC|WRITE_DAC pass by forcing FileOwnerUID to
		// the AnonymousFileOwnerUID sentinel — without it the requester's
		// zero-valued UID would collapse onto a root-owned file's owner.
		// EVERYONE@ ACEs still match. GROUP@ and the "<n>@localdomain"
		// form may still match on UID/GID-0 owned files (residuals
		// tracked under #540).
		evalCtx := &acl.EvaluateContext{
			FileOwnerUID: acl.AnonymousFileOwnerUID,
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
	// MS-DTYP §2.5.3.2: owner-implicit WRITE_OWNER requires
	// SeTakeOwnershipPrivilege (admins only). See acl.Evaluate.
	evalCtx.RequesterHasTakeOwnership = acl.HasTakeOwnershipPrivilege(evalCtx.SID, evalCtx.GroupSIDs)

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
func (s *Service) checkPermission(ctx *AuthContext, handle FileHandle, perm Permission, msg string) error {
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
func (s *Service) checkWritePermission(ctx *AuthContext, handle FileHandle) error {
	return s.checkPermission(ctx, handle, PermissionWrite, "write permission denied")
}

// CheckParentWriteAccess verifies the caller may add or remove a child entry
// in the given directory. It is the public, protocol-facing entry point that
// SMB / NFS handlers call before attempting an entry-creating or
// entry-replacing CREATE so that ACL-based denial surfaces as ACCESS_DENIED
// rather than OBJECT_NAME_COLLISION / DELETE_PENDING.
//
// The check is exactly POSIX WRITE on the parent directory; ACL evaluation
// runs through the same code path as in-process write checks. Use
// CheckParentCreateAccess when the caller knows whether a file or
// subdirectory is being created so the precise ACL bit (ADD_FILE vs
// ADD_SUBDIRECTORY) can be evaluated.
func (s *Service) CheckParentWriteAccess(ctx *AuthContext, parentHandle FileHandle) error {
	return s.checkWritePermission(ctx, parentHandle)
}

// CheckParentCreateAccess verifies the caller may create a new file or
// subdirectory in the given directory. Unlike CheckParentWriteAccess, this
// evaluates the precise NFSv4 ACL bit corresponding to the kind of child
// being created:
//
//   - File create  -> ACE4_ADD_FILE         (0x02, alias of WRITE_DATA)
//   - Dir create   -> ACE4_ADD_SUBDIRECTORY (0x04, alias of APPEND_DATA)
//
// MS-FSA 2.1.5.1.1 and Samba's mkdir_internal()/open_file_ntcreate() check
// each bit independently. A parent DACL that denies SEC_DIR_ADD_FILE only
// (no ADD_SUBDIRECTORY deny) must still allow subdirectory creation —
// required by smbtorture smb2.create.mkdir-visible.
//
// For files that do not carry an ACL we fall back to the generic
// PermissionWrite path so POSIX mode bits, share-level overrides, and
// DOS-READONLY enforcement are honored.
func (s *Service) CheckParentCreateAccess(ctx *AuthContext, parentHandle FileHandle, isDirectory bool) error {
	store, err := s.storeForHandle(parentHandle)
	if err != nil {
		return err
	}
	if err := ctx.Context.Err(); err != nil {
		return err
	}

	file, err := store.GetFile(ctx.Context, parentHandle)
	if err != nil {
		return err
	}

	// Without an ACL there's nothing to refine — fall back to the generic
	// POSIX-write check so mode bits / share-level grants apply uniformly.
	if file.ACL == nil {
		return s.checkWritePermission(ctx, parentHandle)
	}

	shareOpts, _ := store.GetShareOptions(ctx.Context, file.ShareName)

	// Read-only beats any ACL grant — both the per-user share permission
	// (ctx.ShareReadOnly) and the store-level share option. Without the
	// ctx.ShareReadOnly arm a read-only user could create entries under an
	// ALLOW-granting parent DACL on an otherwise read-write share. #1276.
	if ctx.ShareReadOnly || (shareOpts != nil && shareOpts.ReadOnly) {
		return &StoreError{Code: ErrAccessDenied, Message: "write permission denied"}
	}

	// CREATE has no open handle yet, so the handle-based write authorization
	// used for SMB WRITE/SET_INFO (ctx.WriteAuthorizedByHandle) does not apply
	// here — the parent-create gate IS the open-time authorization boundary.
	// We therefore evaluate the parent's DACL precisely below: an ALLOW-only
	// DACL that grants the requesting principal ADD_FILE / ADD_SUBDIRECTORY
	// (directly, or via GENERIC_WRITE / GENERIC_ALL expansion in acl.Evaluate)
	// permits the create, keeping stream-inherit-perms and create.multi passing,
	// while a parent DACL that does not grant the add bit correctly denies.

	// Root bypass aligns with calculatePermissions/evaluateACLPermissions:
	// UID 0 gets all permissions except on read-only shares (handled above).
	if ctx.Identity != nil && ctx.Identity.UID != nil && *ctx.Identity.UID == 0 {
		return nil
	}

	// Reuse the canonical EvaluateContext builder so anonymous-owner
	// handling, Who/SID population, and SeTakeOwnership derivation stay in
	// one place with CheckFileAccess.
	evalCtx := buildFileAccessEvalContext(file, ctx)

	mask := uint32(acl.ACE4_ADD_FILE)
	if isDirectory {
		mask = acl.ACE4_ADD_SUBDIRECTORY
	}

	if !acl.Evaluate(file.ACL, evalCtx, mask) {
		return &StoreError{Code: ErrAccessDenied, Message: "write permission denied"}
	}
	return nil
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
func (s *Service) checkDeletePermission(ctx *AuthContext, parentHandle FileHandle, _ *File) error {
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
func (s *Service) checkReadPermission(ctx *AuthContext, handle FileHandle) error {
	return s.checkPermission(ctx, handle, PermissionRead, "read permission denied")
}

// checkExecutePermission checks execute/traverse permission on a file.
func (s *Service) checkExecutePermission(ctx *AuthContext, handle FileHandle) error {
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

	// accessMaskSystemSecurity is MS-DTYP §2.4.3 ACCESS_SYSTEM_SECURITY
	// (SEC_FLAG_SYSTEM_SECURITY). Reading or writing the SACL requires
	// SeSecurityPrivilege; without it the open MUST fail
	// STATUS_PRIVILEGE_NOT_HELD rather than STATUS_ACCESS_DENIED. Mirrors
	// Samba libcli/security/access_check.c::se_access_check_implicit_owner.
	accessMaskSystemSecurity uint32 = 0x01000000

	// accessMaskPosixGenericAll is the Windows GENERIC_ALL bundle used for
	// MAXIMUM_ALLOWED on the no-DACL path (root bypass + nil-ACL case). The
	// numeric value is the same one ComputeMaximalAccess emits for an owner
	// on the POSIX fallback.
	accessMaskPosixGenericAll uint32 = 0x001F01FF

	// POSIX-mode→access-mask bundles used by ComputeMaximalAccess when a file
	// carries no explicit DACL. Each maps a POSIX rwx bit to the Windows
	// access-right bundle a holder of that permission is granted (MS-SMB2
	// §2.2.13.2 MaximalAccess reply).
	accessMaskPosixRead    uint32 = 0x00120089 // FILE_READ_DATA | FILE_READ_EA | FILE_READ_ATTRIBUTES | READ_CONTROL | SYNCHRONIZE
	accessMaskPosixWrite   uint32 = 0x00120116 // FILE_WRITE_DATA | FILE_APPEND_DATA | FILE_WRITE_EA | FILE_WRITE_ATTRIBUTES | READ_CONTROL | SYNCHRONIZE
	accessMaskPosixExecute uint32 = 0x001200A0 // FILE_EXECUTE | FILE_READ_ATTRIBUTES | READ_CONTROL | SYNCHRONIZE
	accessMaskPosixMinimal uint32 = 0x00120000 // READ_CONTROL | SYNCHRONIZE
)

// CheckFileAccess validates a CREATE DesiredAccess request against an
// existing file's stored DACL per MS-SMB2 §3.3.5.9 and MS-FSA §2.1.5.1.2.1.
//
// Returns the granted access mask. Behavior:
//
//   - Root bypass (UID 0): returns desiredAccess as granted; MAXIMUM_ALLOWED
//     resolves to GENERIC_ALL.
//   - file.ACL == nil: NO DACL-level enforcement. Per MS-DTYP §2.5.3 a NULL
//     DACL grants every right to every principal; per MS-FSA the access check
//     against the security descriptor is only meaningful when an SD exists.
//     We mirror that: return the requested set (minus MAXIMUM_ALLOWED itself)
//     as granted. POSIX semantics (mode-bit enforcement on read/write/delete)
//     remain enforced by the metadata operation layer downstream of CREATE;
//     this is the open-time gate only, and it must not deny opens that the
//     pre-#529 server permitted (load-bearing for WPTS BVT, smb2.create.multi,
//     and DELETE/WRITE_DAC/WRITE_OWNER requests that the POSIX rwx mapping
//     cannot encode).
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
// This wrapper preserves the legacy "no parent" signature for call sites that
// don't have a parent handle in scope (root-handle access checks). CREATE-path
// callers with a parent in scope should use CheckFileAccessWithParent so the
// parent's FILE_DELETE_CHILD can override a DELETE denial on the file's own
// DACL — see that function's documentation for the spec citation.
//
// GENERIC_* bits in desiredAccess are expanded to their file-object-specific
// rights before evaluation (see CheckFileAccessWithParent). OWNER_RIGHTS and
// ACCESSBASED enumeration remain out of scope here — tracked under #531/#532.
func (s *Service) CheckFileAccess(file *File, authCtx *AuthContext, desiredAccess uint32) (uint32, error) {
	return s.CheckFileAccessWithParent(file, nil, authCtx, desiredAccess)
}

// CheckFileAccessWithParent extends CheckFileAccess with the standard
// Windows "delete via parent" override per MS-FSA §2.1.4.13 / §2.1.5.1.2.4:
// when the file's own DACL denies DELETE but the parent directory grants
// FILE_DELETE_CHILD (ACE4_DELETE_CHILD, 0x40) to the caller, DELETE is
// granted on the open. This mirrors Samba's parent_override_delete() in
// source3/smbd/open.c, which cites
// https://blogs.msdn.com/oldnewthing/archive/2004/06/04/148426.aspx —
// "Why does this user with administrative rights get an access-denied
// error when trying to delete a file?"
//
// Without this override, smbtorture's smb2_deltree algorithm cannot remove
// test files whose own DACL was set restrictively by a prior subtest,
// because deltree's recursive unlink opens each child with
// SEC_STD_DELETE | FILE_DELETE_ON_CLOSE | FILE_NON_DIRECTORY_FILE. The
// owner of the parent dir always has DELETE_CHILD via the synthesized
// owner-rwx mask (acl/synthesize.go::rwxToFullMask), so the override
// reliably succeeds for setup_dir's intended cleanup.
//
// The override only applies to the DELETE bit. All other bits remain
// gated by the file's own DACL.
//
// When parent is nil, behavior is identical to CheckFileAccess.
func (s *Service) CheckFileAccessWithParent(file *File, parent *File, authCtx *AuthContext, desiredAccess uint32) (uint32, error) {
	return s.CheckFileAccessWithParentGeneric(file, parent, authCtx, desiredAccess, 0)
}

// CheckFileAccessWithParentGeneric is CheckFileAccessWithParent with explicit
// knowledge of which specific bits in desiredAccess were introduced by GENERIC_*
// expansion (genericDerived). Under SEC_FLAG_MAXIMUM_ALLOWED those bits are
// best-effort: the strict explicit-bit enforcement gate (which fails the open
// when a DIRECTLY-named specific right is denied — smb2.acls.MXAC-NOT-GRANTED)
// excludes them. This matches Samba, where MAX|GENERIC_EXECUTE against a
// read-only DACL succeeds (smb2.maximum_allowed.maximum_allowed) but
// MAX|FILE_WRITE_DATA against the same DACL is denied.
//
// genericDerived is expected to already be in file-object-specific terms (see
// acl.GenericDerivedBits). Pass 0 when the caller has no generic bits to expand.
func (s *Service) CheckFileAccessWithParentGeneric(file *File, parent *File, authCtx *AuthContext, desiredAccess, genericDerived uint32) (uint32, error) {
	maximumAllowed := desiredAccess&accessMaskMaximumAllowed != 0
	// Expand MS-DTYP §2.4.3 GENERIC_* bits to their file-object-specific
	// rights before the subset checks below (MS-DTYP §2.5.3 / MS-FSA
	// §2.1.5.1.2.1). Without this, a request such as MAXIMUM_ALLOWED |
	// GENERIC_READ leaves the raw GENERIC_READ bit (0x80000000) in `explicit`
	// while the DACL evaluation produces only the specific READ rights — the
	// `effective&explicit == explicit` / `granted&explicit == explicit`
	// subset checks could then never be satisfied, denying an open the DACL
	// actually permits (smb2.maximum_allowed). ExpandGenericMask strips the
	// generic bits and leaves MAXIMUM_ALLOWED untouched.
	explicit := acl.ExpandGenericMask(desiredAccess &^ accessMaskMaximumAllowed)

	// Fold any GENERIC_* bits still present in the raw request into the
	// best-effort set. Production callers pre-expand generics and pass the
	// derived bits explicitly (effectiveAccess no longer carries the raw
	// generic flags); direct callers that pass an un-expanded mask are handled
	// here. Either way the MAX strict-enforcement gate below excludes these.
	genericDerived |= acl.GenericDerivedBits(desiredAccess &^ accessMaskMaximumAllowed)

	// Root bypass: identical semantics to computeMaximalAccess and
	// evaluateACLPermissions. UID 0 gets everything; MAXIMUM_ALLOWED resolves
	// to GENERIC_ALL.
	if authCtx != nil && authCtx.Identity != nil && authCtx.Identity.UID != nil && *authCtx.Identity.UID == 0 {
		if maximumAllowed {
			return accessMaskPosixGenericAll | explicit, nil
		}
		return explicit, nil
	}

	// ACCESS_SYSTEM_SECURITY (SACL access) requires SeSecurityPrivilege per
	// MS-DTYP §2.4.3 / MS-FSA §2.1.4.13. Without that privilege the open
	// MUST fail with STATUS_PRIVILEGE_NOT_HELD — Samba returns the same
	// status via se_access_check_implicit_owner. DittoFS has no notion of
	// SeSecurityPrivilege today, so the bit is unconditionally denied to
	// non-root callers. Required by smbtorture
	// smb2.maximum_allowed.maximum_allowed which probes the SACL bit
	// expecting STATUS_PRIVILEGE_NOT_HELD rather than STATUS_ACCESS_DENIED.
	// MAXIMUM_ALLOWED does NOT mask this requirement: Samba's
	// se_access_check_implicit_owner enforces it whether the request was
	// MAX-only or explicit.
	if explicit&accessMaskSystemSecurity != 0 {
		return 0, &StoreError{
			Code:    ErrPrivilegeRequired,
			Message: "SeSecurityPrivilege required for ACCESS_SYSTEM_SECURITY",
		}
	}

	// No DACL stored: nothing to enforce at the open gate. Grant the explicit
	// set as-is; MAXIMUM_ALLOWED expands to GENERIC_ALL. Downstream metadata
	// ops (read/write/delete/setattr) still apply their own POSIX-mode checks,
	// so a per-op DENY for a non-owner with mode 0o600 still produces
	// STATUS_ACCESS_DENIED at the operation level — just not at open.
	if file.ACL == nil {
		if maximumAllowed {
			return accessMaskPosixGenericAll | explicit, nil
		}
		return explicit, nil
	}

	// DACL-present path: single-pass acl.EvaluateGranted. It accumulates the
	// per-bit allow/deny decisions across the whole DACL in one walk and
	// returns the granted subset of `explicit` — bit-identical to probing each
	// requested bit through acl.Evaluate individually, without conflating
	// per-bit DENY semantics.
	evalCtx := buildFileAccessEvalContext(file, authCtx)
	granted := acl.EvaluateGranted(file.ACL, evalCtx, explicit)

	// MS-FSA §2.1.4.13 "Algorithm to Check Access to an Existing File":
	// FILE_READ_ATTRIBUTES is always granted from the containing directory
	// once traverse access to the file's path succeeds. The bit is unmasked
	// from the file's DACL evaluation — even a DACL that explicitly omits
	// READ_ATTRIBUTES still yields a successful open requesting it.
	//
	// Mirrors Samba source3/smbd/open.c::smbd_check_access_rights_fsp which
	// sets `do_not_check_mask = FILE_READ_ATTRIBUTES` before invoking the
	// DACL check. Covers smb2.acls.OWNER (acls.c:765 loop iteration with
	// bit=0x80 expecting OK on a DACL that only grants WRITE_DATA). Refs #559.
	if explicit&acl.ACE4_READ_ATTRIBUTES != 0 && granted&acl.ACE4_READ_ATTRIBUTES == 0 {
		granted |= acl.ACE4_READ_ATTRIBUTES
	}

	// Parent override for DELETE: when the file's own DACL denied DELETE but
	// the parent directory grants FILE_DELETE_CHILD to the caller, grant
	// DELETE here (MS-FSA §2.1.4.13 / Samba parent_override_delete). The
	// override fires only when DELETE was actually requested and not yet
	// granted, and only when a parent file is in scope. This is the same
	// Windows semantics that lets administrators delete files with no DELETE
	// in their own DACL via the containing folder's permissions.
	//
	// Null parent DACL (parent.ACL == nil) follows MS-DTYP §2.5.3: a NULL DACL
	// grants every right to every principal, so the override applies.
	if explicit&acl.ACE4_DELETE != 0 && granted&acl.ACE4_DELETE == 0 && parent != nil {
		if parentGrantsDeleteChild(parent, authCtx) {
			granted |= acl.ACE4_DELETE
		}
	}

	if maximumAllowed {
		// MAXIMUM_ALLOWED on its own never denies. Compute the full set of
		// bits the requester is allowed (independent of what they asked for,
		// minus the MAXIMUM_ALLOWED bit itself). Mirrors computeMaximalAccess
		// so the handle's effective access matches the MxAc reply.
		// Single-pass effective-rights computation over the same evalCtx,
		// equivalent to probing each acl.ProbeBitsAll bit through acl.Evaluate.
		effective := acl.EvaluateGranted(file.ACL, evalCtx, acl.ProbeMaskAll)
		// Apply parent FILE_DELETE_CHILD override to the MAXIMUM_ALLOWED
		// effective-rights set too so the open's GrantedAccess (which the
		// caller propagates into Open.GrantedAccess and surfaces via MxAc)
		// reflects the same Windows semantics on MaxAccess queries.
		if parent != nil && parentGrantsDeleteChild(parent, authCtx) {
			effective |= acl.ACE4_DELETE
		}
		// MS-FSA §2.1.4.13: FILE_READ_ATTRIBUTES is always granted from the
		// containing directory (see explicit-branch comment above). Surface it
		// in the MaxAccess set so MxAc replies and MAXIMUM_ALLOWED opens carry
		// the bit. Refs #559.
		effective |= acl.ACE4_READ_ATTRIBUTES
		effective |= granted

		// Per MS-SMB2 §3.3.5.9 paragraph 8 and Samba
		// smbd_calculate_maximum_allowed_access_fsp: even when
		// MAXIMUM_ALLOWED is set, every EXPLICIT non-MAX bit in DesiredAccess
		// MUST be granted by the DACL. If any explicit bit is missing from
		// the resolved effective set, the open MUST fail STATUS_ACCESS_DENIED.
		// MAXIMUM_ALLOWED suppresses denial only for the bits IMPLICITLY
		// requested via the MAX flag itself, not for bits the caller named
		// outright. Covers smb2.acls.MXAC-NOT-GRANTED.
		//
		// Bits introduced by GENERIC_* expansion are NOT directly-named rights:
		// Samba maps generic→specific only for the best-effort maximal set and
		// never strict-enforces the mapped bits under MAXIMUM_ALLOWED. So
		// MAX|GENERIC_EXECUTE (expands to FILE_EXECUTE|... which a read-only DACL
		// lacks) still succeeds — smb2.maximum_allowed.maximum_allowed — while
		// MAX|FILE_WRITE_DATA (a directly-named specific bit) is denied. Exclude
		// the generic-derived bits from the strict gate to honor both.
		named := explicit &^ genericDerived
		if named != 0 && effective&named != named {
			return effective, &StoreError{
				Code:    ErrAccessDenied,
				Message: "explicit non-MAXIMUM_ALLOWED bit denied by file DACL",
			}
		}
		return effective, nil
	}

	// Strict mode: any requested non-MAXIMUM_ALLOWED bit not granted = deny.
	if explicit != 0 && granted&explicit != explicit {
		return granted, &StoreError{
			Code:    ErrAccessDenied,
			Message: "desired access denied by file DACL",
		}
	}
	return granted, nil
}

// parentGrantsDeleteChild decides whether a parent directory grants
// FILE_DELETE_CHILD (ACE4_DELETE_CHILD) to the requester for the purposes
// of the MS-FSA §2.1.4.13 delete-via-parent override. Null DACL on the
// parent grants everything (MS-DTYP §2.5.3). Root and nil-identity callers
// also bypass — consistent with the rest of CheckFileAccessWithParent.
func parentGrantsDeleteChild(parent *File, authCtx *AuthContext) bool {
	if parent == nil {
		return false
	}
	// Null DACL: grant per MS-DTYP §2.5.3.
	if parent.ACL == nil {
		return true
	}
	// Root bypass: identical to the file-side root short-circuit.
	if authCtx != nil && authCtx.Identity != nil && authCtx.Identity.UID != nil && *authCtx.Identity.UID == 0 {
		return true
	}
	parentEvalCtx := buildFileAccessEvalContext(parent, authCtx)
	return acl.Evaluate(parent.ACL, parentEvalCtx, acl.ACE4_DELETE_CHILD)
}

// ComputeMaximalAccess computes the maximal access mask a requester is
// granted on file, for use in the MS-SMB2 §2.2.13.2 MxAc create-context
// reply. It is the single source of truth for maximal-access evaluation;
// protocol adapters call it instead of reimplementing ACL/POSIX evaluation.
//
// When the file carries an explicit DACL (file.ACL != nil), each MS-DTYP
// access-right bit in acl.ProbeBitsAll is probed against the DACL using the
// same EvaluateContext shape as CheckFileAccess, so the MxAc reply stays
// consistent with permission checks on subsequent operations against the same
// handle. The owner short-circuit is intentionally NOT applied on the ACL
// path: MS-SMB2 §2.2.13.2 requires the reply to reflect SD evaluation, and an
// owner can legitimately be restricted by an OWNER_RIGHTS ACE (MS-DTYP
// §2.5.3). Root (UID 0) still short-circuits to GENERIC_ALL, mirroring
// CheckFileAccessWithParentGeneric.
//
// When file.ACL is nil, the POSIX fallback maps the file's mode bits:
//   - Owner: GENERIC_ALL.
//   - Group member / other: the rwx bundles (accessMaskPosixRead/Write/Execute).
//   - Any authenticated user with no granted bits still receives the minimal
//     READ_CONTROL | SYNCHRONIZE set.
func (s *Service) ComputeMaximalAccess(file *File, authCtx *AuthContext) uint32 {
	// ACL-aware path: probe each defined access-right bit against the SD.
	// No owner short-circuit — the DACL may legitimately restrict the owner.
	if file.ACL != nil {
		// Root bypass mirrors CheckFileAccessWithParentGeneric. Hoisted above
		// evalCtx construction to avoid the allocation on the root-admin hot
		// path.
		if authCtx != nil && authCtx.Identity != nil && authCtx.Identity.UID != nil && *authCtx.Identity.UID == 0 {
			return accessMaskPosixGenericAll
		}
		evalCtx := buildFileAccessEvalContext(file, authCtx)
		// Single-pass probe over acl.ProbeMaskAll (the OR of acl.ProbeBitsAll),
		// mirroring the MAXIMUM_ALLOWED branch of
		// CheckFileAccessWithParentGeneric. ProbeBitsAll contains only specific
		// rights, so the previous per-bit ExpandGenericMask was a no-op; the
		// result is still expanded defensively per MS-DTYP §2.5.3 so the
		// MaximalAccess reply contains only resolved rights.
		granted := acl.EvaluateGranted(file.ACL, evalCtx, acl.ProbeMaskAll)
		return acl.ExpandGenericMask(granted)
	}

	// POSIX fallback: owner gets GENERIC_ALL.
	if authCtx != nil && authCtx.Identity != nil && authCtx.Identity.UID != nil && *authCtx.Identity.UID == file.UID {
		return accessMaskPosixGenericAll
	}

	// Compute from POSIX mode bits for a non-owner. Use group permission bits
	// when the requester is a member of the file's owning group, otherwise the
	// "other" bits.
	isGroupMember := authCtx != nil && authCtx.Identity != nil && authCtx.Identity.GID != nil && *authCtx.Identity.GID == file.GID
	if !isGroupMember && authCtx != nil && authCtx.Identity != nil {
		for _, gid := range authCtx.Identity.GIDs {
			if gid == file.GID {
				isGroupMember = true
				break
			}
		}
	}

	var permBits uint32
	if isGroupMember {
		permBits = uint32((file.Mode >> 3) & 0x7) // group bits
	} else {
		permBits = uint32(file.Mode & 0x7) // other bits
	}

	var access uint32
	if permBits&0x4 != 0 { // read
		access |= accessMaskPosixRead
	}
	if permBits&0x2 != 0 { // write
		access |= accessMaskPosixWrite
	}
	if permBits&0x1 != 0 { // execute
		access |= accessMaskPosixExecute
	}

	// Ensure at minimum READ_CONTROL | SYNCHRONIZE for any authenticated user.
	if access == 0 {
		access = accessMaskPosixMinimal
	}

	return access
}

// buildFileAccessEvalContext mirrors evaluateACLPermissions's EvaluateContext
// construction so per-bit ACL evaluation in CheckFileAccess produces the same
// allow/deny decisions a downstream read/write permission check would later
// produce against the same file. Kept private to the metadata package.
func buildFileAccessEvalContext(file *File, authCtx *AuthContext) *acl.EvaluateContext {
	if authCtx == nil || authCtx.Identity == nil || authCtx.Identity.UID == nil {
		// FileOwnerUID is forced to the AnonymousFileOwnerUID sentinel so
		// the anonymous requester's zero-valued UID cannot collapse onto a
		// root-owned file's owner and pick up OWNER@ ACEs plus the
		// MS-DTYP §2.5.3.2 owner-implicit RC|WRITE_DAC grant. See #540.
		return &acl.EvaluateContext{
			FileOwnerUID: acl.AnonymousFileOwnerUID,
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
	// MS-DTYP §2.5.3.2: owner-implicit WRITE_OWNER requires
	// SeTakeOwnershipPrivilege (admins only). See acl.Evaluate.
	evalCtx.RequesterHasTakeOwnership = acl.HasTakeOwnershipPrivilege(evalCtx.SID, evalCtx.GroupSIDs)

	return evalCtx
}

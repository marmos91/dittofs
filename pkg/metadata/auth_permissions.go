package metadata

import (
	"context"

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
	// If the user has share-level write permission (ctx.ShareWritable), grant write-
	// related permissions on files in the share, bypassing file-level Unix permission
	// checks. This allows authenticated users with share write access to create/modify
	// files even if the file's Unix permissions would normally deny access.
	//
	// Note: ShareReadOnly takes precedence - if the share is read-only for this user,
	// write permission is denied regardless of ShareWritable.
	if ctx.ShareWritable && !ctx.ShareReadOnly {
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

// checkReadPermission checks read permission on a file.
func (s *MetadataService) checkReadPermission(ctx *AuthContext, handle FileHandle) error {
	return s.checkPermission(ctx, handle, PermissionRead, "read permission denied")
}

// checkExecutePermission checks execute/traverse permission on a file.
func (s *MetadataService) checkExecutePermission(ctx *AuthContext, handle FileHandle) error {
	return s.checkPermission(ctx, handle, PermissionExecute, "execute permission denied")
}

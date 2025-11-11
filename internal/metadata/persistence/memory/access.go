package memory

import (
	"context"
	"fmt"
	"net"
	"slices"

	"github.com/marmos91/dittofs/internal/metadata"
)

// CheckExportAccess verifies if a client can access an export and applies squashing rules.
//
// This method implements the complete export-level access control flow as defined
// in RFC 1813 and common NFS server implementations. It is called during the mount
// process to determine whether a client should be granted access to an export.
//
// Access Control Flow:
//  1. Verify the export exists
//  2. Check authentication requirements (RequireAuth flag)
//  3. Validate authentication flavor against allowed list
//  4. Check denied clients list (deny takes precedence)
//  5. Check allowed clients list (if specified)
//  6. Apply UID/GID squashing rules (AllSquash, RootSquash)
//  7. Return access decision with effective credentials
//
// Squashing:
// Squashing is applied at mount time so that all subsequent operations use
// the squashed credentials consistently. This ensures:
//   - Root access can be mapped to nobody/nogroup (RootSquash)
//   - All users can be mapped to a single user (AllSquash)
//   - Consistent credential handling throughout the mount session
//
// The returned AuthContext contains the effective (post-squash) credentials
// that should be used for all permission checks during this mount session.
//
// Parameters:
//   - exportPath: Path of the export being accessed
//   - clientAddr: IP address of the client
//   - authFlavor: Authentication flavor (0=AUTH_NULL, 1=AUTH_UNIX, etc.)
//   - uid: Unix UID from client (may be nil for AUTH_NULL)
//   - gid: Unix GID from client (may be nil for AUTH_NULL)
//   - gids: Supplementary group IDs from client
//
// Returns:
//   - *AccessDecision: Contains allowed status, reason, and export properties
//   - *AuthContext: Contains effective credentials after squashing
//   - error: Returns ExportError with specific error code if access denied
func (r *MemoryRepository) CheckExportAccess(
	ctx context.Context,
	exportPath string,
	clientAddr string,
	authFlavor uint32,
	uid *uint32,
	gid *uint32,
	gids []uint32,
) (*metadata.AccessDecision, *metadata.AuthContext, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// ========================================================================
	// Step 1: Check if export exists
	// ========================================================================

	ed, exists := r.exports[exportPath]
	if !exists {
		return nil, nil, &metadata.ExportError{
			Code:    metadata.ExportErrNotFound,
			Message: fmt.Sprintf("export not found: %s", exportPath),
			Export:  exportPath,
		}
	}

	opts := ed.Export.Options

	// ========================================================================
	// Step 2: Check authentication requirements
	// ========================================================================

	if opts.RequireAuth && authFlavor == 0 {
		return nil, nil, &metadata.ExportError{
			Code:    metadata.ExportErrAuthRequired,
			Message: "authentication required for this export",
			Export:  exportPath,
		}
	}

	// ========================================================================
	// Step 3: Check if auth flavor is allowed
	// ========================================================================

	if len(opts.AllowedAuthFlavors) > 0 {
		allowed := slices.Contains(opts.AllowedAuthFlavors, authFlavor)
		if !allowed {
			return nil, nil, &metadata.ExportError{
				Code:    metadata.ExportErrAuthRequired,
				Message: fmt.Sprintf("authentication flavor %d not allowed", authFlavor),
				Export:  exportPath,
			}
		}
	}

	// ========================================================================
	// Step 4: Check denied clients first (deny takes precedence)
	// ========================================================================

	if len(opts.DeniedClients) > 0 {
		for _, denied := range opts.DeniedClients {
			if matchesIPPattern(clientAddr, denied) {
				return nil, nil, &metadata.ExportError{
					Code:    metadata.ExportErrAccessDenied,
					Message: fmt.Sprintf("client %s is explicitly denied", clientAddr),
					Export:  exportPath,
				}
			}
		}
	}

	// ========================================================================
	// Step 5: Check allowed clients (if specified)
	// ========================================================================

	if len(opts.AllowedClients) > 0 {
		allowed := false
		for _, pattern := range opts.AllowedClients {
			if matchesIPPattern(clientAddr, pattern) {
				allowed = true
				break
			}
		}
		if !allowed {
			return nil, nil, &metadata.ExportError{
				Code:    metadata.ExportErrAccessDenied,
				Message: fmt.Sprintf("client %s not in allowed list", clientAddr),
				Export:  exportPath,
			}
		}
	}

	// ========================================================================
	// Step 6: Determine allowed auth flavors for response
	// ========================================================================

	allowedAuth := opts.AllowedAuthFlavors
	if len(allowedAuth) == 0 {
		// If not specified, allow AUTH_NULL and AUTH_UNIX (RFC 1813 default)
		allowedAuth = []uint32{0, 1}
	}

	// ========================================================================
	// Step 7: Apply squashing rules to get effective credentials
	// ========================================================================

	effectiveUID, effectiveGID, effectiveGIDs := metadata.ApplySquashing(
		&opts,
		authFlavor,
		uid,
		gid,
		gids,
	)

	// ========================================================================
	// Step 8: Build authentication context with effective credentials
	// ========================================================================

	authCtx := &metadata.AuthContext{
		AuthFlavor: authFlavor,
		UID:        effectiveUID,
		GID:        effectiveGID,
		GIDs:       effectiveGIDs,
		ClientAddr: clientAddr,
	}

	// ========================================================================
	// Step 9: Build access decision
	// ========================================================================

	decision := &metadata.AccessDecision{
		Allowed:     true,
		Reason:      "access granted",
		AllowedAuth: allowedAuth,
		ReadOnly:    opts.ReadOnly,
	}

	return decision, authCtx, nil
}

// matchesIPPattern checks if an IP address matches a pattern (IP address or CIDR).
//
// This supports two formats:
//   - CIDR notation: "192.168.1.0/24" matches all IPs in that subnet
//   - Exact IP: "192.168.1.100" matches only that specific IP
//
// This is used for allowed/denied client lists in export configurations.
//
// Parameters:
//   - clientIP: The client's IP address to check
//   - pattern: The pattern to match against (CIDR or exact IP)
//
// Returns:
//   - bool: True if the IP matches the pattern
func matchesIPPattern(clientIP string, pattern string) bool {
	// Try parsing as CIDR first
	_, ipNet, err := net.ParseCIDR(pattern)
	if err == nil {
		ip := net.ParseIP(clientIP)
		if ip != nil {
			return ipNet.Contains(ip)
		}
		return false
	}

	// Otherwise, exact IP match
	return clientIP == pattern
}

// CheckAccess performs Unix-style permission checking for file access.
//
// This implements the access control logic for the ACCESS NFS procedure,
// following standard Unix permission semantics with owner/group/other
// permission bits.
//
// Permission Check Flow:
//  1. If AUTH_NULL, grant minimal permissions (world-readable only)
//  2. If owner (UID matches), check owner permission bits (rwx)
//  3. If group member (GID or supplementary GID matches), check group bits
//  4. Otherwise, check other permission bits
//
// Permission Bit Mapping:
//   - Unix read (4): NFS AccessRead (0x0001)
//   - Unix write (2): NFS AccessModify/Extend/Delete (0x0004/0x0008/0x0010)
//   - Unix execute (1): NFS AccessLookup/Execute (0x0002/0x0020)
//
// Directory vs File Permissions:
//   - Directories: read=list, execute=search, write=modify entries
//   - Files: read=read data, execute=execute file, write=modify data
//
// Parameters:
//   - handle: The file handle to check permissions for
//   - requested: Bitmap of requested permissions (AccessRead, AccessModify, etc.)
//   - ctx: Authentication context with client credentials
//
// Returns:
//   - uint32: Bitmap of granted permissions (subset of requested)
//   - error: Returns error only for internal failures (file not found)
//
// Note: Denied access returns 0 permissions, not an error. This follows
// RFC 1813 semantics where ACCESS always succeeds but may grant no permissions.
func (r *MemoryRepository) CheckAccess(ctx *metadata.AccessCheckContext, handle metadata.FileHandle, requested uint32) (uint32, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// ========================================================================
	// Step 1: Get file attributes
	// ========================================================================

	key := handleToKey(handle)
	attr, exists := r.files[key]
	if !exists {
		return 0, fmt.Errorf("file not found")
	}

	granted := uint32(0)

	// ========================================================================
	// Step 2: Handle AUTH_NULL (minimal permissions)
	// ========================================================================

	if ctx.AuthFlavor == 0 || ctx.UID == nil {
		// Only grant read/lookup if world-readable
		if attr.Mode&0004 != 0 { // Other read bit
			if requested&0x0001 != 0 { // AccessRead
				granted |= 0x0001
			}
		}
		if attr.Type == metadata.FileTypeDirectory && attr.Mode&0001 != 0 { // Other execute (search)
			if requested&0x0002 != 0 { // AccessLookup
				granted |= 0x0002
			}
		}
		return granted, nil
	}

	uid := *ctx.UID
	gid := *ctx.GID

	// ========================================================================
	// Step 3: Determine which permission bits apply
	// ========================================================================

	var permBits uint32

	if uid == attr.UID {
		// Owner permissions (bits 6-8)
		permBits = (attr.Mode >> 6) & 0x7
	} else if gid == attr.GID || containsGID(ctx.GIDs, attr.GID) {
		// Group permissions (bits 3-5)
		permBits = (attr.Mode >> 3) & 0x7
	} else {
		// Other permissions (bits 0-2)
		permBits = attr.Mode & 0x7
	}

	// ========================================================================
	// Step 4: Map Unix permission bits to NFS access bits
	// ========================================================================
	// permBits: rwx = 0x4 (read), 0x2 (write), 0x1 (execute)

	hasRead := (permBits & 0x4) != 0
	hasWrite := (permBits & 0x2) != 0
	hasExecute := (permBits & 0x1) != 0

	// ========================================================================
	// Step 5: Grant permissions based on file type
	// ========================================================================

	if attr.Type == metadata.FileTypeDirectory {
		// Directory-specific permissions
		if hasRead && (requested&0x0001 != 0) { // AccessRead
			granted |= 0x0001 // Can list directory
		}
		if hasExecute && (requested&0x0002 != 0) { // AccessLookup
			granted |= 0x0002 // Can search/lookup in directory
		}
		if hasWrite {
			if requested&0x0004 != 0 { // AccessModify
				granted |= 0x0004 // Can modify directory entries
			}
			if requested&0x0008 != 0 { // AccessExtend
				granted |= 0x0008 // Can add entries
			}
			if requested&0x0010 != 0 { // AccessDelete
				granted |= 0x0010 // Can delete entries
			}
		}
		if hasExecute && (requested&0x0020 != 0) { // AccessExecute
			granted |= 0x0020 // Can traverse directory
		}
	} else {
		// Regular file and other types
		if hasRead && (requested&0x0001 != 0) { // AccessRead
			granted |= 0x0001 // Can read file
		}
		if hasWrite {
			if requested&0x0004 != 0 { // AccessModify
				granted |= 0x0004 // Can modify file
			}
			if requested&0x0008 != 0 { // AccessExtend
				granted |= 0x0008 // Can extend file
			}
		}
		if hasExecute && (requested&0x0020 != 0) { // AccessExecute
			granted |= 0x0020 // Can execute file
		}

		// Delete permission requires write access to parent directory
		// We can't check that here without parent context, so we grant it
		// if the user has write permission on the file itself
		// The actual delete operation will do proper parent directory checks
		if hasWrite && (requested&0x0010 != 0) { // AccessDelete
			granted |= 0x0010
		}
	}

	return granted, nil
}

// CheckDumpAccess verifies if a client can call the DUMP procedure.
//
// The DUMP procedure lists all active mounts and is typically restricted
// to prevent information disclosure. This method checks if a client is
// authorized to call DUMP based on server configuration.
//
// Access Logic:
//  1. If no restrictions configured, allow all (RFC 1813 default)
//  2. Check denied list first (deny takes precedence)
//  3. Check allowed list (if specified)
//
// Configuration:
//   - DumpAllowedClients: List of IPs/CIDRs that can call DUMP
//   - DumpDeniedClients: List of IPs/CIDRs that cannot call DUMP
//
// Parameters:
//   - clientAddr: IP address of the client
//
// Returns:
//   - error: Returns ExportError if access denied, nil if allowed
func (r *MemoryRepository) CheckDumpAccess(ctx context.Context, clientAddr string) error {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// ========================================================================
	// Step 1: Check if any restrictions are configured
	// ========================================================================

	if len(r.serverConfig.DumpAllowedClients) == 0 &&
		len(r.serverConfig.DumpDeniedClients) == 0 {
		// No restrictions configured, allow all (RFC 1813 default)
		return nil
	}

	// ========================================================================
	// Step 2: Check denied clients first (deny takes precedence)
	// ========================================================================

	if len(r.serverConfig.DumpDeniedClients) > 0 {
		for _, denied := range r.serverConfig.DumpDeniedClients {
			if matchesIPPattern(clientAddr, denied) {
				return &metadata.ExportError{
					Code:    metadata.ExportErrAccessDenied,
					Message: fmt.Sprintf("client %s is denied DUMP access", clientAddr),
					Export:  "DUMP",
				}
			}
		}
	}

	// ========================================================================
	// Step 3: Check allowed clients (if specified)
	// ========================================================================

	if len(r.serverConfig.DumpAllowedClients) > 0 {
		allowed := false
		for _, pattern := range r.serverConfig.DumpAllowedClients {
			if matchesIPPattern(clientAddr, pattern) {
				allowed = true
				break
			}
		}
		if !allowed {
			return &metadata.ExportError{
				Code:    metadata.ExportErrAccessDenied,
				Message: fmt.Sprintf("client %s not in DUMP allowed list", clientAddr),
				Export:  "DUMP",
			}
		}
	}

	// Access granted
	return nil
}

// containsGID checks if a target GID is present in a list of GIDs.
//
// This is used for group membership checks during permission validation.
// It checks both the primary group and supplementary groups.
//
// Parameters:
//   - gids: List of group IDs to search
//   - target: Group ID to find
//
// Returns:
//   - bool: True if target is in the list
func containsGID(gids []uint32, target uint32) bool {
	return slices.Contains(gids, target)
}

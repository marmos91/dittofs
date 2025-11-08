package metadata

// ApplySquashing applies UID/GID squashing rules based on export options.
//
// This function implements the standard NFS squashing behaviors:
//   - AllSquash: Maps all users to anonymous UID/GID
//   - RootSquash: Maps root (UID 0) to anonymous UID/GID
//   - AUTH_NULL: Always uses anonymous UID/GID
//
// Squashing is a security and access control mechanism that prevents
// clients from using their own UID/GID for file operations. This is
// particularly useful for:
//   - Public/world-accessible exports (AllSquash)
//   - Preventing root privilege escalation (RootSquash)
//   - Anonymous access scenarios
//
// Parameters:
//   - opts: Export options containing squashing configuration
//   - authFlavor: Authentication method (0=AUTH_NULL, 1=AUTH_UNIX)
//   - uid: Original UID from client credentials (nil for AUTH_NULL)
//   - gid: Original GID from client credentials (nil for AUTH_NULL)
//   - gids: Original supplementary GIDs from client credentials
//
// Returns:
//   - effectiveUID: UID to use for permission checks
//   - effectiveGID: GID to use for permission checks
//   - effectiveGIDs: Supplementary GIDs to use for permission checks
func ApplySquashing(
	opts *ExportOptions,
	authFlavor uint32,
	uid *uint32,
	gid *uint32,
	gids []uint32,
) (effectiveUID *uint32, effectiveGID *uint32, effectiveGIDs []uint32) {
	// Determine anonymous UID/GID (use configured values or defaults)
	anonUID := opts.AnonUID
	if anonUID == 0 {
		anonUID = DefaultAnonUID
	}

	anonGID := opts.AnonGID
	if anonGID == 0 {
		anonGID = DefaultAnonGID
	}

	// AUTH_NULL always gets anonymous credentials
	if authFlavor == 0 { // AUTH_NULL
		return &anonUID, &anonGID, []uint32{}
	}

	// AUTH_UNIX with AllSquash: map all users to anonymous
	if opts.AllSquash {
		return &anonUID, &anonGID, []uint32{}
	}

	// AUTH_UNIX with RootSquash: map root to anonymous
	if opts.RootSquash && uid != nil && *uid == 0 {
		squashedUID := anonUID
		squashedGID := anonGID

		// If original GID is also 0, squash it too
		if gid != nil && *gid == 0 {
			return &squashedUID, &squashedGID, gids
		}

		// GID is not root, keep original GID but squash UID
		return &squashedUID, gid, gids
	}

	// No squashing: use original credentials
	return uid, gid, gids
}

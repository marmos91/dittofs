package metadata

import "context"

// AuthContext contains authentication credentials for access control checks.
// This is extracted from the RPC authentication layer and passed to the repository.
//
// IMPORTANT: The UID/GID fields in this structure may have been modified by
// squashing options (AllSquash, RootSquash) and should be used for all
// permission checks. The original credentials are not preserved here.
type AuthContext struct {
	Context context.Context

	// AuthFlavor is the authentication method used
	// 0 = AUTH_NULL, 1 = AUTH_UNIX, etc.
	AuthFlavor uint32

	// UID is the effective user ID after applying squashing rules.
	// For AUTH_NULL: Set to AnonUID
	// For AUTH_UNIX with AllSquash: Set to AnonUID
	// For AUTH_UNIX with RootSquash and UID==0: Set to AnonUID
	// For AUTH_UNIX otherwise: Original UID from credentials
	// Only valid when AuthFlavor == AUTH_UNIX (or squashed from AUTH_UNIX)
	UID *uint32

	// GID is the effective group ID after applying squashing rules.
	// For AUTH_NULL: Set to AnonGID
	// For AUTH_UNIX with AllSquash: Set to AnonGID
	// For AUTH_UNIX with RootSquash and GID==0: Set to AnonGID
	// For AUTH_UNIX otherwise: Original GID from credentials
	// Only valid when AuthFlavor == AUTH_UNIX (or squashed from AUTH_UNIX)
	GID *uint32

	// GIDs is a list of supplementary group IDs after applying squashing rules.
	// For AUTH_NULL: Empty
	// For AUTH_UNIX with AllSquash: Empty (only AnonGID is used)
	// For AUTH_UNIX with RootSquash: Original GIDs (root squashing doesn't affect supplementary groups)
	// For AUTH_UNIX otherwise: Original supplementary GIDs
	// Only valid when AuthFlavor == AUTH_UNIX (or squashed from AUTH_UNIX)
	GIDs []uint32

	// ClientAddr is the network address of the client
	// Format: "IP:port" or just "IP"
	ClientAddr string
}

// AccessCheckContext contains authentication context for access permission checks.
// This is used by the CheckAccess repository method to determine if a client
// has specific permissions on a file or directory.
type AccessCheckContext struct {
	Context context.Context

	// AuthFlavor indicates the authentication method.
	// 0 = AUTH_NULL, 1 = AUTH_UNIX, etc.
	AuthFlavor uint32

	// UID is the authenticated user ID (only valid for AUTH_UNIX).
	UID *uint32

	// GID is the authenticated group ID (only valid for AUTH_UNIX).
	GID *uint32

	// GIDs is a list of supplementary group IDs (only valid for AUTH_UNIX).
	GIDs []uint32
}

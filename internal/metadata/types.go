package metadata

// TimeDelta represents time resolution with seconds and nanoseconds.
type TimeDelta struct {
	// Seconds component of time resolution
	Seconds uint32

	// Nseconds component of time resolution (0-999999999)
	Nseconds uint32
}

// AccessCheckContext contains authentication context for access permission checks.
// This is used by the CheckAccess repository method to determine if a client
// has specific permissions on a file or directory.
type AccessCheckContext struct {
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

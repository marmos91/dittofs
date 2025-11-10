package metadata

import "time"

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

// SetAttrs contains the attributes to set in a SETATTR operation.
// Each attribute has a corresponding Set* flag that indicates whether
// that attribute should be modified.
//
// Attributes without their Set* flag set to true remain unchanged.
// This allows atomic multi-attribute updates with selective modification.
//
// Per RFC 1813 Section 3.3.2, the server automatically updates ctime
// (change time) whenever any attribute is modified. Clients cannot set
// ctime directly.
type SetAttrs struct {
	// SetMode indicates whether to update the Mode field.
	SetMode bool

	// Mode is the new permission bits (only lower 12 bits are used).
	// Format: Unix permission bits (0o7777 max)
	//   - Bits 0-2: Other permissions (rwx)
	//   - Bits 3-5: Group permissions (rwx)
	//   - Bits 6-8: Owner permissions (rwx)
	//   - Bits 9-11: Special bits (setuid, setgid, sticky)
	// Only valid when SetMode is true.
	Mode uint32

	// SetUID indicates whether to update the UID field.
	SetUID bool

	// UID is the new owner user ID.
	// Only the file owner or root can change ownership.
	// Only valid when SetUID is true.
	UID uint32

	// SetGID indicates whether to update the GID field.
	SetGID bool

	// GID is the new owner group ID.
	// Only the file owner or root can change group ownership.
	// Only valid when SetGID is true.
	GID uint32

	// SetSize indicates whether to update the Size field.
	SetSize bool

	// Size is the new file size in bytes.
	// If less than current size: truncate (remove trailing content)
	// If greater than current size: extend (pad with zeros)
	// If zero: delete all content
	// Cannot be used on directories or special files.
	// Only valid when SetSize is true.
	Size uint64

	// SetAtime indicates whether to update the Atime field.
	SetAtime bool

	// Atime is the new access time.
	// This can be set to a specific time or SET_TO_SERVER_TIME.
	// Only valid when SetAtime is true.
	Atime time.Time

	// SetMtime indicates whether to update the Mtime field.
	SetMtime bool

	// Mtime is the new modification time.
	// This can be set to a specific time or SET_TO_SERVER_TIME.
	// Only valid when SetMtime is true.
	Mtime time.Time
}

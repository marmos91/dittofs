// Package models provides shared domain types for DittoFS control plane.
//
// This package contains all data models used across the control plane,
// including users, groups, shares, and store configurations. It provides
// a single source of truth for domain types with GORM annotations for
// database persistence.
package models

// SharePermission represents the access level a user or group has on a share.
//
// Permission levels are hierarchical:
//   - none: No access
//   - read: Read-only access
//   - read-write: Full read/write access
//   - admin: Full access plus administrative operations
type SharePermission string

const (
	// PermissionNone indicates no access to the share.
	PermissionNone SharePermission = "none"

	// PermissionRead allows reading files and listing directories.
	PermissionRead SharePermission = "read"

	// PermissionReadWrite allows reading, writing, creating, and deleting files.
	PermissionReadWrite SharePermission = "read-write"

	// PermissionAdmin allows full access plus administrative operations.
	PermissionAdmin SharePermission = "admin"
)

// Level returns the numeric level of the permission for comparison.
// Higher values indicate more permissive access.
//
// Returns:
//   - 0: none
//   - 1: read
//   - 2: read-write
//   - 3: admin
func (p SharePermission) Level() int {
	switch p {
	case PermissionNone:
		return 0
	case PermissionRead:
		return 1
	case PermissionReadWrite:
		return 2
	case PermissionAdmin:
		return 3
	default:
		return 0
	}
}

// CanRead returns true if this permission level allows reading.
func (p SharePermission) CanRead() bool {
	return p.Level() >= PermissionRead.Level()
}

// CanWrite returns true if this permission level allows writing.
func (p SharePermission) CanWrite() bool {
	return p.Level() >= PermissionReadWrite.Level()
}

// CanAdmin returns true if this permission level allows administrative operations.
func (p SharePermission) CanAdmin() bool {
	return p.Level() >= PermissionAdmin.Level()
}

// IsValid returns true if this is a valid permission value.
func (p SharePermission) IsValid() bool {
	switch p {
	case PermissionNone, PermissionRead, PermissionReadWrite, PermissionAdmin:
		return true
	default:
		return false
	}
}

// String returns the string representation of the permission.
func (p SharePermission) String() string {
	return string(p)
}

// ParseSharePermission converts a string to a SharePermission.
// Returns PermissionNone if the string is not a valid permission.
func ParseSharePermission(s string) SharePermission {
	p := SharePermission(s)
	if p.IsValid() {
		return p
	}
	return PermissionNone
}

// MaxPermission returns the higher of two permissions.
func MaxPermission(a, b SharePermission) SharePermission {
	if a.Level() > b.Level() {
		return a
	}
	return b
}

// SquashMode defines the identity mapping mode for NFS shares.
// This matches the Synology NAS squash options.
//
// Options:
//   - none: No mapping, UIDs pass through unchanged
//   - root_to_admin: Root (UID 0) retains admin/root privileges (default)
//   - root_to_guest: Root (UID 0) is mapped to anonymous (root_squash)
//   - all_to_admin: All users are mapped to root/admin
//   - all_to_guest: All users are mapped to anonymous (all_squash)
type SquashMode string

const (
	// SquashNone means no identity mapping - UIDs pass through unchanged.
	// File-level permissions control access.
	SquashNone SquashMode = "none"

	// SquashRootToAdmin means root (UID 0) retains admin/root privileges.
	// This is the default behavior - root has full access.
	SquashRootToAdmin SquashMode = "root_to_admin"

	// SquashRootToGuest means root (UID 0) is mapped to anonymous UID/GID.
	// This is equivalent to traditional NFS "root_squash".
	SquashRootToGuest SquashMode = "root_to_guest"

	// SquashAllToAdmin means all users are mapped to root (UID 0).
	// Everyone gets admin/root privileges.
	SquashAllToAdmin SquashMode = "all_to_admin"

	// SquashAllToGuest means all users are mapped to anonymous UID/GID.
	// This is equivalent to traditional NFS "all_squash".
	SquashAllToGuest SquashMode = "all_to_guest"
)

// DefaultSquashMode is the default squash mode for new shares.
// Root retains admin privileges by default.
const DefaultSquashMode = SquashRootToAdmin

// IsValid returns true if this is a valid squash mode.
func (s SquashMode) IsValid() bool {
	switch s {
	case SquashNone, SquashRootToAdmin, SquashRootToGuest, SquashAllToAdmin, SquashAllToGuest:
		return true
	default:
		return false
	}
}

// String returns the string representation of the squash mode.
func (s SquashMode) String() string {
	return string(s)
}

// ParseSquashMode converts a string to a SquashMode.
// Returns DefaultSquashMode if the string is not a valid mode.
func ParseSquashMode(s string) SquashMode {
	m := SquashMode(s)
	if m.IsValid() {
		return m
	}
	return DefaultSquashMode
}

// MapsRoot returns true if this mode affects root (UID 0).
func (s SquashMode) MapsRoot() bool {
	switch s {
	case SquashRootToGuest, SquashAllToAdmin, SquashAllToGuest:
		return true
	default:
		return false
	}
}

// MapsAllUsers returns true if this mode affects all users.
func (s SquashMode) MapsAllUsers() bool {
	switch s {
	case SquashAllToAdmin, SquashAllToGuest:
		return true
	default:
		return false
	}
}

// AllSquashModes returns all valid squash modes for display/validation.
func AllSquashModes() []SquashMode {
	return []SquashMode{
		SquashNone,
		SquashRootToAdmin,
		SquashRootToGuest,
		SquashAllToAdmin,
		SquashAllToGuest,
	}
}

// SquashModeDescription returns a human-readable description of the squash mode.
func (s SquashMode) Description() string {
	switch s {
	case SquashNone:
		return "No mapping - UIDs pass through"
	case SquashRootToAdmin:
		return "Map root to admin (default)"
	case SquashRootToGuest:
		return "Map root to guest (root_squash)"
	case SquashAllToAdmin:
		return "Map all users to admin"
	case SquashAllToGuest:
		return "Map all users to guest (all_squash)"
	default:
		return "Unknown"
	}
}

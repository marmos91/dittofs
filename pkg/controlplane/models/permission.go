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

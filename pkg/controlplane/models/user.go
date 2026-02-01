package models

import (
	"encoding/hex"
	"fmt"
	"time"
)

// UserRole represents the role of a user in the system.
type UserRole string

const (
	// RoleUser is a regular user with limited permissions.
	RoleUser UserRole = "user"
	// RoleAdmin is an administrator with full permissions.
	RoleAdmin UserRole = "admin"
)

// IsValid checks if the role is a valid UserRole.
func (r UserRole) IsValid() bool {
	return r == RoleUser || r == RoleAdmin
}

// User represents a DittoFS user for authentication and authorization.
//
// Users can authenticate via different protocols (NFS, SMB, API) and have
// their identity mapped to the appropriate format. Unix identity (UID/GID)
// is stored directly on the user for NFS reverse lookup and file ownership.
type User struct {
	ID                 string     `gorm:"primaryKey;size:36" json:"id"`
	Username           string     `gorm:"uniqueIndex;not null;size:255" json:"username"`
	PasswordHash       string     `gorm:"not null" json:"-"`
	NTHash             string     `json:"-"` // For SMB NTLM authentication
	Enabled            bool       `gorm:"default:true" json:"enabled"`
	MustChangePassword bool       `gorm:"default:false" json:"must_change_password"`
	Role               string     `gorm:"default:user;size:50" json:"role"` // user, admin
	UID                *uint32    `gorm:"uniqueIndex" json:"uid,omitempty"`
	GID                *uint32    `json:"gid,omitempty"`
	DisplayName        string     `gorm:"size:255" json:"display_name,omitempty"`
	Email              string     `gorm:"size:255" json:"email,omitempty"`
	CreatedAt          time.Time  `gorm:"autoCreateTime" json:"created_at"`
	LastLogin          *time.Time `json:"last_login,omitempty"`

	// Many-to-many relationship with groups
	Groups []Group `gorm:"many2many:user_groups;" json:"groups,omitempty"`

	// One-to-many relationship with share permissions
	SharePermissions []UserSharePermission `gorm:"foreignKey:UserID" json:"share_permissions,omitempty"`
}

// TableName returns the table name for User.
func (User) TableName() string {
	return "users"
}

// GetDisplayName returns the display name, or username if display name is not set.
func (u *User) GetDisplayName() string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Username
}

// GetNTHash returns the NT hash as a 16-byte array.
// Returns false if the NTHash field is empty or invalid.
func (u *User) GetNTHash() ([16]byte, bool) {
	var ntHash [16]byte
	if u.NTHash == "" {
		return ntHash, false
	}

	decoded, err := hex.DecodeString(u.NTHash)
	if err != nil || len(decoded) != 16 {
		return ntHash, false
	}

	copy(ntHash[:], decoded)
	return ntHash, true
}

// SetNTHashFromPassword computes and sets the NT hash from a plaintext password.
func (u *User) SetNTHashFromPassword(password string) {
	ntHash := ComputeNTHash(password)
	u.NTHash = hex.EncodeToString(ntHash[:])
}

// HasGroup checks if the user belongs to the specified group.
func (u *User) HasGroup(groupName string) bool {
	for _, g := range u.Groups {
		if g.Name == groupName {
			return true
		}
	}
	return false
}

// GetGroupNames returns a slice of group names the user belongs to.
func (u *User) GetGroupNames() []string {
	names := make([]string, len(u.Groups))
	for i, g := range u.Groups {
		names[i] = g.Name
	}
	return names
}

// GetExplicitSharePermission returns the user's explicit permission for a share.
// Returns PermissionNone and false if no explicit permission is set.
// Note: This method requires SharePermissions to be loaded with ShareName populated.
func (u *User) GetExplicitSharePermission(shareName string) (SharePermission, bool) {
	for _, p := range u.SharePermissions {
		if p.ShareName == shareName {
			return SharePermission(p.Permission), true
		}
	}
	return PermissionNone, false
}

// Validate checks if the user has valid configuration.
func (u *User) Validate() error {
	if u.Username == "" {
		return fmt.Errorf("username is required")
	}
	if u.Role != "" && !UserRole(u.Role).IsValid() {
		return fmt.Errorf("invalid role %q", u.Role)
	}
	return nil
}

// IsAdmin checks if the user has admin role.
func (u *User) IsAdmin() bool {
	return u.Role == string(RoleAdmin)
}

// GetRole returns the user's role as a UserRole type.
func (u *User) GetRole() UserRole {
	return UserRole(u.Role)
}

// UserSharePermission defines a user's permission for a specific share.
type UserSharePermission struct {
	UserID     string `gorm:"primaryKey;size:36" json:"user_id"`
	ShareID    string `gorm:"primaryKey;size:36" json:"share_id"`
	ShareName  string `gorm:"size:255" json:"share_name"`         // Denormalized for lookups
	Permission string `gorm:"not null;size:50" json:"permission"` // none, read, read-write, admin
}

// TableName returns the table name for UserSharePermission.
func (UserSharePermission) TableName() string {
	return "user_share_permissions"
}

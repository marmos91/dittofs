package models

import (
	"fmt"
	"time"
)

// Group represents a DittoFS group for organizing users and permissions.
type Group struct {
	ID          string    `gorm:"primaryKey;size:36" json:"id"`
	Name        string    `gorm:"uniqueIndex;not null;size:255" json:"name"`
	GID         *uint32   `gorm:"uniqueIndex" json:"gid,omitempty"`
	Description string    `gorm:"size:1024" json:"description,omitempty"`
	CreatedAt   time.Time `gorm:"autoCreateTime" json:"created_at"`

	// Many-to-many relationship with users
	Users []User `gorm:"many2many:user_groups;" json:"users,omitempty"`

	// One-to-many relationship with share permissions
	SharePermissions []GroupSharePermission `gorm:"foreignKey:GroupID" json:"share_permissions,omitempty"`
}

// TableName returns the table name for Group.
func (Group) TableName() string {
	return "groups"
}

// GetSharePermission returns the group's permission for a share.
// Returns PermissionNone if no permission is set for the share.
// Note: This method requires SharePermissions to be loaded with ShareName populated.
func (g *Group) GetSharePermission(shareName string) SharePermission {
	for _, p := range g.SharePermissions {
		if p.ShareName == shareName {
			return SharePermission(p.Permission)
		}
	}
	return PermissionNone
}

// Validate checks if the group has valid configuration.
func (g *Group) Validate() error {
	if g.Name == "" {
		return fmt.Errorf("group name is required")
	}
	return nil
}

// GroupSharePermission defines a group's permission for a specific share.
type GroupSharePermission struct {
	GroupID    string `gorm:"primaryKey;size:36" json:"group_id"`
	ShareID    string `gorm:"primaryKey;size:36" json:"share_id"`
	ShareName  string `gorm:"size:255" json:"share_name"`         // Denormalized for lookups
	Permission string `gorm:"not null;size:50" json:"permission"` // none, read, read-write, admin
}

// TableName returns the table name for GroupSharePermission.
func (GroupSharePermission) TableName() string {
	return "group_share_permissions"
}

// WellKnownGroups defines standard group names.
var WellKnownGroups = []string{
	"admins",
	"editors",
	"viewers",
}

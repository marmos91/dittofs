package identity

import (
	"fmt"
)

// Group represents a DittoFS group for organizing users and permissions.
//
// Groups are abstract permission containers without protocol-specific identity
// (no Unix GID or Windows SID). Protocol-specific identity is resolved per-share
// via ShareIdentityMapping.
//
// Groups can have share-level permissions that are inherited by all members.
// When a user belongs to multiple groups, the highest permission level wins.
type Group struct {
	// Name is the unique identifier for the group.
	Name string `json:"name" yaml:"name" mapstructure:"name"`

	// SharePermissions maps share names to permission levels.
	// All group members inherit these permissions.
	SharePermissions map[string]SharePermission `json:"share_permissions,omitempty" yaml:"share_permissions" mapstructure:"share_permissions"`

	// Description is an optional human-readable description of the group.
	Description string `json:"description,omitempty" yaml:"description,omitempty" mapstructure:"description"`
}

// GetSharePermission returns the group's permission for a share.
// Returns PermissionNone if no permission is set for the share.
func (g *Group) GetSharePermission(shareName string) SharePermission {
	if g.SharePermissions == nil {
		return PermissionNone
	}
	perm, ok := g.SharePermissions[shareName]
	if !ok {
		return PermissionNone
	}
	return perm
}

// Validate checks if the group has valid configuration.
func (g *Group) Validate() error {
	if g.Name == "" {
		return fmt.Errorf("group name is required")
	}
	for shareName, perm := range g.SharePermissions {
		if !perm.IsValid() {
			return fmt.Errorf("invalid permission %q for share %q", perm, shareName)
		}
	}
	return nil
}

// WellKnownGroups defines standard group names.
var WellKnownGroups = []string{
	"admins",
	"editors",
	"viewers",
}

package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Group represents a DittoFS group for organizing users and permissions.
//
// Groups can have share-level permissions that are inherited by all members.
// When a user belongs to multiple groups, the highest permission level wins.
type Group struct {
	// Name is the unique identifier for the group.
	Name string `yaml:"name" mapstructure:"name"`

	// GID is the Unix group ID.
	// Used for NFS group membership checks.
	GID uint32 `yaml:"gid" mapstructure:"gid"`

	// SID is the Windows Security Identifier for the group.
	// If empty, a SID will be auto-generated from the GID.
	SID string `yaml:"sid,omitempty" mapstructure:"sid"`

	// SharePermissions maps share names to permission levels.
	// All group members inherit these permissions.
	SharePermissions map[string]SharePermission `yaml:"share_permissions" mapstructure:"share_permissions"`

	// Description is an optional human-readable description of the group.
	Description string `yaml:"description,omitempty" mapstructure:"description"`
}

// GetSID returns the Windows SID, auto-generating one if not set.
//
// The auto-generated SID follows the format:
// S-1-5-21-dittofs-{hash}-{gid}
//
// This ensures consistent SID generation across restarts.
func (g *Group) GetSID() string {
	if g.SID != "" {
		return g.SID
	}
	return GenerateGroupSIDFromGID(g.GID)
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

// GenerateGroupSIDFromGID creates a deterministic Windows group SID from a Unix GID.
//
// Format: S-1-5-21-{dittofs-hash}-1-{gid}
// The "-1-" distinguishes group SIDs from user SIDs (which use "-0-").
func GenerateGroupSIDFromGID(gid uint32) string {
	// Use a hash of "dittofs" as the identifier authority
	h := sha256.Sum256([]byte("dittofs"))
	hashStr := hex.EncodeToString(h[:4])

	var subAuth1 uint32
	_, _ = fmt.Sscanf(hashStr, "%08x", &subAuth1)

	// S-1-5-21-{subAuth1}-1-{gid}
	// The "-1-" distinguishes group SIDs from user SIDs
	return fmt.Sprintf("S-1-5-21-%d-1-%d", subAuth1, gid)
}

// WellKnownGroups defines standard group names and GIDs.
var WellKnownGroups = map[string]uint32{
	"admins":  100,
	"editors": 101,
	"viewers": 102,
}

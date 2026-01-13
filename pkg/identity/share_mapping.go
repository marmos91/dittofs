package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// ShareIdentityMapping defines a user's Unix/Windows identity on a specific share.
// This enables protocol adapters (NFS, SMB) to resolve the correct UID/GID/SID
// for file operations on each share.
//
// Unlike the abstract User model which uses DittoFS groups, this struct provides
// the concrete protocol-specific identity mapping required for filesystem operations.
type ShareIdentityMapping struct {
	// Username is the DittoFS username this mapping belongs to.
	Username string `json:"username" yaml:"username" mapstructure:"username"`

	// ShareName is the share this mapping applies to (e.g., "/export").
	ShareName string `json:"share_name" yaml:"share_name" mapstructure:"share_name"`

	// Unix identity mapping
	// Used for NFS authentication and file ownership.

	// UID is the Unix user ID for this share.
	UID uint32 `json:"uid" yaml:"uid" mapstructure:"uid"`

	// GID is the primary Unix group ID for this share.
	GID uint32 `json:"gid" yaml:"gid" mapstructure:"gid"`

	// GIDs is a list of supplementary Unix group IDs for this share.
	GIDs []uint32 `json:"gids,omitempty" yaml:"gids,omitempty" mapstructure:"gids"`

	// Windows identity mapping
	// Used for SMB authentication.

	// SID is the Windows Security Identifier for this share.
	// If empty, a SID will be auto-generated from the UID.
	SID string `json:"sid,omitempty" yaml:"sid,omitempty" mapstructure:"sid"`

	// GroupSIDs is a list of Windows group Security Identifiers for this share.
	GroupSIDs []string `json:"group_sids,omitempty" yaml:"group_sids,omitempty" mapstructure:"group_sids"`
}

// GetSID returns the Windows SID, auto-generating one if not set.
//
// The auto-generated SID follows the format:
// S-1-5-21-dittofs-{hash(uid)}
//
// This ensures consistent SID generation across restarts.
func (m *ShareIdentityMapping) GetSID() string {
	if m.SID != "" {
		return m.SID
	}
	return GenerateSIDFromUID(m.UID)
}

// HasGID checks if the mapping has the specified GID in supplementary groups.
func (m *ShareIdentityMapping) HasGID(gid uint32) bool {
	if m.GID == gid {
		return true
	}
	for _, g := range m.GIDs {
		if g == gid {
			return true
		}
	}
	return false
}

// Validate checks if the mapping has valid configuration.
func (m *ShareIdentityMapping) Validate() error {
	if m.Username == "" {
		return fmt.Errorf("username is required")
	}
	if m.ShareName == "" {
		return fmt.Errorf("share name is required")
	}
	return nil
}

// GenerateSIDFromUID creates a deterministic Windows SID from a Unix UID.
//
// Format: S-1-5-21-{dittofs-hash}-{uid}
// The dittofs-hash provides a unique identifier authority.
func GenerateSIDFromUID(uid uint32) string {
	// Use a hash of "dittofs" as the identifier authority
	// This ensures our SIDs don't conflict with real Windows SIDs
	h := sha256.Sum256([]byte("dittofs"))
	hashStr := hex.EncodeToString(h[:4])

	// Parse first 4 bytes as uint32 for the sub-authority
	// Note: Sscanf cannot fail here because hashStr is always 8 hex chars from sha256
	var subAuth1 uint32
	_, _ = fmt.Sscanf(hashStr, "%08x", &subAuth1)

	// S-1-5-21-{subAuth1}-0-{uid}
	// 1 = NT Authority
	// 5 = NT Authority
	// 21 = Non-unique domain
	return fmt.Sprintf("S-1-5-21-%d-0-%d", subAuth1, uid)
}

// DefaultShareIdentityMapping creates a default mapping for a user on a share.
// This is used when no explicit mapping exists.
func DefaultShareIdentityMapping(username, shareName string, uid, gid uint32) *ShareIdentityMapping {
	return &ShareIdentityMapping{
		Username:  username,
		ShareName: shareName,
		UID:       uid,
		GID:       gid,
	}
}

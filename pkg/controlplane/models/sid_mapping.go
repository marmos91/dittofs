package models

import (
	"errors"
	"time"
)

// SIDMapping is a durable binding between a foreign (Active Directory / LDAP)
// domain SID and a stable DittoFS Unix UID/GID.
//
// Local/algorithmic SIDs (this machine's domain SID + an RID derived from the
// local UID via the Samba formula — see pkg/auth/sid/mapper.go) are NOT stored
// here: they are deterministically reproducible from the machine SID alone.
// Only FOREIGN SIDs of the form S-1-5-21-<ad-domain>-<rid>, which are not
// derivable from a local UID, require durable allocation.
//
// The mapping is keyed on the full domain SID and is allocated exactly once.
// Re-resolving an existing foreign SID MUST return the same UID/GID — never
// remap — because a remap would silently re-attribute files owned by the old
// UID to a different principal (a data-exposure / data-loss invariant).
//
// IsGroup distinguishes a group SID (→ GID) from a user SID (→ UID); a SID is
// allocated as one or the other based on the resolution context.
type SIDMapping struct {
	// SID is the canonical foreign domain SID string (e.g.
	// "S-1-5-21-111-222-333-1107"). Primary key — uniqueness is the
	// never-remap guarantee.
	SID string `gorm:"primaryKey;type:varchar(184)" json:"sid"`

	// UnixID is the allocated Unix identifier: a UID when IsGroup is false,
	// a GID when IsGroup is true.
	UnixID uint32 `gorm:"not null;index:idx_sid_mapping_unixid" json:"unix_id"`

	// IsGroup reports whether UnixID is a GID (true) or a UID (false).
	IsGroup bool `gorm:"not null;index:idx_sid_mapping_unixid" json:"is_group"`

	// DisplayName is an optional human-readable label (e.g. the AD
	// sAMAccountName) captured at allocation time for diagnostics.
	DisplayName string `gorm:"type:varchar(255)" json:"display_name,omitempty"`

	CreatedAt time.Time `gorm:"autoCreateTime" json:"created_at"`
}

// TableName returns the table name for SIDMapping.
func (SIDMapping) TableName() string {
	return "sid_mappings"
}

// Error types for SID mapping operations.
var (
	// ErrSIDMappingNotFound is returned when no mapping exists for a SID.
	ErrSIDMappingNotFound = errors.New("sid mapping not found")
)

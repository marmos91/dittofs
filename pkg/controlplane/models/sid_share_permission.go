package models

// SIDSharePermission grants a share permission directly to a Windows/AD security
// identifier (SID) — a domain user or group — WITHOUT a local DittoFS user or
// group object.
//
// This is the storage behind issue #1528: an operator grants access to an AD
// principal (resolved to its SID at grant time, or a raw SID) and, at login, a
// Kerberos-authenticated principal's PAC user/group SIDs are matched against
// these grants. Unlike UserSharePermission/GroupSharePermission — which are
// foreign-keyed to a local users/groups row — this record is keyed on the raw
// SID and has NO foreign key, so no local shadow object is ever needed.
//
// The primary key is (SID, ShareID): one permission level per principal per
// share, upserted on re-grant, exactly like the local grant tables.
type SIDSharePermission struct {
	// SID is the canonical Windows SID string (e.g.
	// "S-1-5-21-111-222-333-1107"). Part of the primary key.
	SID string `gorm:"column:sid;primaryKey;type:varchar(184)" json:"sid"`

	// ShareID is the share this grant applies to. Part of the primary key.
	ShareID string `gorm:"primaryKey;size:36" json:"share_id"`

	// ShareName is denormalized for lookups without a shares join, matching
	// UserSharePermission/GroupSharePermission.
	ShareName string `gorm:"size:255" json:"share_name"`

	// Permission is the granted level (none, read, read-write, admin).
	Permission string `gorm:"not null;size:50" json:"permission"`

	// IsGroup reports whether the SID is a group SID (matched against a login's
	// PAC group SIDs) or a user SID (matched against the user SID). It also
	// selects the NFS numeric projection: a group SID maps to a GID. No GORM
	// `default:` is set: a `default:true` would coerce an explicit Go false back
	// to true on INSERT (refs #514), silently turning a user-SID grant into a
	// group grant. Every writer sets this field explicitly.
	IsGroup bool `gorm:"not null" json:"is_group"`

	// UnixID is the Unix GID (group SID) or UID (user SID) this SID resolves to,
	// captured at grant time. NFS carries no SID on the wire, so it authorizes an
	// AD principal by matching this numeric id against the login's UID/GIDs — the
	// same id the LDAP provider derives at login (the RID in idmap=rid mode, or
	// the RFC2307 uidNumber/gidNumber). 0 means unresolved (e.g. a raw-SID grant
	// while LDAP is unavailable): the grant still works over SMB via the SID, but
	// has no NFS numeric projection.
	UnixID uint32 `gorm:"column:unix_id;not null;default:0" json:"unix_id"`

	// DisplayName is the human-readable principal captured at grant time (e.g.
	// "CUBBIT\\Cubbit" or "alice@cubbit.local"), for diagnostics and UI display.
	DisplayName string `gorm:"size:255" json:"display_name,omitempty"`
}

// TableName returns the table name for SIDSharePermission.
func (SIDSharePermission) TableName() string {
	return "sid_share_permissions"
}

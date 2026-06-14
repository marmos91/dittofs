package models

import (
	"encoding/json"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
)

// KerberosLevel constants for share security policy.
const (
	KerberosLevelKrb5  = "krb5"
	KerberosLevelKrb5i = "krb5i"
	KerberosLevelKrb5p = "krb5p"
)

// Share defines a DittoFS share/export configuration.
// Protocol-specific settings (NFS squash, SMB guest access, etc.) are stored
// in the share_adapter_configs table via ShareAdapterConfig.
type Share struct {
	ID                 string  `gorm:"primaryKey;size:36" json:"id"`
	Name               string  `gorm:"uniqueIndex;not null;size:255" json:"name"` // e.g., "/export"
	MetadataStoreID    string  `gorm:"not null;size:36" json:"metadata_store_id"`
	LocalBlockStoreID  string  `gorm:"not null;size:36" json:"local_block_store_id"`
	RemoteBlockStoreID *string `gorm:"size:36" json:"remote_block_store_id"`
	ReadOnly           bool    `gorm:"default:false" json:"read_only"`
	Enabled            bool    `gorm:"default:true;not null" json:"enabled"` // REST-02 gate: restore refuses if any share on the target store is still enabled.
	EncryptData        bool    `gorm:"default:false" json:"encrypt_data"`    // SMB3: set SMB2_SHAREFLAG_ENCRYPT_DATA in TREE_CONNECT
	// AclFlagInheritedCanonicalization controls whether the SMB CREATE/SET_INFO
	// Security path canonicalizes the SE_DACL_AUTO_INHERITED control bit per
	// MS-DTYP §2.5.3.4.2 (clearing it when AUTO_INHERIT_REQ is unset). Default
	// true matches Windows behavior; set to false to opt into the Samba
	// extension where the bit survives without AUTO_INHERIT_REQ (refs #514).
	AclFlagInheritedCanonicalization bool `gorm:"default:true;not null" json:"acl_flag_inherited_canonicalization"`
	// AccessBasedEnumeration enables Windows access-based enumeration on the
	// share (SHI1005_FLAGS_ACCESS_BASED_DIRECTORY_ENUM per MS-SRVS). When
	// true, TREE_CONNECT sets SMB2_SHAREFLAG_ACCESS_BASED_DIRECTORY_ENUM in
	// ShareFlags (MS-SMB2 §2.2.10) and QUERY_DIRECTORY hides entries the
	// caller cannot read. Default false matches the historical behaviour
	// (refs #532, #549).
	AccessBasedEnumeration bool `gorm:"default:false;not null" json:"access_based_enumeration"`
	// ChangeNotifyDisabled rejects SMB2 CHANGE_NOTIFY on this share with
	// STATUS_NOT_IMPLEMENTED. Mirrors Samba `kernel change notify = no` and
	// the smb2.change_notify_disabled torture test. Default false leaves
	// change notify enabled.
	ChangeNotifyDisabled bool `gorm:"default:false;not null" json:"change_notify_disabled"`
	// StreamsDisabled rejects SMB2 CREATE requests that reference an
	// Alternate Data Stream (named ADS, `::$DATA`, or any stream-type
	// suffix past the base path) with STATUS_OBJECT_NAME_INVALID.
	// Mirrors Samba `smbd:streams = no` and the
	// smb2.create_no_streams.no_stream torture test. Default false leaves
	// stream support enabled.
	StreamsDisabled bool `gorm:"default:false;not null" json:"streams_disabled"`
	// ContinuousAvailability advertises SMB2_SHARE_CAP_CONTINUOUS_AVAILABILITY
	// in the TREE_CONNECT response (MS-SMB2 §2.2.10) and allows SMB3 persistent
	// durable handles (DH2Q SMB2_DHANDLE_FLAG_PERSISTENT) on this share. When
	// false, a persistent-handle request degrades to a plain durable handle.
	// Default false (refs #739).
	ContinuousAvailability bool `gorm:"default:false;not null" json:"continuous_availability"`
	// AllowMFsymlink enables automatic conversion of 1067-byte XSym
	// (Minshall+French) symlink files written by macOS/Windows SMB clients into
	// real symlinks on CLOSE. The conversion target is client-controlled, so
	// promotion is opt-in. Default false (XSym files are stored as regular
	// files and never promoted).
	// Column pinned explicitly: GORM's default naming would mangle the
	// "MFsymlink" initialism into a different snake_case column than the
	// "allow_mfsymlink" the store field-map and backfill use.
	AllowMFsymlink bool `gorm:"column:allow_mfsymlink;default:false;not null" json:"allow_mfsymlink"`
	// TrashEnabled turns on the per-share recycle bin (#190). Default false.
	TrashEnabled bool `gorm:"default:false;not null" json:"trash_enabled"`
	// TrashRetentionDays auto-empties bin entries older than N days (0 = keep forever).
	TrashRetentionDays int `gorm:"default:0;not null" json:"trash_retention_days"`
	// TrashRestrictToAdmin limits empty/force-delete to admins (users may still restore).
	TrashRestrictToAdmin bool `gorm:"default:false;not null" json:"trash_restrict_to_admin"`
	// TrashMaxBytes caps total bin bytes (0 = unbounded); over-cap evicts oldest.
	TrashMaxBytes int64 `gorm:"default:0;not null" json:"trash_max_bytes"`
	// TrashExcludePatterns are globs that bypass the bin (immediate delete),
	// stored as a JSON array string (same encoding as BlockedOperations).
	TrashExcludePatterns string    `gorm:"type:text" json:"-"`
	DefaultPermission    string    `gorm:"default:read-write;size:50" json:"default_permission"`      // none, read, read-write, admin
	Config               string    `gorm:"type:text" json:"-"`                                        // JSON blob for additional share config
	BlockedOperations    string    `gorm:"type:text" json:"-"`                                        // JSON array of blocked operations
	RetentionPolicy      string    `gorm:"size:10;default:''" json:"retention_policy"`                // pin, ttl, lru (empty = LRU default)
	RetentionTTL         int64     `gorm:"default:0" json:"retention_ttl"`                            // TTL in seconds (0 = not set)
	LocalStoreSize       int64     `gorm:"default:0" json:"local_store_size"`                         // Per-share disk size override in bytes (0 = system default)
	ReadBufferSize       int64     `gorm:"default:0;column:read_buffer_size" json:"read_buffer_size"` // Read buffer override in bytes (0 = system default)
	QuotaBytes           int64     `gorm:"default:0;column:quota_bytes" json:"quota_bytes"`           // Per-share byte quota (0 = unlimited)
	CreatedAt            time.Time `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt            time.Time `gorm:"autoUpdateTime" json:"updated_at"`

	// Relationships
	MetadataStore    MetadataStoreConfig    `gorm:"foreignKey:MetadataStoreID" json:"metadata_store,omitempty"`
	LocalBlockStore  BlockStoreConfig       `gorm:"foreignKey:LocalBlockStoreID" json:"local_block_store,omitempty"`
	RemoteBlockStore *BlockStoreConfig      `gorm:"foreignKey:RemoteBlockStoreID" json:"remote_block_store"`
	AccessRules      []ShareAccessRule      `gorm:"foreignKey:ShareID" json:"access_rules,omitempty"`
	UserPermissions  []UserSharePermission  `gorm:"foreignKey:ShareID" json:"user_permissions,omitempty"`
	GroupPermissions []GroupSharePermission `gorm:"foreignKey:ShareID" json:"group_permissions,omitempty"`

	// Parsed configuration (not stored in DB)
	ParsedConfig map[string]any `gorm:"-" json:"config,omitempty"`
}

// TableName returns the table name for Share.
func (Share) TableName() string {
	return "shares"
}

// GetConfig returns the parsed additional configuration.
func (s *Share) GetConfig() (map[string]any, error) {
	if s.ParsedConfig != nil {
		return s.ParsedConfig, nil
	}
	if s.Config == "" {
		return make(map[string]any), nil
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(s.Config), &cfg); err != nil {
		return nil, err
	}
	s.ParsedConfig = cfg
	return cfg, nil
}

// SetConfig sets the additional configuration from a map.
func (s *Share) SetConfig(cfg map[string]any) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	s.Config = string(data)
	s.ParsedConfig = cfg
	return nil
}

// GetDefaultPermission returns the default permission as a SharePermission type.
func (s *Share) GetDefaultPermission() SharePermission {
	return ParseSharePermission(s.DefaultPermission)
}

// GetRetentionPolicy returns the parsed retention policy for this share.
// Empty or unset defaults to LRU for backward compatibility.
func (s *Share) GetRetentionPolicy() block.RetentionPolicy {
	p, err := block.ParseRetentionPolicy(s.RetentionPolicy)
	if err != nil {
		return block.RetentionLRU
	}
	return p
}

// GetRetentionTTL converts the stored TTL (seconds) to a time.Duration.
func (s *Share) GetRetentionTTL() time.Duration {
	return time.Duration(s.RetentionTTL) * time.Second
}

// ShareAccessRule defines client access rules for a share.
type ShareAccessRule struct {
	ID            string `gorm:"primaryKey;size:36" json:"id"`
	ShareID       string `gorm:"not null;size:36;index" json:"share_id"`
	RuleType      string `gorm:"not null;size:50" json:"rule_type"`       // allow, deny
	ClientPattern string `gorm:"not null;size:255" json:"client_pattern"` // IP/CIDR pattern
}

// TableName returns the table name for ShareAccessRule.
func (ShareAccessRule) TableName() string {
	return "share_access_rules"
}

// GetBlockedOps returns the blocked operations for this share as a string slice.
func (s *Share) GetBlockedOps() []string {
	return parseStringSlice(s.BlockedOperations)
}

// SetBlockedOps serializes the blocked operations from a string slice.
func (s *Share) SetBlockedOps(ops []string) {
	s.BlockedOperations = marshalStringSlice(ops)
}

// GetTrashExcludePatterns returns the recycle-bin exclude globs as a string slice.
func (s *Share) GetTrashExcludePatterns() []string {
	return parseStringSlice(s.TrashExcludePatterns)
}

// SetTrashExcludePatterns serializes the recycle-bin exclude globs to a JSON
// string for storage (same encoding as BlockedOperations).
func (s *Share) SetTrashExcludePatterns(patterns []string) {
	s.TrashExcludePatterns = marshalStringSlice(patterns)
}

package models

import (
	"encoding/json"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
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
	ID                 string    `gorm:"primaryKey;size:36" json:"id"`
	Name               string    `gorm:"uniqueIndex;not null;size:255" json:"name"` // e.g., "/export"
	MetadataStoreID    string    `gorm:"not null;size:36" json:"metadata_store_id"`
	LocalBlockStoreID  string    `gorm:"not null;size:36" json:"local_block_store_id"`
	RemoteBlockStoreID *string   `gorm:"size:36" json:"remote_block_store_id"`
	ReadOnly           bool      `gorm:"default:false" json:"read_only"`
	Enabled            bool      `gorm:"default:true;not null" json:"enabled"`                      // D-01/D-25. REST-02 gate: restore refuses if any share on the target store is still enabled.
	EncryptData        bool      `gorm:"default:false" json:"encrypt_data"`                         // SMB3: set SMB2_SHAREFLAG_ENCRYPT_DATA in TREE_CONNECT
	DefaultPermission  string    `gorm:"default:read-write;size:50" json:"default_permission"`      // none, read, read-write, admin
	Config             string    `gorm:"type:text" json:"-"`                                        // JSON blob for additional share config
	BlockedOperations  string    `gorm:"type:text" json:"-"`                                        // JSON array of blocked operations
	RetentionPolicy    string    `gorm:"size:10;default:''" json:"retention_policy"`                // pin, ttl, lru (empty = LRU default)
	RetentionTTL       int64     `gorm:"default:0" json:"retention_ttl"`                            // TTL in seconds (0 = not set)
	LocalStoreSize     int64     `gorm:"default:0" json:"local_store_size"`                         // Per-share disk size override in bytes (0 = system default)
	ReadBufferSize     int64     `gorm:"default:0;column:read_buffer_size" json:"read_buffer_size"` // Read buffer override in bytes (0 = system default)
	QuotaBytes         int64     `gorm:"default:0;column:quota_bytes" json:"quota_bytes"`           // Per-share byte quota (0 = unlimited)
	CreatedAt          time.Time `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt          time.Time `gorm:"autoUpdateTime" json:"updated_at"`

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
func (s *Share) GetRetentionPolicy() blockstore.RetentionPolicy {
	p, err := blockstore.ParseRetentionPolicy(s.RetentionPolicy)
	if err != nil {
		return blockstore.RetentionLRU
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

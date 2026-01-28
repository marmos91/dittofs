package models

import (
	"encoding/json"
	"time"
)

// Share defines a DittoFS share/export configuration.
type Share struct {
	ID                string    `gorm:"primaryKey;size:36" json:"id"`
	Name              string    `gorm:"uniqueIndex;not null;size:255" json:"name"` // e.g., "/export"
	MetadataStoreID   string    `gorm:"not null;size:36" json:"metadata_store_id"`
	PayloadStoreID    string    `gorm:"not null;size:36" json:"payload_store_id"`
	ReadOnly          bool      `gorm:"default:false" json:"read_only"`
	DefaultPermission string    `gorm:"default:read-write;size:50" json:"default_permission"` // none, read, read-write, admin
	Squash            string    `gorm:"default:root_to_admin;size:50" json:"squash"`          // Identity mapping mode
	Config            string    `gorm:"type:text" json:"-"`                                   // JSON blob for additional share config
	CreatedAt         time.Time `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt         time.Time `gorm:"autoUpdateTime" json:"updated_at"`

	// Anonymous UID/GID for squashing (used by root_to_guest and all_to_guest)
	AnonymousUID *uint32 `json:"anonymous_uid,omitempty"`
	AnonymousGID *uint32 `json:"anonymous_gid,omitempty"`

	// Guest access (per-share) - for SMB anonymous access
	GuestEnabled bool    `gorm:"default:false" json:"guest_enabled"`
	GuestUID     *uint32 `json:"guest_uid,omitempty"`
	GuestGID     *uint32 `json:"guest_gid,omitempty"`

	// Relationships
	MetadataStore    MetadataStoreConfig    `gorm:"foreignKey:MetadataStoreID" json:"metadata_store,omitempty"`
	PayloadStore     PayloadStoreConfig     `gorm:"foreignKey:PayloadStoreID" json:"payload_store,omitempty"`
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

// GetSquashMode returns the squash mode as a SquashMode type.
func (s *Share) GetSquashMode() SquashMode {
	return ParseSquashMode(s.Squash)
}

// GetDefaultPermission returns the default permission as a SharePermission type.
func (s *Share) GetDefaultPermission() SharePermission {
	return ParseSharePermission(s.DefaultPermission)
}

// GetAnonymousUID returns the anonymous UID, defaulting to 65534 (nobody).
func (s *Share) GetAnonymousUID() uint32 {
	if s.AnonymousUID != nil {
		return *s.AnonymousUID
	}
	return 65534 // nobody
}

// GetAnonymousGID returns the anonymous GID, defaulting to 65534 (nogroup).
func (s *Share) GetAnonymousGID() uint32 {
	if s.AnonymousGID != nil {
		return *s.AnonymousGID
	}
	return 65534 // nogroup
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

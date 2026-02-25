package models

import (
	"encoding/json"
	"time"
)

// ShareAdapterConfig stores per-adapter per-share typed JSON configuration.
// This decouples protocol-specific settings (NFS squash, SMB guest access, etc.)
// from the generic Share model, enabling each adapter to define its own config.
type ShareAdapterConfig struct {
	ID          string    `gorm:"primaryKey;size:36" json:"id"`
	ShareID     string    `gorm:"not null;size:36;uniqueIndex:idx_share_adapter" json:"share_id"`
	AdapterType string    `gorm:"not null;size:50;uniqueIndex:idx_share_adapter" json:"adapter_type"`
	Config      string    `gorm:"type:text" json:"config"`
	CreatedAt   time.Time `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt   time.Time `gorm:"autoUpdateTime" json:"updated_at"`
}

// TableName returns the table name for ShareAdapterConfig.
func (ShareAdapterConfig) TableName() string {
	return "share_adapter_configs"
}

// ParseConfig unmarshals the JSON config into the provided target.
func (c *ShareAdapterConfig) ParseConfig(target any) error {
	if c.Config == "" {
		return nil
	}
	return json.Unmarshal([]byte(c.Config), target)
}

// SetConfig marshals the provided source into the JSON config field.
func (c *ShareAdapterConfig) SetConfig(source any) error {
	data, err := json.Marshal(source)
	if err != nil {
		return err
	}
	c.Config = string(data)
	return nil
}

// NFSExportOptions contains NFS-specific export options for a share.
// These were previously stored directly on the Share model.
type NFSExportOptions struct {
	Squash             string  `json:"squash"`
	AnonymousUID       *uint32 `json:"anonymous_uid,omitempty"`
	AnonymousGID       *uint32 `json:"anonymous_gid,omitempty"`
	AllowAuthSys       bool    `json:"allow_auth_sys"`
	RequireKerberos    bool    `json:"require_kerberos"`
	MinKerberosLevel   string  `json:"min_kerberos_level"`
	NetgroupID         *string `json:"netgroup_id,omitempty"`
	DisableReaddirplus bool    `json:"disable_readdirplus"`
}

// DefaultNFSExportOptions returns the default NFS export options for a new share.
func DefaultNFSExportOptions() NFSExportOptions {
	return NFSExportOptions{
		Squash:           string(DefaultSquashMode),
		AllowAuthSys:     true,
		RequireKerberos:  false,
		MinKerberosLevel: KerberosLevelKrb5,
	}
}

// GetSquashMode returns the squash mode as a SquashMode type.
func (o *NFSExportOptions) GetSquashMode() SquashMode {
	return ParseSquashMode(o.Squash)
}

// GetAnonymousUID returns the anonymous UID, defaulting to 65534 (nobody).
func (o *NFSExportOptions) GetAnonymousUID() uint32 {
	if o.AnonymousUID != nil {
		return *o.AnonymousUID
	}
	return 65534 // nobody
}

// GetAnonymousGID returns the anonymous GID, defaulting to 65534 (nogroup).
func (o *NFSExportOptions) GetAnonymousGID() uint32 {
	if o.AnonymousGID != nil {
		return *o.AnonymousGID
	}
	return 65534 // nogroup
}

// SMBShareOptions contains SMB-specific share options.
// These were previously stored directly on the Share model.
type SMBShareOptions struct {
	GuestEnabled bool    `json:"guest_enabled"`
	GuestUID     *uint32 `json:"guest_uid,omitempty"`
	GuestGID     *uint32 `json:"guest_gid,omitempty"`
}

// DefaultSMBShareOptions returns the default SMB share options for a new share.
func DefaultSMBShareOptions() SMBShareOptions {
	return SMBShareOptions{
		GuestEnabled: false,
	}
}

package models

import (
	"encoding/json"
	"time"
)

// AdapterConfig defines a protocol adapter configuration.
type AdapterConfig struct {
	ID        string    `gorm:"primaryKey;size:36" json:"id"`
	Type      string    `gorm:"uniqueIndex;not null;size:50" json:"type"` // nfs, smb
	Enabled   bool      `gorm:"default:true" json:"enabled"`
	Port      int       `gorm:"default:0" json:"port"`
	Config    string    `gorm:"type:text" json:"-"` // JSON blob for adapter-specific config
	CreatedAt time.Time `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt time.Time `gorm:"autoUpdateTime" json:"updated_at"`

	// Parsed configuration (not stored in DB)
	ParsedConfig map[string]any `gorm:"-" json:"config,omitempty"`
}

// Default ports an adapter binds to when its configured port is 0. The adapter
// factory and the unchanged-listener check both resolve a zero port through
// these, so a reload compares resolved-against-resolved rather than a sentinel
// against a concrete port — keeping this the single source of truth.
const (
	DefaultNFSPort = 12049
	DefaultSMBPort = 12445
)

// DefaultPort returns the port an adapter of the given type binds to when its
// configured port is 0, or 0 when the type has no known default.
func DefaultPort(adapterType string) int {
	switch adapterType {
	case "nfs":
		return DefaultNFSPort
	case "smb":
		return DefaultSMBPort
	default:
		return 0
	}
}

// TableName returns the table name for AdapterConfig.
func (AdapterConfig) TableName() string {
	return "adapters"
}

// GetConfig returns the parsed configuration.
func (a *AdapterConfig) GetConfig() (map[string]any, error) {
	if a.ParsedConfig != nil {
		return a.ParsedConfig, nil
	}
	if a.Config == "" {
		return make(map[string]any), nil
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(a.Config), &cfg); err != nil {
		return nil, err
	}
	a.ParsedConfig = cfg
	return cfg, nil
}

// SetConfig sets the configuration from a map.
func (a *AdapterConfig) SetConfig(cfg map[string]any) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	a.Config = string(data)
	a.ParsedConfig = cfg
	return nil
}

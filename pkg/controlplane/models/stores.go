package models

import (
	"encoding/json"
	"time"
)

// MetadataStoreConfig defines a metadata store instance configuration.
type MetadataStoreConfig struct {
	ID        string    `gorm:"primaryKey;size:36" json:"id"`
	Name      string    `gorm:"uniqueIndex;not null;size:255" json:"name"`
	Type      string    `gorm:"not null;size:50" json:"type"` // memory, badger, postgres
	Config    string    `gorm:"type:text" json:"-"`           // JSON blob for type-specific config
	CreatedAt time.Time `gorm:"autoCreateTime" json:"created_at"`

	// Parsed configuration (not stored in DB)
	ParsedConfig map[string]any `gorm:"-" json:"config,omitempty"`
}

// TableName returns the table name for MetadataStoreConfig.
func (MetadataStoreConfig) TableName() string {
	return "metadata_stores"
}

// GetConfig returns the parsed configuration.
func (m *MetadataStoreConfig) GetConfig() (map[string]any, error) {
	if m.ParsedConfig != nil {
		return m.ParsedConfig, nil
	}
	if m.Config == "" {
		return make(map[string]any), nil
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(m.Config), &cfg); err != nil {
		return nil, err
	}
	m.ParsedConfig = cfg
	return cfg, nil
}

// SetConfig sets the configuration from a map.
func (m *MetadataStoreConfig) SetConfig(cfg map[string]any) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	m.Config = string(data)
	m.ParsedConfig = cfg
	return nil
}

// BlockStoreConfig defines a block store instance configuration.
type BlockStoreConfig struct {
	ID        string    `gorm:"primaryKey;size:36" json:"id"`
	Name      string    `gorm:"uniqueIndex:idx_block_store_name;not null;size:255" json:"name"`
	Type      string    `gorm:"not null;size:50" json:"type"` // memory, s3
	Config    string    `gorm:"type:text" json:"-"`           // JSON blob for type-specific config
	CreatedAt time.Time `gorm:"autoCreateTime" json:"created_at"`

	// Parsed configuration (not stored in DB)
	ParsedConfig map[string]any `gorm:"-" json:"config,omitempty"`
}

// TableName returns the table name for BlockStoreConfig.
func (BlockStoreConfig) TableName() string {
	return "block_store_configs"
}

// GetConfig returns the parsed configuration.
func (b *BlockStoreConfig) GetConfig() (map[string]any, error) {
	if b.ParsedConfig != nil {
		return b.ParsedConfig, nil
	}
	if b.Config == "" {
		return make(map[string]any), nil
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(b.Config), &cfg); err != nil {
		return nil, err
	}
	b.ParsedConfig = cfg
	return cfg, nil
}

// SetConfig sets the configuration from a map.
func (b *BlockStoreConfig) SetConfig(cfg map[string]any) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	b.Config = string(data)
	b.ParsedConfig = cfg
	return nil
}

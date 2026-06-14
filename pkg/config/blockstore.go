package config

import "fmt"

// BlockstoreConfig is the top-level container for blockstore-related
// tunables (currently blockstore.local.dedup_lru_size); additional
// layers (remote tier, cache tier) may be added in subsequent
// milestones.
type BlockstoreConfig struct {
	Local BlockstoreLocalConfig `mapstructure:"local" yaml:"local"`
}

// BlockstoreLocalConfig holds local-tier blockstore tunables.
type BlockstoreLocalConfig struct {
	// DedupLRUSize is the slot count for the in-memory hash dedup LRU.
	// Default 4096 when zero. Surface for the RAM-only per-share hash
	// LRU consulted between FastCDC.Next() and Put(hash, data) in
	// pkg/block/local/fs/rollup.go.
	DedupLRUSize int `mapstructure:"dedup_lru_size" yaml:"dedup_lru_size"`
}

// ApplyDefaults fills any zero-valued field with the defaults.
func (c *BlockstoreLocalConfig) ApplyDefaults() {
	if c.DedupLRUSize <= 0 {
		c.DedupLRUSize = 4096
	}
}

// Validate returns an error if the BlockstoreLocalConfig has invalid
// values. The error message includes the canonical dotted config path
// (blockstore.local.dedup_lru_size) so operators can pinpoint the
// offending key in their config file.
func (c *BlockstoreLocalConfig) Validate() error {
	if c.DedupLRUSize <= 0 {
		return fmt.Errorf("blockstore.local.dedup_lru_size must be > 0 (got %d)", c.DedupLRUSize)
	}
	return nil
}

// ApplyDefaults fans out defaults to every sub-tier.
func (c *BlockstoreConfig) ApplyDefaults() {
	c.Local.ApplyDefaults()
}

// Validate fans out validation to every sub-tier.
func (c *BlockstoreConfig) Validate() error {
	return c.Local.Validate()
}

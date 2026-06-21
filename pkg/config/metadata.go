package config

import "fmt"

// MetadataConfig configures global, engine-specific metadata-store tunables.
// These apply to every metadata store of the matching engine opened by the
// server (the per-store path/connection details still live in the control-plane
// database; this section carries operational knobs that are the same for every
// store of an engine on this node).
type MetadataConfig struct {
	// Badger configures the BadgerDB metadata engine's in-memory caches.
	Badger BadgerMetadataConfig `mapstructure:"badger" yaml:"badger"`
}

// BadgerMetadataConfig configures the BadgerDB metadata engine's block- and
// index-cache sizes (#1245 Bug D).
//
// BadgerDB's own defaults are tiny (256 MiB block, index cache disabled), which
// thrashes on a busy server: Badger logs "Block cache might be too small ...
// hit-ratio: 0.26 ... sets-rejected", every cold lookup hits disk, and the
// stalls widen the window for the dedup transaction-conflict race and the
// append-log "pressure wait timed out" path.
//
// Both sizes are in MiB. When a size is 0 (the default), it is NOT set to a
// fixed number — instead the badger engine AUTO-SIZES that cache as a fraction
// of the memory available to the process (≈15% for the block cache, ≈7.5% for
// the index cache), clamped to sane floors (512 MiB block / 256 MiB index) and
// ceilings (4 GiB block / 2 GiB index). So a 4 GiB host gets ≈614 MiB block /
// ≈307 MiB index automatically, and operators only set these to override the
// auto-sizing.
//
// Environment overrides follow the DITTOFS_METADATA_BADGER_* convention, e.g.
// DITTOFS_METADATA_BADGER_BLOCK_CACHE_MB=2048.
type BadgerMetadataConfig struct {
	// BlockCacheSizeMB is BadgerDB's block cache size in MiB. 0 = auto-size
	// from available RAM. Override: DITTOFS_METADATA_BADGER_BLOCK_CACHE_MB.
	BlockCacheSizeMB int64 `mapstructure:"block_cache_mb" yaml:"block_cache_mb"`

	// IndexCacheSizeMB is BadgerDB's index cache size in MiB. 0 = auto-size
	// from available RAM. Override: DITTOFS_METADATA_BADGER_INDEX_CACHE_MB.
	IndexCacheSizeMB int64 `mapstructure:"index_cache_mb" yaml:"index_cache_mb"`
}

// ApplyDefaults leaves both sizes at 0 by design: a zero value is the signal to
// the badger engine to RAM-relative auto-size (see BadgerMetadataConfig). It is
// present for symmetry with the other config sub-structs and so future fields
// have a home.
func (c *BadgerMetadataConfig) ApplyDefaults() {}

// ApplyDefaults fans out to the badger sub-section.
func (c *MetadataConfig) ApplyDefaults() {
	c.Badger.ApplyDefaults()
}

// Validate rejects negative cache sizes. Zero is valid (auto-size from
// available RAM); any positive value is accepted verbatim — an operator who
// sets an explicit size is trusted with the number.
func (c *BadgerMetadataConfig) Validate() error {
	if c.BlockCacheSizeMB < 0 {
		return fmt.Errorf("metadata.badger.block_cache_mb must be >= 0 (got %d); 0 auto-sizes from available RAM", c.BlockCacheSizeMB)
	}
	if c.IndexCacheSizeMB < 0 {
		return fmt.Errorf("metadata.badger.index_cache_mb must be >= 0 (got %d); 0 auto-sizes from available RAM", c.IndexCacheSizeMB)
	}
	return nil
}

// Validate fans out to the badger sub-section.
func (c *MetadataConfig) Validate() error {
	return c.Badger.Validate()
}

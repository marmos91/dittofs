package config

import (
	"fmt"
	"time"
)

// defaultRemoteCacheSize is the on-disk ceiling applied to a share's local
// tier when a remote block store is configured but no explicit
// LocalStoreSize / max_size is set. With a remote configured the local tier
// is a write-through cache, not durable storage, so it must be bounded to
// avoid filling the host volume on a fast-writer / slow-uploader. 10 GiB is
// a conservative default; operators raise it via the config key or override
// per-share with --local-store-size.
const defaultRemoteCacheSize uint64 = 10 << 30 // 10 GiB

// defaultBackpressureMaxWait is how long a write stalls waiting for the
// syncer to drain unsynced bytes (freeing cache space) before returning
// ErrDiskFull, when the remote is healthy and every local chunk is still
// unsynced. Separate from the LRU evict wait: this is the graceful-stall
// window for the remote-cache backpressure path.
const defaultBackpressureMaxWait = 60 * time.Second

// BlockstoreConfig is the top-level container for blockstore-related
// tunables; additional layers (remote tier, cache tier) may be added in
// subsequent milestones.
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

	// DefaultRemoteCacheSize is the on-disk ceiling (bytes) applied to a
	// share's local tier when a remote block store is configured but no
	// explicit per-share size is set. Bounds the write-through cache so a
	// fast writer cannot exhaust the host volume while the syncer lags.
	// Default 10 GiB when zero (ApplyDefaults). Local-only shares ignore
	// this — they keep their existing (system-deduced) local size.
	DefaultRemoteCacheSize uint64 `mapstructure:"default_remote_cache_size" yaml:"default_remote_cache_size"`

	// BackpressureMaxWait is how long a write blocks waiting for the syncer
	// to drain unsynced bytes (and free cache space) before returning
	// ErrDiskFull, when the remote is healthy but every local chunk is
	// still unsynced. Default 60s when zero. Distinct from the internal LRU
	// evict wait.
	BackpressureMaxWait time.Duration `mapstructure:"backpressure_max_wait" yaml:"backpressure_max_wait"`

	// MaxLogBytes is the per-share append-log pressure budget in bytes: the
	// on-disk append log buffers freshly-written bytes before the async
	// rollup folds them into CAS chunks, and AppendWrite stalls
	// (ErrPressureTimeout) once the buffered total exceeds this budget. This
	// is THE append-log backpressure lever. 0 means "use the system-deduced
	// default" (DeduceDefaults: 25% of RAM, floor 1 GiB). A per-share block
	// store config `max_log_bytes` overrides this global default for that
	// share; this global default in turn overrides the deduced default.
	MaxLogBytes uint64 `mapstructure:"max_log_bytes" yaml:"max_log_bytes"`
}

// ApplyDefaults fills any zero-valued field with the defaults.
func (c *BlockstoreLocalConfig) ApplyDefaults() {
	if c.DedupLRUSize <= 0 {
		c.DedupLRUSize = 4096
	}
	if c.DefaultRemoteCacheSize == 0 {
		c.DefaultRemoteCacheSize = defaultRemoteCacheSize
	}
	if c.BackpressureMaxWait <= 0 {
		c.BackpressureMaxWait = defaultBackpressureMaxWait
	}
}

// Validate returns an error if the BlockstoreLocalConfig has invalid
// values. The error message includes the canonical dotted config path so
// operators can pinpoint the offending key in their config file.
func (c *BlockstoreLocalConfig) Validate() error {
	if c.DedupLRUSize <= 0 {
		return fmt.Errorf("blockstore.local.dedup_lru_size must be > 0 (got %d)", c.DedupLRUSize)
	}
	// DefaultRemoteCacheSize, MaxLogBytes, and BackpressureMaxWait treat zero
	// as "apply the built-in (or system-deduced) default", so Validate only
	// rejects an explicitly negative backpressure wait — the one nonsensical
	// value a duration can take. MaxLogBytes is uint64 and so cannot be
	// negative; any positive value is honored as an explicit override.
	if c.BackpressureMaxWait < 0 {
		return fmt.Errorf("blockstore.local.backpressure_max_wait must be >= 0 (got %s)", c.BackpressureMaxWait)
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

package config

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/marmos91/dittofs/internal/pathutil"
)

// defaultRemoteCacheSize is the on-disk ceiling applied to a share's local
// tier when a remote block store is configured but no explicit
// JournalSize / max_size is set. With a remote configured the local tier
// is a write-through cache, not durable storage, so it must be bounded to
// avoid filling the host volume on a fast-writer / slow-uploader. 10 GiB is
// a conservative default; operators raise it via the config key or override
// per-share with --journal-size.
const defaultRemoteCacheSize uint64 = 10 << 30 // 10 GiB

// defaultBackpressureMaxWait is how long a write stalls waiting for the
// syncer to drain unsynced bytes (freeing cache space) before returning
// ErrDiskFull, when the remote is healthy and every local chunk is still
// unsynced. Separate from the LRU evict wait: this is the graceful-stall
// window for the remote-cache backpressure path.
const defaultBackpressureMaxWait = 60 * time.Second

// defaultBlockDirName is the subdirectory of the state directory that holds
// every share's journal when blockstore.journal.path is unset.
const defaultBlockDirName = "blocks"

// BlockstoreConfig is the top-level container for blockstore-related
// tunables; additional layers (remote tier, cache tier) may be added in
// subsequent milestones.
type BlockstoreConfig struct {
	Journal BlockstoreJournalConfig `mapstructure:"journal" yaml:"journal"`
}

// BlockstoreJournalConfig holds local-tier blockstore tunables.
type BlockstoreJournalConfig struct {
	// Path is the directory holding every share's local journal. Each share
	// gets its own subdirectory beneath it, so no two shares write into the
	// same directory and their I/O stays independent. A leading ~ is
	// expanded; the result must be absolute so the location can never
	// resolve against the server's working directory. Defaults to
	// <state dir>/blocks when unset.
	Path string `mapstructure:"path" yaml:"path"`

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

	// ChunkSize is the FastCDC minimum chunk size in bytes, the dominant knob
	// for effective chunk size and so for random-read amplification. Average
	// and maximum are derived from it (4x/8x) unless ChunkMax overrides the
	// ceiling. Lower it (131072 = 128 KiB) for random-access workloads such as
	// VM images or databases: weaker dedup and more manifest rows, but far less
	// read amplification. Reads never re-chunk, so a change affects only newly
	// written data. 0 keeps the built-in profile.
	ChunkSize uint64 `mapstructure:"chunk_size" yaml:"chunk_size"`

	// ChunkMax overrides the derived maximum chunk size in bytes. 0 keeps the
	// value derived from ChunkSize.
	ChunkMax uint64 `mapstructure:"chunk_max" yaml:"chunk_max"`

	// DirtyExpire is how long a write may sit unflushed before the journal
	// fsyncs it, bounding what a crash can lose. Negative disables the timer,
	// leaving segment rotation as the only durability point. 0 keeps the
	// journal's own default.
	DirtyExpire time.Duration `mapstructure:"dirty_expire" yaml:"dirty_expire"`

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
func (c *BlockstoreJournalConfig) ApplyDefaults() {
	if c.Path == "" {
		c.Path = filepath.Join(GetStateDir(), defaultBlockDirName)
	}
	// A ~ that cannot be expanded (no home directory) is left as-is and
	// fails Validate's absolute-path check rather than being silently
	// resolved against the working directory.
	if expanded, err := pathutil.ExpandPath(c.Path); err == nil {
		c.Path = expanded
	}
	if c.DefaultRemoteCacheSize == 0 {
		c.DefaultRemoteCacheSize = defaultRemoteCacheSize
	}
	if c.BackpressureMaxWait <= 0 {
		c.BackpressureMaxWait = defaultBackpressureMaxWait
	}
}

// Validate returns an error if the BlockstoreJournalConfig has invalid
// values. The error message includes the canonical dotted config path so
// operators can pinpoint the offending key in their config file.
func (c *BlockstoreJournalConfig) Validate() error {
	// DefaultRemoteCacheSize, MaxLogBytes, and BackpressureMaxWait treat zero
	// as "apply the built-in (or system-deduced) default", so Validate only
	// rejects an explicitly negative backpressure wait — the one nonsensical
	// value a duration can take. MaxLogBytes is uint64 and so cannot be
	// negative; any positive value is honored as an explicit override.
	if c.BackpressureMaxWait < 0 {
		return fmt.Errorf("blockstore.journal.backpressure_max_wait must be >= 0 (got %s)", c.BackpressureMaxWait)
	}
	// Empty means "apply the default" and is only reachable before
	// ApplyDefaults; a value the operator did set must be absolute.
	if c.Path != "" && !filepath.IsAbs(c.Path) {
		return fmt.Errorf("blockstore.journal.path must be an absolute directory (got %q)", c.Path)
	}
	return nil
}

// ApplyDefaults fans out defaults to every sub-tier.
func (c *BlockstoreConfig) ApplyDefaults() {
	c.Journal.ApplyDefaults()
}

// Validate fans out validation to every sub-tier.
func (c *BlockstoreConfig) Validate() error {
	return c.Journal.Validate()
}

package shares

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block/chunker"
	"github.com/marmos91/dittofs/pkg/block/journal"
)

// ShareJournalDir returns where a share's local storage lives beneath the
// server's journal root.
//
// The share name is sanitized because it arrives from an operator and reaches
// the filesystem here; the layout keeps the historical `shares/<name>` level so
// a root adopted from an existing install addresses the same directories it
// always did.
func ShareJournalDir(root, shareName string) string {
	if root == "" {
		return ""
	}
	return filepath.Join(root, "shares", sanitizeShareName(shareName))
}

// OpenShareJournal opens (or recovers) a share's journal beneath the server's
// journal root.
//
// It replaces reading a path out of a per-share block store config: every share
// roots under one configured directory and gets its own subdirectory, so two
// shares never share a directory and their I/O stays independent.
func OpenShareJournal(shareName string, defaults *LocalStoreDefaults) (*journal.Store, error) {
	if defaults == nil || defaults.JournalRoot == "" {
		return nil, fmt.Errorf("no journal root configured; set blockstore.journal.path")
	}
	shareDir := ShareJournalDir(defaults.JournalRoot, shareName)
	if err := os.MkdirAll(shareDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create share directory: %w", err)
	}

	var cfg journal.Config
	cfg.EvictMaxWait = defaults.BackpressureMaxWait
	cfg.DirtyExpiry = clampDirtyExpire(defaults.DirtyExpire)
	return openJournalStore(shareDir, int64(defaults.MaxSize), defaults.MaxLogBytes, cfg)
}

// journalChunkParams derives the FastCDC profile from the configured minimum
// chunk size, with the maximum overridable on its own. ok is false when nothing
// usable is configured, leaving the built-in profile in force.
//
// The profile rides the syncer rather than the journal: the journal's seam is
// content-agnostic and has no use for it.
//
// An invalid combination is warned about and dropped rather than applied: a
// profile that fails validation would otherwise cut every newly written block
// to the wrong size, and reads never re-chunk, so the damage would outlive the
// misconfiguration.
func journalChunkParams(defaults *LocalStoreDefaults) (chunker.Params, bool) {
	if defaults.ChunkSize == 0 {
		return chunker.Params{}, false
	}
	n := int(defaults.ChunkSize)
	cp := chunker.Params{Min: n, Avg: n * 4, Max: n * 8}
	if defaults.ChunkMax > 0 {
		cp.Max = int(defaults.ChunkMax)
		// A ceiling below the derived average lowers the average to meet it
		// rather than invalidating the whole profile: the operator asked for
		// smaller chunks, which is satisfiable.
		if cp.Avg > cp.Max {
			cp.Avg = cp.Max
		}
	}
	if err := cp.Validate(); err != nil {
		logger.Warn("blockstore.journal chunk settings are invalid; keeping the default profile",
			"chunk_size", defaults.ChunkSize, "chunk_max", defaults.ChunkMax, "error", err)
		return chunker.Params{}, false
	}
	return cp, true
}

// clampDirtyExpire holds a configured commit interval at the floor below which
// it stops being a tuning choice: a sub-second interval issues disk barriers
// faster than the disk retires them, and a typo must not do that. A negative
// value disables the timer deliberately and passes through.
func clampDirtyExpire(d time.Duration) time.Duration {
	if d > 0 && d < minDirtyExpire {
		logger.Warn("blockstore.journal dirty_expire is below the floor; clamping",
			"dirty_expire", d, "floor", minDirtyExpire)
		return minDirtyExpire
	}
	return d
}

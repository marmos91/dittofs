package badger

import (
	"sync"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/dgraph-io/badger/v4/options"

	"github.com/marmos91/dittofs/internal/sysinfo"
)

// Badger cache sizing (#1245 Bug D).
//
// BadgerDB keeps two in-memory caches that dominate metadata read performance:
//
//   - the BLOCK cache, holding decompressed/decrypted LSM-tree data blocks, and
//   - the INDEX cache, holding the block-offset indices used to locate keys.
//
// Badger's own defaults are tiny (256 MiB block, 0 index — index cache disabled
// by default). On a host serving hundreds of concurrent NFS/SMB operations over
// a large directory tree this thrashes: Badger logs "Block cache might be too
// small ... hit-ratio: 0.26 ... sets-rejected" and every cold lookup walks the
// LSM tree from disk. The thrashing also widens the window for the dedup
// transaction-conflict race and the append-log "pressure wait timed out" path,
// because metadata operations stall on disk I/O instead of returning from cache.
//
// We fix this in two ways:
//
//  1. The sizes are CONFIGURABLE — explicitly via BadgerMetadataStoreConfig
//     (sourced from the per-store config map and the global DITTOFS_* /
//     config.yaml metadata.badger.* keys).
//
//  2. When NOT explicitly configured, the sizes AUTO-SCALE with the memory
//     available to the process (autoSizeCacheMB), so a 4 GiB host gets a
//     usefully large cache without any tuning, while a large host gets more.

const (
	// blockCacheMemFraction is the fraction of process-available memory
	// allocated to Badger's block cache when no explicit size is set.
	// Conservative: the process also holds the append-log, the working set,
	// and (on metadata stores fronting remote block stores) read buffers.
	blockCacheMemFraction = 0.15

	// indexCacheMemFraction is the fraction of process-available memory
	// allocated to Badger's index cache when no explicit size is set. Indices
	// are smaller than data blocks, so this is half the block-cache fraction.
	indexCacheMemFraction = 0.075

	// minBlockCacheMB / minIndexCacheMB are absolute floors (in MiB). These sit
	// well above Badger's own tiny default so even a memory-detection failure
	// (which falls back to the 4 GiB sysinfo default) yields a usefully large
	// cache. They also guarantee a non-zero block cache, which Badger REQUIRES
	// whenever compression or encryption is enabled.
	minBlockCacheMB int64 = 512
	minIndexCacheMB int64 = 256

	// maxBlockCacheMB / maxIndexCacheMB cap the auto-sized caches so a very
	// large host does not hand an unbounded slice of RAM to a single metadata
	// store. Operators who genuinely want more set the sizes explicitly.
	maxBlockCacheMB int64 = 4096
	maxIndexCacheMB int64 = 2048
)

// globalCacheDefaults holds operator-configured cache sizes (in MiB) sourced
// from the global config (metadata.badger.*). They are applied by
// NewBadgerMetadataStoreWithDefaults / the default-options path when the
// per-store config does not set its own sizes. A zero value means "unset —
// fall through to RAM-relative auto-sizing".
var globalCacheDefaults struct {
	mu           sync.RWMutex
	blockCacheMB int64
	indexCacheMB int64
}

// SetGlobalBadgerCacheDefaults records operator-configured Badger cache sizes
// (in MiB) sourced from the global DITTOFS_* / config.yaml metadata.badger.*
// keys. These become the default for every badger metadata store opened
// afterwards that does not specify its own BlockCacheSizeMB / IndexCacheSizeMB.
//
// A zero value for either size means "not configured" — that dimension then
// falls through to RAM-relative auto-sizing (autoSizeCacheMB). Safe for
// concurrent use; call once at startup before opening stores.
func SetGlobalBadgerCacheDefaults(blockCacheMB, indexCacheMB int64) {
	globalCacheDefaults.mu.Lock()
	globalCacheDefaults.blockCacheMB = blockCacheMB
	globalCacheDefaults.indexCacheMB = indexCacheMB
	globalCacheDefaults.mu.Unlock()
}

func getGlobalBadgerCacheDefaults() (blockCacheMB, indexCacheMB int64) {
	globalCacheDefaults.mu.RLock()
	defer globalCacheDefaults.mu.RUnlock()
	return globalCacheDefaults.blockCacheMB, globalCacheDefaults.indexCacheMB
}

// autoSizeCacheMB returns RAM-relative Badger block- and index-cache sizes (in
// MiB) for a host with availMem bytes of process-available memory.
//
// Formula: each cache is a fixed fraction of available memory
// (blockCacheMemFraction / indexCacheMemFraction), then clamped to the
// [min, max]MB bounds. The fractions are deliberately conservative because the
// same process also holds the append-log, the metadata working set, and read
// buffers. The floors keep small hosts (and the memory-detection fallback) well
// above Badger's tiny default; the ceilings stop a very large host from handing
// a single store an unbounded slice of RAM.
//
// Example: on a 4 GiB host neither cache hits a bound — block = 4096*0.15 ≈
// 614 MiB, index = 4096*0.075 ≈ 307 MiB.
func autoSizeCacheMB(availMem uint64) (blockCacheMB, indexCacheMB int64) {
	availMB := int64(availMem >> 20)
	blockCacheMB = clampMB(int64(float64(availMB)*blockCacheMemFraction), minBlockCacheMB, maxBlockCacheMB)
	indexCacheMB = clampMB(int64(float64(availMB)*indexCacheMemFraction), minIndexCacheMB, maxIndexCacheMB)
	return blockCacheMB, indexCacheMB
}

// clampMB clamps v to the inclusive [lo, hi] range.
func clampMB(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// resolveCacheSizesMB resolves the effective Badger block- and index-cache
// sizes (in MiB) for a store, applying the precedence:
//
//  1. explicit per-store config (config.BlockCacheSizeMB / IndexCacheSizeMB),
//  2. operator global config (SetGlobalBadgerCacheDefaults),
//  3. RAM-relative auto-sizing (autoSizeCacheMB) for whichever dimension is
//     still zero.
//
// Each dimension is resolved independently, so an operator can pin the block
// cache while letting the index cache auto-scale (or vice versa). availMem is
// the process-available memory in bytes (from sysinfo).
func resolveCacheSizesMB(config BadgerMetadataStoreConfig, availMem uint64) (blockCacheMB, indexCacheMB int64) {
	blockCacheMB = config.BlockCacheSizeMB
	indexCacheMB = config.IndexCacheSizeMB

	if blockCacheMB <= 0 || indexCacheMB <= 0 {
		gBlock, gIndex := getGlobalBadgerCacheDefaults()
		if blockCacheMB <= 0 && gBlock > 0 {
			blockCacheMB = gBlock
		}
		if indexCacheMB <= 0 && gIndex > 0 {
			indexCacheMB = gIndex
		}
	}

	if blockCacheMB <= 0 || indexCacheMB <= 0 {
		autoBlock, autoIndex := autoSizeCacheMB(availMem)
		if blockCacheMB <= 0 {
			blockCacheMB = autoBlock
		}
		if indexCacheMB <= 0 {
			indexCacheMB = autoIndex
		}
	}

	return blockCacheMB, indexCacheMB
}

// detectAvailableMemory returns the memory available to this process in bytes.
// Indirected through a package var so tests can pin a deterministic figure.
var detectAvailableMemory = func() uint64 {
	return sysinfo.NewDetector().AvailableMemory()
}

// buildBadgerOptions constructs the badger.Options used to open a metadata
// store. It is a pure function of its inputs (the store config and the
// process-available memory figure), which makes the option construction — in
// particular the resolved BlockCacheSize / IndexCacheSize threading — directly
// testable without opening a database.
//
// When config.BadgerOptions is non-nil it is used verbatim (the operator has
// taken full control of Badger tuning); only the unconditional SyncWrites
// override is layered on by the caller. Otherwise sensible metadata-workload
// defaults are applied and the cache sizes are resolved via resolveCacheSizesMB.
func buildBadgerOptions(config BadgerMetadataStoreConfig, availMem uint64) badger.Options {
	if config.BadgerOptions != nil {
		return *config.BadgerOptions
	}

	opts := badger.DefaultOptions(config.DBPath)

	// Optimize for metadata workload:
	// - Frequent small reads/writes (file attributes, directory entries)
	// - Range scans for directory listings (READDIR operations)
	// - Concurrent access from multiple NFS clients
	// - Large working set from directory scanning (Finder, ls -R, etc.)
	// - High cache hit ratio critical for performance
	opts = opts.WithLoggingLevel(badger.WARNING) // Reduce log noise
	opts = opts.WithCompression(options.None)    // Metadata is small, compression overhead not worth it

	// Resolve cache sizes with precedence: explicit > global config >
	// RAM-relative auto-sizing (see resolveCacheSizesMB / #1245 Bug D). The
	// resolved sizes are always > 0 (floors enforced), which also satisfies
	// Badger's requirement that the block cache be non-zero under
	// compression/encryption.
	blockCacheMB, indexCacheMB := resolveCacheSizesMB(config, availMem)

	opts = opts.WithBlockCacheSize(blockCacheMB << 20) // MiB -> bytes
	opts = opts.WithIndexCacheSize(indexCacheMB << 20) // MiB -> bytes

	return opts
}

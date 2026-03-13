package local

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// PendingBlock represents a block ready for upload to the remote block store.
type PendingBlock struct {
	// BlockIndex is the flat block index (fileOffset / BlockSize).
	BlockIndex uint64

	// Data is the block content.
	Data []byte

	// DataSize is the actual size of valid data in the block.
	DataSize uint32

	// Hash is the SHA-256 content hash; zero means not yet computed.
	Hash blockstore.ContentHash
}

// FlushedBlock records info about a block that was just flushed from memory to disk.
// Used by GetDirtyBlocks to avoid a round-trip (write then read back).
type FlushedBlock struct {
	// BlockIndex is the flat block index.
	BlockIndex uint64

	// CachePath is the path to the .blk file on disk.
	CachePath string

	// DataSize is the actual size of valid data in the block.
	DataSize uint32
}

// Stats contains cache statistics for observability.
type Stats struct {
	DiskUsed      int64 // Current total size of on-disk cached data in bytes
	MaxDisk       int64 // Configured maximum disk cache size (0 = unlimited)
	MemUsed       int64 // Current in-memory dirty buffer usage in bytes
	MaxMemory     int64 // Configured memory budget for dirty buffers
	FileCount     int   // Number of files with cached data
	MemBlockCount int   // Number of in-memory dirty blocks
}

// LocalStore is the interface for on-node block caching.
// It manages the two-tier (memory + disk) cache that sits between
// protocol adapters and the remote block store.
type LocalStore interface {
	// --- Read operations ---

	// ReadAt reads data from the cache at the specified offset into dest.
	// Returns (true, nil) if all requested bytes were found in cache,
	// (false, nil) on cache miss for any block in the range.
	ReadAt(ctx context.Context, payloadID string, dest []byte, offset uint64) (bool, error)

	// GetFileSize returns the cached file size and whether the file is tracked.
	// This is a fast in-memory lookup -- no disk or store access.
	GetFileSize(ctx context.Context, payloadID string) (uint64, bool)

	// IsBlockCached checks if a specific block is available in cache (memory or disk).
	IsBlockCached(ctx context.Context, payloadID string, blockIdx uint64) bool

	// GetBlockData returns the raw data for a specific block, checking memory first
	// then disk. Returns data, dataSize, and error.
	GetBlockData(ctx context.Context, payloadID string, blockIdx uint64) ([]byte, uint32, error)

	// --- Write operations ---

	// WriteAt writes data to the cache at the specified offset.
	WriteAt(ctx context.Context, payloadID string, data []byte, offset uint64) error

	// WriteFromRemote caches data fetched from the remote block store.
	// The block is marked Remote since it already exists remotely.
	WriteFromRemote(ctx context.Context, payloadID string, data []byte, offset uint64) error

	// --- Flush operations ---

	// Flush writes all dirty in-memory blocks for a file to disk as .blk files.
	// Returns the list of blocks that were flushed.
	Flush(ctx context.Context, payloadID string) ([]FlushedBlock, error)

	// GetDirtyBlocks flushes all in-memory blocks for a file to disk, then returns
	// all blocks in Local state as PendingBlocks ready for upload.
	GetDirtyBlocks(ctx context.Context, payloadID string) ([]PendingBlock, error)

	// SyncFileBlocks persists all queued FileBlock metadata updates to the store.
	SyncFileBlocks(ctx context.Context)

	// SyncFileBlocksForFile persists queued FileBlock metadata only for blocks
	// belonging to the given payloadID.
	SyncFileBlocksForFile(ctx context.Context, payloadID string)

	// --- Lifecycle and management ---

	// Start launches background goroutines (e.g., periodic metadata persistence).
	Start(ctx context.Context)

	// Close flushes pending metadata and marks the cache as closed.
	Close() error

	// Truncate discards cached blocks beyond newSize.
	Truncate(ctx context.Context, payloadID string, newSize uint64) error

	// EvictMemory removes all cached data (memory and disk tracking) for a file.
	EvictMemory(ctx context.Context, payloadID string) error

	// DeleteBlockFile removes a single block from memory, disk, and metadata.
	DeleteBlockFile(ctx context.Context, payloadID string, blockIdx uint64) error

	// DeleteAllBlockFiles removes all blocks for a file from memory, disk, and metadata.
	DeleteAllBlockFiles(ctx context.Context, payloadID string) error

	// TruncateBlockFiles removes all blocks whose start offset >= newSize.
	TruncateBlockFiles(ctx context.Context, payloadID string, newBlockCount uint64) error

	// SetSkipFsync disables fsync in Flush() for S3 backends.
	SetSkipFsync(skip bool)

	// SetEvictionEnabled controls whether the cache can evict blocks to make room.
	SetEvictionEnabled(enabled bool)

	// SetRetentionPolicy updates the cache retention policy for eviction decisions.
	//   - pin: never evict cached blocks
	//   - ttl: evict only after file last-access exceeds ttl duration
	//   - lru: evict least-recently-accessed blocks first (default)
	SetRetentionPolicy(policy blockstore.RetentionPolicy, ttl time.Duration)

	// Stats returns a snapshot of current cache statistics.
	Stats() Stats

	// ListFiles returns the payloadIDs of all files currently tracked in the cache.
	ListFiles() []string

	// MarkBlockRemote marks a block as confirmed in the remote block store.
	MarkBlockRemote(ctx context.Context, payloadID string, blockIdx uint64) bool

	// MarkBlockSyncing claims a block for sync to remote (Local -> Syncing).
	MarkBlockSyncing(ctx context.Context, payloadID string, blockIdx uint64) bool

	// MarkBlockLocal reverts a block to Local state after a failed sync attempt.
	MarkBlockLocal(ctx context.Context, payloadID string, blockIdx uint64) bool

	// GetStoredFileSize returns the total stored data size for a file by summing
	// the DataSize of all FileBlock records in the metadata store.
	GetStoredFileSize(ctx context.Context, payloadID string) (uint64, error)

	// ExistsOnDisk checks if a specific block is present on disk.
	ExistsOnDisk(ctx context.Context, payloadID string, blockIdx uint64) (bool, error)
}

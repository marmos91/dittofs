package local

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/health"
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

	// LocalPath is the path to the .blk file on disk.
	LocalPath string

	// DataSize is the actual size of valid data in the block.
	DataSize uint32
}

// Stats contains local store statistics for observability.
type Stats struct {
	DiskUsed      int64 // Current total size of on-disk block data in bytes
	MaxDisk       int64 // Configured maximum disk size (0 = unlimited)
	MemUsed       int64 // Current in-memory dirty buffer usage in bytes
	MaxMemory     int64 // Configured memory budget for dirty buffers
	FileCount     int   // Number of files with local data
	MemBlockCount int   // Number of in-memory dirty blocks
}

// LocalStore is the interface for on-node block storage.
// It manages the two-tier (memory + disk) store that sits between
// protocol adapters and the remote block store.
type LocalStore interface {
	// --- Read operations ---

	// ReadAt reads data from the local store at the specified offset into dest.
	// Returns (true, nil) if all requested bytes were found locally,
	// (false, nil) on miss for any block in the range.
	ReadAt(ctx context.Context, payloadID string, dest []byte, offset uint64) (bool, error)

	// GetFileSize returns the tracked file size and whether the file is tracked.
	// This is a fast in-memory lookup -- no disk or store access.
	GetFileSize(ctx context.Context, payloadID string) (uint64, bool)

	// IsBlockLocal checks if a specific block is available locally (memory or disk).
	IsBlockLocal(ctx context.Context, payloadID string, blockIdx uint64) bool

	// GetBlockData returns the raw data for a specific block, checking memory first
	// then disk. Returns data, dataSize, and error.
	GetBlockData(ctx context.Context, payloadID string, blockIdx uint64) ([]byte, uint32, error)

	// --- Write operations ---

	// WriteAt writes data to the local store at the specified offset.
	WriteAt(ctx context.Context, payloadID string, data []byte, offset uint64) error

	// WriteFromRemote stores data fetched from the remote block store locally.
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

	// Close flushes pending metadata and marks the store as closed.
	Close() error

	// Truncate discards local blocks beyond newSize.
	Truncate(ctx context.Context, payloadID string, newSize uint64) error

	// EvictMemory removes all in-memory data and disk tracking for a file.
	EvictMemory(ctx context.Context, payloadID string) error

	// DeleteBlockFile removes a single block from memory, disk, and metadata.
	DeleteBlockFile(ctx context.Context, payloadID string, blockIdx uint64) error

	// DeleteAllBlockFiles removes all blocks for a file from memory, disk, and metadata.
	DeleteAllBlockFiles(ctx context.Context, payloadID string) error

	// TruncateBlockFiles removes all blocks whose start offset >= newSize.
	TruncateBlockFiles(ctx context.Context, payloadID string, newBlockCount uint64) error

	// SetSkipFsync disables fsync in Flush() for S3 backends.
	SetSkipFsync(skip bool)

	// SetEvictionEnabled controls whether the local store can evict blocks to make room.
	SetEvictionEnabled(enabled bool)

	// SetRetentionPolicy updates the retention policy for eviction decisions.
	//   - pin: never evict local blocks
	//   - ttl: evict only after file last-access exceeds ttl duration
	//   - lru: evict least-recently-accessed blocks first (default)
	SetRetentionPolicy(policy blockstore.RetentionPolicy, ttl time.Duration)

	// Stats returns a snapshot of current local store statistics.
	Stats() Stats

	// ListFiles returns the payloadIDs of all files currently tracked in the local store.
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

	// Healthcheck returns the current health of the local store as a
	// structured [health.Report]. Implementations must satisfy
	// [health.Checker] so the upstream API layer can wrap them with a
	// [health.CachedChecker] and serve /status routes.
	//
	// Implementations should be cheap to call (no full directory scans,
	// no large I/O) and idempotent. The expectation is something on the
	// order of a stat() and possibly a write probe — see
	// fs.FSStore.Healthcheck for the canonical pattern.
	Healthcheck(ctx context.Context) health.Report
}

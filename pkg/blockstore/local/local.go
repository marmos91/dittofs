package local

import (
	"context"
	"iter"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/health"
)

// FlushedBlock records info about a block that was just flushed from
// memory to disk. Used by GetDirtyBlocks to avoid a round-trip (write
// then read back).
//
// Phase 17: preserved as the return type of the transitional Flush
// method below. Slated for deletion in Phase 18 when the Syncer
// rewrite eliminates the only consumer (engine/syncer.go).
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

// LocalStore is the host-side admin interface for the on-node block
// store. It EMBEDS [blockstore.BlockStoreAppend] (the byte-access +
// append-log surface tested by
// [blockstoretest.BlockStoreAppendConformance]) and adds lifecycle,
// eviction, retention, and observability methods that are caller-
// visible only from within the daemon process.
//
// A small transitional admin-superset (ReadAt / WriteAt / Flush /
// IsBlockLocal / GetBlockData / WriteFromRemote / DeleteAllBlockFiles)
// is retained through Phase 17 and slated for deletion in Phase 18
// (Syncer rewrite). Each transitional method carries a
// "Deprecated: removed in Phase 18" godoc tag so the cleanup wave can
// find every site via grep. Their consumers live in the engine
// (engine.go, fetch.go, syncer.go, upload.go, dedup.go); Phase 17
// could not rewrite those sites within D-01's atomic-merge
// constraint (every commit must `go build ./...`-clean) — Phase
// 18's Syncer simplification rewrites them onto BlockStore.Put / Get
// / Walk and deletes the transitional methods in a single coherent
// change.
type LocalStore interface {
	// Embedding contributes Put, Get, GetRange, Has, Delete, Head,
	// Walk from BlockStore plus AppendWrite and DeleteLog from
	// BlockStoreAppend. The Get signature is byte-identical to the
	// Phase 16 LocalStore.Get this interface supersedes — engine
	// call sites that currently type-assert a *fs.FSStore continue
	// to compile when narrowed to local.LocalStore (or further to
	// blockstore.BlockStore).
	blockstore.BlockStoreAppend

	// ListUnsynced returns a push iterator over every CAS hash present
	// in the local store that has not yet been marked synced in the
	// injected SyncedHashStore. The iterator uses snapshot-at-start
	// semantics: the hash set existing at iteration begin is captured
	// up front; chunks that land after iteration begins are picked up
	// on the NEXT pass — no live-tail catch-up. Iteration stops on the
	// first non-nil error yielded; the yielded error position surfaces
	// any per-hash backend error.
	//
	// When no SyncedHashStore is wired (local-only configurations where
	// no remote mirror is configured), the iterator yields nothing —
	// the synced set is empty by definition and the unsynced set is its
	// strict-subset complement, which collapses to the empty set under
	// the "nothing to mirror anywhere" invariant.
	ListUnsynced(ctx context.Context) iter.Seq2[blockstore.ContentHash, error]

	// --- Lifecycle ---

	// Start launches background goroutines (e.g., periodic metadata
	// persistence).
	Start(ctx context.Context)

	// Close flushes pending metadata and marks the store as closed.
	Close() error

	// --- Per-file admin ---

	// GetFileSize returns the tracked file size and whether the file
	// is tracked. This is a fast in-memory lookup — no disk or store
	// access.
	GetFileSize(ctx context.Context, payloadID string) (uint64, bool)

	// Truncate discards local blocks beyond newSize.
	Truncate(ctx context.Context, payloadID string, newSize uint64) error

	// EvictMemory removes all in-memory data and disk tracking for a
	// file.
	EvictMemory(ctx context.Context, payloadID string) error

	// ListFiles returns the payloadIDs of all files currently tracked
	// in the local store.
	ListFiles() []string

	// GetStoredFileSize returns the total stored data size for a file
	// by summing the DataSize of all FileBlock records in the metadata
	// store.
	GetStoredFileSize(ctx context.Context, payloadID string) (uint64, error)

	// --- Metadata sync ---

	// SyncFileBlocks persists all queued FileBlock metadata updates to
	// the store.
	SyncFileBlocks(ctx context.Context)

	// SyncFileBlocksForFile persists queued FileBlock metadata only for
	// blocks belonging to the given payloadID.
	SyncFileBlocksForFile(ctx context.Context, payloadID string)

	// --- Retention / eviction policy ---

	// SetEvictionEnabled controls whether the local store can evict
	// blocks to make room.
	SetEvictionEnabled(enabled bool)

	// SetRetentionPolicy updates the retention policy for eviction
	// decisions.
	//   - pin: never evict local blocks
	//   - ttl: evict only after file last-access exceeds ttl duration
	//   - lru: evict least-recently-accessed blocks first (default)
	SetRetentionPolicy(policy blockstore.RetentionPolicy, ttl time.Duration)

	// --- Observability ---

	// Stats returns a snapshot of current local store statistics.
	Stats() Stats

	// Healthcheck returns the current health of the local store as a
	// structured [health.Report]. Implementations must satisfy
	// [health.Checker] so the upstream API layer can wrap them with a
	// [health.CachedChecker] and serve /status routes.
	//
	// Implementations should be cheap to call (no full directory
	// scans, no large I/O) and idempotent. The expectation is
	// something on the order of a stat() and possibly a write probe —
	// see fs.FSStore.Healthcheck for the canonical pattern.
	Healthcheck(ctx context.Context) health.Report

	// --- Transitional admin-superset methods ---
	//
	// Engine consumers at engine/{engine.go:147,320,423,635,800,828,
	// fetch.go:140,155,192,302,420,482, syncer.go:381, upload.go:168,
	// dedup.go:248}. Phase 18's Syncer simplification rewrites these
	// sites onto BlockStore.Put / Get / Walk and deletes the methods
	// (and FlushedBlock) entirely. Until then they remain on the
	// interface and on *fs.FSStore so every Phase 17 commit
	// `go build ./...`-cleans (D-01 atomic-merge).
	//
	// Grep marker for the Phase 18 cleanup wave: TRANSITIONAL-PHASE-18.
	// The marker is plain text (not a godoc "Deprecated:" tag) so
	// staticcheck SA1019 does not fire on existing call sites that will
	// be rewritten in Phase 18.

	// ReadAt reads data from the local store at the specified offset
	// into dest. Returns (true, nil) if all requested bytes were
	// found locally, (false, nil) on miss for any block in the
	// range.
	//
	// TRANSITIONAL-PHASE-18: removed when Syncer simplification rewrites
	// engine consumers onto BlockStore.Put/Get/Walk.
	ReadAt(ctx context.Context, payloadID string, dest []byte, offset uint64) (bool, error)

	// WriteAt writes data to the local store at the specified offset.
	//
	// TRANSITIONAL-PHASE-18: see ReadAt.
	WriteAt(ctx context.Context, payloadID string, data []byte, offset uint64) error

	// Flush writes all dirty in-memory blocks for a file to disk as
	// .blk files. Returns the list of blocks that were flushed.
	//
	// TRANSITIONAL-PHASE-18: see ReadAt.
	Flush(ctx context.Context, payloadID string) ([]FlushedBlock, error)

	// IsBlockLocal checks if a specific block is available locally
	// (memory or disk).
	//
	// TRANSITIONAL-PHASE-18: see ReadAt.
	IsBlockLocal(ctx context.Context, payloadID string, blockIdx uint64) bool

	// GetBlockData returns the raw data for a specific block,
	// checking memory first then disk. Returns data, dataSize, and
	// error.
	//
	// TRANSITIONAL-PHASE-18: see ReadAt.
	GetBlockData(ctx context.Context, payloadID string, blockIdx uint64) ([]byte, uint32, error)

	// WriteFromRemote stores data fetched from the remote block
	// store locally. The block is marked Remote since it already
	// exists remotely.
	//
	// TRANSITIONAL-PHASE-18: see ReadAt.
	WriteFromRemote(ctx context.Context, payloadID string, data []byte, offset uint64) error

	// DeleteAllBlockFiles removes all blocks for a file from memory,
	// disk, and metadata.
	//
	// TRANSITIONAL-PHASE-18: see ReadAt.
	DeleteAllBlockFiles(ctx context.Context, payloadID string) error
}

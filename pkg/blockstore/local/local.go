package local

import (
	"context"
	"iter"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/health"
)

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
// [blockstoretest.BlockStoreAppendConformance]) and adds lifecycle
// eviction, retention, and observability methods that are caller-
// visible only from within the daemon process.
type LocalStore interface {
	// Embedding contributes Put, Get, GetRange, Has, Delete, Head
	// Walk from BlockStore plus AppendWrite and DeleteLog from
	// BlockStoreAppend. The Get signature is byte-identical to the
	// LocalStore.Get this interface supersedes — engine
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

	// ReadPayloadAt serves bytes for [offset, offset+len(dest)) from the
	// local store, consulting BOTH the in-flight append log (pre-rollup
	// bytes that have not yet been chunked into CAS) AND the rolled-up
	// CAS chunks via the FileBlock manifest. This is the primary
	// payload-keyed read entry on the local store interface; the engine
	// calls this BEFORE falling back to a CAS-hash-keyed walk on miss.
	//
	// Returns (len(dest), nil) when every requested byte was satisfied
	// from local storage. Returns (0, blockstore.ErrFileBlockNotFound)
	// when no part of the requested range exists in either the append
	// log or the FileBlock manifest — the caller treats this as "must
	// fall back to remote-fetch + zero-fill". Returns (n, err) for
	// genuine I/O errors.
	//
	// The semantics are payload-keyed (payloadID + offset), NOT
	// CAS-hash-keyed: callers do not need to know which chunk covers
	// which byte. This is the critical entry that closes the pre-rollup
	// read-after-write gap; without it, freshly-appended bytes return
	// zeros until the async rollup commits FileBlock rows.
	ReadPayloadAt(ctx context.Context, payloadID string, dest []byte, offset uint64) (int, error)

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
}

package local

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/journal"
	"github.com/marmos91/dittofs/pkg/health"
)

// Stats contains local store statistics for observability.
type Stats struct {
	DiskUsed      int64 // Current total size of on-disk block data in bytes
	MaxDisk       int64 // Configured maximum disk size (0 = unlimited)
	MaxLogBytes   int64 // Append-log pressure budget in bytes (max_log_bytes)
	MemUsed       int64 // Current in-memory dirty buffer usage in bytes
	FileCount     int   // Number of files with local data
	MemBlockCount int   // Number of in-memory dirty blocks
}

// LocalStore is the host-side admin interface for the on-node block store. It is
// the narrowed, journal-native surface: the local tier is now a per-file byte
// cache (WriteAt/ReadAt keyed by payloadID + offset), NOT a content-addressed
// (hash-keyed) blob store. The two production implementations are the
// journal-backed *fs.FSStore and the in-memory *memory.MemoryStore.
//
// The carve seam is journal's: SetCarveTargets injects the dedup oracle + block
// sink, and Carve packs dirty ranges into remote blocks (writing the FileChunk
// manifest rows inside the sink's commit transaction). Cold reads resolve
// through the block-hash → locator + FileChunk rows and Hydrate the fetched
// bytes back into the local tier.
type LocalStore interface {
	// --- Data plane (payloadID + offset keyed) ---

	// WriteAt buffers a dirty client write at offset. It never fsyncs;
	// durability is a separate Commit.
	WriteAt(ctx context.Context, payloadID string, offset int64, data []byte) error

	// ReadAt fills dst with the file's bytes at offset. Never-written ranges are
	// POSIX holes and are zero-filled. Ranges written but evicted return
	// cold=true (dst zero-filled as a placeholder) so the caller hydrates from
	// the remote store and retries.
	ReadAt(ctx context.Context, payloadID string, offset int64, dst []byte) (n int, cold bool, err error)

	// Hydrate writes bytes fetched from the remote store during a cold read.
	// Same append primitive as WriteAt, but the record is born clean (already
	// durable remotely) so it is immediately evictable.
	Hydrate(ctx context.Context, payloadID string, offset int64, data []byte) error

	// Commit fsyncs the file's buffered writes so they become durable. NFS
	// COMMIT / SMB Flush land here. Backends without a durable substrate (the
	// in-memory store) implement it as a no-op returning nil.
	Commit(ctx context.Context, payloadID string) error

	// FileSize reports a file's data high-water mark (max end offset over its
	// live intervals); ok is false when the file has no local entry.
	FileSize(ctx context.Context, payloadID string) (int64, bool)

	// DataExtents returns the sorted, non-overlapping byte ranges [start, end)
	// within [0, fileSize) that the LOCAL tier knows hold data (including
	// evicted/cold ranges, which are still logically present). Closes the
	// NFSv4.2 SEEK data-loss gap (#1481): the engine unions this with the CAS
	// FileChunk manifest so SEEK/READ_PLUS see the same data/hole map READ does.
	DataExtents(ctx context.Context, payloadID string, fileSize int64) ([][2]uint64, error)

	// Truncate shrinks a file to newSize: live intervals past newSize are
	// dropped and a straddling interval is clipped. Growing is a no-op here.
	Truncate(ctx context.Context, payloadID string, newSize int64) error

	// Delete drops all of a file's cached ranges (crash-safe tombstone) so a
	// subsequent read resolves purely through the restored/remote manifest.
	Delete(ctx context.Context, payloadID string) error

	// ListFiles returns every payloadID with live local data, in no guaranteed
	// order. Lets a caller drive a bulk reset (Delete every file).
	ListFiles(ctx context.Context) []string

	// --- Carve (local → remote) ---

	// SetCarveTargets injects the carve collaborators (the remote-durable dedup
	// oracle and the block sink that seals/frames/uploads/commits). Call once
	// before the first Carve. Backends with no real carve (memory) may store
	// them and drive them from Carve, or ignore them.
	SetCarveTargets(deduper journal.Deduper, sink journal.BlockSink)

	// Carve packs eligible files' dirty ranges into remote blocks and flips the
	// carved records to synced. opts.Force bypasses the age/size batching gate;
	// opts.FileID (empty = all files) scopes it to one file.
	Carve(ctx context.Context, opts journal.CarveOptions) (journal.CarveResult, error)

	// UnsyncedBytes reports dirty bytes not yet carved to the remote store — the
	// eviction backpressure signal.
	UnsyncedBytes() int64

	// --- Eviction ---

	// Evict frees local storage under pressure, coldest first, until targetBytes
	// have been freed (targetBytes <= 0 evicts a single unit). Only fully-synced
	// data qualifies so eviction never destroys the only copy of dirty bytes.
	Evict(ctx context.Context, targetBytes int64) (journal.EvictResult, error)

	// SetEvictionEnabled gates eviction. Health-driven: while the remote is
	// unhealthy, cold-marking a range would strand unrecoverable bytes, so
	// eviction is paused.
	SetEvictionEnabled(enabled bool)

	// SetRetentionPolicy is a compatibility no-op on the journal-native local
	// store: the journal evicts whole fully-synced segments approx-LRU and does
	// not honor a pin/ttl/lru knob. Retained so the health/admin path compiles.
	SetRetentionPolicy(policy block.RetentionPolicy, ttl time.Duration)

	// --- Lifecycle ---

	// Start launches background goroutines (if any). Close flushes and marks the
	// store closed.
	Start(ctx context.Context)
	Close() error

	// --- Observability ---

	// Stats returns a snapshot of current local store statistics.
	Stats() Stats

	// Healthcheck returns the current health of the local store. Implementations
	// must satisfy [health.Checker] so the API layer can wrap them with a
	// [health.CachedChecker].
	Healthcheck(ctx context.Context) health.Report
}

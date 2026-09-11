// Package local declares the host-side admin interface for the on-node block
// store. It is the narrowed, journal-native surface: the local tier is a
// per-file byte cache (WriteAt/ReadAt keyed by FileID + offset), NOT a
// content-addressed (hash-keyed) blob store. The two implementations are
// *journal.Store (disk-backed) and *memory.MemoryStore (in-memory).
//
// The interface is declared in journal's vocabulary — FileID, ReadState,
// CarveOptions, EvictResult, Stats — so *journal.Store satisfies it directly
// and callers never bridge a second keyspace. local imports journal (one
// direction); journal does not import local.
package local

import (
	"context"

	"github.com/marmos91/dittofs/pkg/block/journal"
	"github.com/marmos91/dittofs/pkg/health"
)

// LocalStore is the per-share local byte cache. All production consumers hold
// the whole interface, so it is deliberately one wide interface rather than
// composable slices — see the ponytail note below.
//
// The carve seam is journal's: SetCarveTargets injects the dedup oracle + block
// sink, and Carve packs dirty ranges into remote blocks (writing the FileChunk
// manifest rows inside the sink's commit transaction). Cold reads resolve
// through the block-hash → locator + FileChunk rows and Hydrate the fetched
// bytes back into the local tier.
//
// ponytail: one wide interface rather than composable slices, sectioned by the
// comments below. Every production consumer holds the whole store, so splitting
// it would add named unions without narrowing a single dependency; split when a
// consumer genuinely needs only one section.
type LocalStore interface {
	// --- Data plane (FileID + offset keyed) ---

	// WriteAt buffers a dirty client write at offset. It never fsyncs;
	// durability is a separate Commit.
	WriteAt(ctx context.Context, id journal.FileID, offset int64, data []byte) error

	// ReadAt fills dst with the file's bytes at offset. Never-written ranges and
	// evicted ranges are both zero-filled and reported through ReadState, so the
	// caller can hydrate a cold range from the remote store and reconcile a hole
	// against the CAS manifest before trusting the zeros.
	ReadAt(ctx context.Context, id journal.FileID, offset int64, dst []byte) (n int, st journal.ReadState, err error)

	// Hydrate writes bytes fetched from the remote store during a cold read.
	// Same append primitive as WriteAt, but the record is born clean (already
	// durable remotely) so it is immediately evictable.
	//
	// notAfter is the write version sampled before the caller resolved which
	// remote bytes to fetch. The write-back is dropped when the range changed
	// since, so a fetch stalled across a write, truncate or punch cannot put
	// the pre-mutation bytes back. Zero disables the gate.
	Hydrate(ctx context.Context, id journal.FileID, offset int64, data []byte, notAfter uint64) error

	// WriteVersion reports a monotonic marker of the store's write history,
	// sampled before resolving a fetch to bound what it may write back.
	WriteVersion() uint64

	// Invalidate demotes the durable bytes covering [offset, offset+length) to
	// remote-only, so a read of the range fetches rather than serving them. A
	// caller that has proven the local copy unusable calls it before re-fetching,
	// since Hydrate fills and will not write over a range the store still claims.
	Invalidate(ctx context.Context, id journal.FileID, offset, length int64) error

	// Commit fsyncs the file's buffered writes so they become durable. NFS
	// COMMIT / SMB Flush land here. Backends without a durable substrate (the
	// in-memory store) implement it as a no-op returning nil.
	Commit(ctx context.Context, id journal.FileID) error

	// FileSize reports a file's data high-water mark (max end offset over its
	// live intervals); ok is false when the file has no local entry.
	FileSize(ctx context.Context, id journal.FileID) (int64, bool)

	// DataExtents returns the sorted, non-overlapping byte ranges [start, end)
	// within [0, fileSize) that the LOCAL tier knows hold data (including
	// evicted/cold ranges, which are still logically present). Closes the
	// NFSv4.2 SEEK data-loss gap (#1481): the engine unions this with the CAS
	// FileChunk manifest so SEEK/READ_PLUS see the same data/hole map READ does.
	DataExtents(ctx context.Context, id journal.FileID, fileSize int64) ([][2]uint64, error)

	// Truncate shrinks a file to newSize: live intervals past newSize are
	// dropped and a straddling interval is clipped. Growing is a no-op here.
	Truncate(ctx context.Context, id journal.FileID, newSize int64) error

	// Delete drops all of a file's cached ranges (crash-safe tombstone) so a
	// subsequent read resolves purely through the restored/remote manifest.
	Delete(ctx context.Context, id journal.FileID) error

	// ListFiles returns every FileID with live local data, in no guaranteed
	// order. Lets a caller drive a bulk reset (Delete every file).
	ListFiles(ctx context.Context) []journal.FileID

	// FileCount reports the number of files with a live local entry.
	FileCount() int

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

	// SetEvictionPinned gates eviction on the share's retention policy. It is
	// independent of SetEvictionEnabled: a pinned store never evicts, so a
	// health transition re-enabling eviction cannot shed a pinned share's bytes.
	SetEvictionPinned(pinned bool)

	// --- Lifecycle ---

	// Start is a no-op retained on the interface: *journal.Open launches its
	// background loops itself, so there is nothing a caller must start. Close
	// flushes and marks the store closed.
	Start(ctx context.Context)

	Close() error

	// --- Durability ---

	// Durable reports whether the substrate survives a device loss. Drives the
	// honest CLOSE/COMMIT contract alongside the remote tier's report.
	Durable() bool

	// SetDurable overrides the durability report (config["durable"]).
	SetDurable(v bool)

	// --- Observability ---

	// Stats returns a snapshot of current store statistics.
	Stats() journal.Stats

	// Healthcheck returns the current health of the local store. Implementations
	// must satisfy [health.Checker] so the API layer can wrap them with a
	// [health.CachedChecker].
	Healthcheck(ctx context.Context) health.Report
}

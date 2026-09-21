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
)

// The journal-backed store is the production local tier, and this is where the
// compiler is told so: a capability the interface names but journal no longer
// exports (a rename, a signature change) fails the build here, at the
// declaration, rather than silently deselecting a consumer that reached for it
// structurally.
var _ LocalStore = (*journal.Store)(nil)

// LocalStore is the per-share local byte cache. All production consumers hold
// the whole interface, so it is deliberately one wide interface rather than
// composable slices — see the ponytail note below.
//
// The carve seam is journal's Flush: callers enumerate journal ListFiles per
// file id (the empty id is not special) and pass a reading fn plus an AfterFile
// reap; the sink commits the FileChunk manifest rows inside the flush's
// transaction. Cold reads resolve
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
	//
	// What the flags must mean, stated as a rule rather than as a description
	// of any one tier:
	//
	//   - Hole false claims every byte of the window is one this tier was given
	//     — written or hydrated — so the bytes in dst are those bytes. It ends
	//     the caller's inquiry: nothing downstream re-checks it.
	//   - Hole true claims only that the tier does not hold all of the window.
	//     It is not a claim that the share lacks those bytes; it asks the caller
	//     to resolve them against the manifest and the remote first.
	//   - A range the tier cannot classify reports Hole true. Never false.
	//
	// The asymmetry is the whole point, and it is why zero-filling is safe at
	// all: a wrong Hole true costs a manifest check or a fetch that finds
	// nothing, while a wrong Hole false hands the zero fill to the client as
	// data. Uncertainty resolves toward the answer that forces another
	// question, never toward the one that ends the inquiry.
	//
	// The counterpart surface is engine.Store.DataExtents, in
	// pkg/block/engine/dataextents.go, which obeys this same rule through the
	// opposite token: it may over-report data but must never under-report it.
	// The polarity differs because the terminal answer differs — there "hole"
	// is what ends the inquiry, since a sparse-copy client skips a range the
	// map calls empty, while "data" merely forces a READ that can still refuse.
	//
	// So the two surfaces disagree on which token is the safe one BY DESIGN.
	// Anyone reading both in one sitting will be tempted to tidy them into
	// agreement; that edit is the bug, in whichever direction it is made. The
	// rule they share is the sentence above about uncertainty, not the token
	// each one reaches for. engine.Store.DataExtents carries the reverse
	// pointer back here.
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

	// SeedCold and SeedColdBatch mark extents remote-durable-but-not-local, so
	// a read of them faults in from the remote store instead of zero-filling.
	// They arm cold reads over a tier that holds none of the bytes: a restore, a
	// pre-journal upgrade and a server-side copy all leave ranges the manifest
	// places and the tier has never seen. The batch form exists because a tier
	// makes the markers durable once per call rather than once per payload.
	// A tier that cannot hold a range it does not have records nothing.
	SeedCold(ctx context.Context, id journal.FileID, extents [][2]int64) error
	SeedColdBatch(ctx context.Context, seeds []journal.ColdSeed) error

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

	// --- Flush (local → remote) ---

	// Flush runs one flush pass over a file's dirty ranges: it offers each
	// contiguous dirty run to fn and flips the fragments fn reports durable.
	// opts.Force bypasses the age/size batching gate; id scopes it to one file
	// (the empty id is not special — callers enumerate ListFiles and flush
	// each id). fn is mandatory.
	Flush(ctx context.Context, id journal.FileID, opts journal.FlushOptions, fn journal.FlushFunc) error

	// UnsyncedBytes reports dirty bytes not yet carved to the remote store — the
	// eviction backpressure signal.
	UnsyncedBytes() int64

	// UploadConcurrency and BlockSize report the flush shape the tier was
	// configured for: how many block uploads a pass may hold in flight, and the
	// target carve block size. A non-positive answer means the tier has no
	// preference and the caller's own default applies, so a store that does not
	// size its own flushes returns 0 rather than inventing a number.
	UploadConcurrency() int
	BlockSize() int64

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

	// MaxLocalBytes reports the tier's effective disk cap in bytes, 0 when it
	// is uncapped. The store resolves it itself (an explicit budget, or one
	// derived from free space at open), so the cap is a store fact rather than
	// a caller's guess.
	MaxLocalBytes() int64

	// ColdExtents totals the ranges the tier has demoted to remote-only: bytes
	// it no longer holds and would have to fetch to serve. A store that never
	// evicts reports (0, 0). O(live intervals), so callers treat it as a
	// periodic gauge, and a cancelled walk reports no counts rather than a
	// partial total that would read as less remote-only data than there is.
	ColdExtents(ctx context.Context) (bytes int64, extents int64, err error)

	// ColdSeeded reports whether the tier's account of its remote-only ranges
	// can be trusted yet. A tier that has never been told what the share's
	// manifest holds describes an evicted range as absent rather than cold, so
	// its ColdExtents total reads as zero on exactly the case a caller asks
	// about. A tier with no cold ranges to seed is seeded by construction.
	ColdSeeded() bool

	// --- Snapshots ---

	// JournalVersion reports the tier's current write watermark, captured by
	// snapshot create to record the point in time the snapshot names. A tier
	// that keeps no version history reports 0.
	JournalVersion() uint64

	// SetPinVersion holds the bytes of every record at or below v against GC
	// and eviction, so a live snapshot's durable copy cannot be reclaimed out
	// from under it. A tier that never reclaims has nothing to hold back.
	SetPinVersion(v uint64)

	// RestoreToVersion rewinds the tier to v's point-in-time view and
	// re-materializes it durably at the head of the log. It is the local-only
	// restore primitive, used where the tier is the only durable copy of the
	// bytes; it must not run against a share that is still serving writes.
	RestoreToVersion(ctx context.Context, v uint64) error

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

	// DurableExtent reports how far a file's bytes are on stable storage: bytes
	// below the returned offset survive an unclean shutdown, bytes above it
	// were only buffered and are gone after one. ok is false when the tier
	// cannot answer, which callers must read as "unknown", never as "nothing is
	// durable" — a published size derived from it would otherwise describe
	// bytes a crash takes away, leaving the range reading as a hole of zeros.
	DurableExtent(ctx context.Context, id journal.FileID) (int64, bool)

	// SetVerifyReads turns per-read integrity verification of already-resident
	// bytes on and off while the share serves. On, a read that does not match
	// what the tier recorded is healed or failed closed instead of handing back
	// silently-wrong bytes; off is the raw fast path. A tier that stores no
	// checksum has nothing to verify either way.
	SetVerifyReads(v bool)

	// --- Observability ---

	// Stats returns a snapshot of current store statistics.
	Stats() journal.Stats

	// Closed reports whether the store has been closed and is no longer
	// accepting reads or writes. It is the local tier's whole contribution to
	// the engine's health report: the engine shapes the report (it is the one
	// that knows the remote's state too), so the local store answers the plain
	// question rather than returning a status type of its own. Must be cheap
	// enough to call on a health probe — no IO.
	Closed() bool
}

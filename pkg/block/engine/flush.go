package engine

import (
	"context"
	"math"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/journal"
)

// Flush ensures all dirty data for a payload is persisted by delegating
// to the syncer's carve drain. CAS StoreChunk already dedups physically
// by content hash, so no separate file-level dedup hook runs here.
//
// Flush is the single COMMIT/CLOSE seam for every protocol (NFSv3 COMMIT,
// NFSv4 COMMIT, NFS DATA_SYNC/FILE_SYNC WRITE, SMB Flush/CLOSE all reach it
// via common.CommitBlockStore). The WRITE path honors NFS UNSTABLE and defers
// the append-log fsync, so this is where that fsync is paid: SyncPayload
// makes the payload's page-cache-resident records durable BEFORE the syncer
// drain and BEFORE we report success. A fsync failure aborts the flush so the
// durability point never falsely acks (PR3).
func (bs *Store) Flush(ctx context.Context, payloadID string) (*block.FlushResult, error) {
	if err := bs.enter(); err != nil {
		return nil, err
	}
	defer bs.closeMu.RUnlock()
	// Durability barrier: fsync the deferred writes first.
	if err := bs.local.Commit(ctx, payloadID); err != nil {
		return nil, err
	}
	// A durable local store already makes the payload crash-safe at this point,
	// so under the default (async-remote) policy the ack must NOT block on the
	// remote mirror — that is exactly what common.CommitBlockStore documents.
	// Carving to the remote synchronously here turned every FILE_SYNC/DATA_SYNC
	// WRITE into an inline S3 PutObject the reply waited on: multi-second per-op
	// stalls at ~3% CPU (#1621). The background carve loop mirrors the data; a
	// strict share (require_durable_commit) still drains inline below.
	//
	// Still perform the per-payload FileChunk metadata quiesce that syncer.Flush
	// would (persist queued manifest updates so reads and restart-recovery see
	// the authoritative manifest) — only the remote carve drain is skipped.
	if bs.LocalDurable() && !bs.RequireDurableCommit() {
		return &block.FlushResult{Finalized: false}, nil
	}
	// Delegate to the syncer's carve drain.
	return bs.syncer.Flush(ctx, payloadID)
}

// DrainAllUploads forces every dirty payload through rollup and then waits for
// all pending remote uploads to complete.
//
// Rollup must run first: it is what turns still-dirty append-log data into CAS
// chunks, which is the only thing the carver packs to the remote. Draining
// the syncer alone leaves any data still inside the rollup stabilization window
// un-chunked, so it never reaches the remote and the caller's durability
// guarantee silently does not hold (see DrainRollups). The snapshot path rolls
// up explicitly before calling this; the standalone `system drain-uploads` path
// relies on the rollup here.
func (bs *Store) DrainAllUploads(ctx context.Context) error {
	if err := bs.enter(); err != nil {
		return err
	}
	defer bs.closeMu.RUnlock()
	// Force-carve every dirty range to the remote (bypassing the age/size
	// batching gate), then wait for the uploads to settle.
	if _, err := bs.local.Carve(ctx, journal.CarveOptions{Force: true}); err != nil {
		return err
	}
	return bs.syncer.DrainAllUploads(ctx)
}

// SyncCounts returns the lifetime (completed, failed) sync counts for this
// store: chunks that reached the remote and failed carve upload attempts.
// Both are monotonic. The drain-uploads idle watchdog reads them as a
// progress signal. Returns (0, 0) when the store is closing or has no remote
// (local-only stores never sync, so the counters are meaningless — matching
// stats.go, which also reports zeros in that mode).
func (bs *Store) SyncCounts() (completed, failed int) {
	if err := bs.enter(); err != nil {
		return 0, 0
	}
	defer bs.closeMu.RUnlock()
	if bs.remote == nil {
		return 0, 0
	}
	return bs.syncer.SyncCounts()
}

// DrainRollups forces the local store to roll up every currently-dirty
// payload into CAS + the FileChunk manifest, bypassing the
// stabilization-window gate. The snapshot-create orchestration calls this
// BEFORE the metadata Backup() so the dump observes a fully-populated
// FileAttr.Blocks (and therefore a non-empty snapshot manifest). It must
// run before DrainAllUploads — rollup is what produces the CAS chunks the
// carver then packs to the remote.
func (bs *Store) DrainRollups(ctx context.Context) error {
	if err := bs.enter(); err != nil {
		return err
	}
	defer bs.closeMu.RUnlock()
	_, err := bs.local.Carve(ctx, journal.CarveOptions{Force: true})
	return err
}

// ColdSeed is one payload's worth of work for SeedColdBatch: the payload ID and
// the {offset, length} extents of it to mark cold.
type ColdSeed struct {
	PayloadID string
	Extents   [][2]int64
}

// SeedColdBatch marks the seeds' extents as remote-durable-but-not-local so a
// subsequent read faults them in from the remote store rather than zero-filling.
// Snapshot restore and the pre-journal upgrade both use it (remote-backed shares
// only) to arm cold reads over a local tier that holds none of the bytes.
//
// Callers pass many payloads at once because the local tier makes the markers
// durable once per call, not once per payload — an fsync per file is nearly all
// of a manifest seed's wall clock on a share with many small files. Every entry
// is held in memory until that write, so callers bound their own batches.
//
// The batch is one lifecycle-gated op, so a Close waits out the append in flight
// and the seeds after it fail fast rather than reopening the cold log behind a
// torn-down store. Seeding in batches rather than one call for a whole manifest
// is what keeps that wait short.
//
// No-op when the local store has no remote-hydration support (e.g. the in-memory
// test store), which the caller only hits on non-remote paths anyway.
func (bs *Store) SeedColdBatch(ctx context.Context, seeds []ColdSeed) error {
	if err := bs.enter(); err != nil {
		return err
	}
	defer bs.closeMu.RUnlock()
	type coldSeeder interface {
		SeedColdBatch(ctx context.Context, seeds []journal.ColdSeed) error
	}
	cs, ok := bs.local.(coldSeeder)
	if !ok {
		return nil
	}
	js := make([]journal.ColdSeed, 0, len(seeds))
	for _, sd := range seeds {
		js = append(js, journal.ColdSeed{ID: journal.FileID(sd.PayloadID), Extents: sd.Extents})
	}
	return cs.SeedColdBatch(ctx, js)
}

// SeedColdRefs gives the local tier an account of the ranges a payload's chunk
// refs place, marking each one remote-durable-but-not-local. A server-side copy
// hands the destination the source's refs without moving a byte, so the
// destination's ranges exist in the manifest and nowhere in the index; this is
// what puts them there, and it is the caller's post-commit step — seeding before
// the manifest lands would describe rows a rolled-back copy never wrote.
//
// It changes no read: a hole on a remote-backed share already reconciles against
// the manifest and hydrates exactly as a cold range does. What it changes is
// everything that reasons about residency from the index — the remote-only byte
// count an operator acts on, and OfflineReadiness, which can tell a copy nobody
// has read back from a range whose interval was lost only once the copy has one.
//
// Only the parts of each ref the index does not already describe are seeded, so
// a copy over a destination that still holds local bytes leaves those alone, and
// a repeat call costs nothing. Refs with no hash place no bytes (a sparse hole
// carries no chunk) and are skipped, as are empty ones.
//
// One file at a time, under its shard lock, because the destination is live —
// see journal.Store.SeedCold.
func (bs *Store) SeedColdRefs(ctx context.Context, payloadID string, refs []block.ChunkRef) error {
	if err := bs.enter(); err != nil {
		return err
	}
	defer bs.closeMu.RUnlock()
	// Nothing to record on a share with no remote: a cold range there has
	// nowhere to hydrate from and fails its reads closed, where the same range
	// left as a hole reads as the zeros it is. Local-only copies materialize real
	// bytes into the destination's own journal, which describes them already, so
	// there is no work here rather than work being refused — the same shape as
	// the no-remote-hydration case below.
	if !bs.HasRemoteStore() {
		return nil
	}
	type coldSeeder interface {
		SeedCold(ctx context.Context, id journal.FileID, extents [][2]int64) error
	}
	cs, ok := bs.local.(coldSeeder)
	if !ok {
		return nil
	}
	extents := make([][2]int64, 0, len(refs))
	for _, r := range refs {
		if r.Size == 0 || r.Hash.IsZero() {
			continue
		}
		// The index addresses its ranges in int64, so a ref out past that could
		// only be described as a negative offset. Skipping it leaves the range a
		// hole, which still reconciles against the manifest on read.
		if r.Offset > math.MaxInt64-uint64(r.Size) {
			continue
		}
		extents = append(extents, [2]int64{int64(r.Offset), int64(r.Size)})
	}
	if len(extents) == 0 {
		return nil
	}
	return cs.SeedCold(ctx, journal.FileID(payloadID), extents)
}

// RestoreToVersion rewinds the local journal to a snapshot's version watermark
// and re-materializes that point-in-time view durably at the log head. It is the
// local-only snapshot-restore primitive the runtime calls instead of
// ResetLocalState when the share has no remote store (the journal is the only
// durable copy of the bytes). No-op when the local store is not journal-backed
// (e.g. the in-memory test store), which the caller only reaches off this path.
func (bs *Store) RestoreToVersion(ctx context.Context, v uint64) error {
	if err := bs.enter(); err != nil {
		return err
	}
	defer bs.closeMu.RUnlock()
	type restorer interface {
		RestoreToVersion(ctx context.Context, v uint64) error
	}
	r, ok := bs.local.(restorer)
	if !ok {
		return nil
	}
	return r.RestoreToVersion(ctx, v)
}

// SetPinVersion sets the local journal's snapshot pin watermark so GC/eviction
// keep the bytes of every at-or-below-watermark record (the durable copy for a
// live local-only snapshot). No-op when the local store is not journal-backed.
func (bs *Store) SetPinVersion(v uint64) {
	type pinner interface{ SetPinVersion(v uint64) }
	if p, ok := bs.local.(pinner); ok {
		p.SetPinVersion(v)
	}
}

// JournalVersion returns the local journal's current LSN watermark, captured by
// snapshot create (after DrainRollups) to record the snapshot's version. Returns
// 0 when the local store is not journal-backed.
func (bs *Store) JournalVersion() uint64 {
	type versioner interface{ JournalVersion() uint64 }
	if j, ok := bs.local.(versioner); ok {
		return j.JournalVersion()
	}
	return 0
}

// ResetLocalState drops every file's locally cached ranges so post-restore reads
// resolve purely through the restored manifest. The snapshot-restore
// orchestration calls it BEFORE the metadata Reset() + Restore(), not after:
// clearing the local tier first leaves no dirty interval for a background rollup
// worker to flush into the freshly-restored metadata, so a file modified in place
// after the snapshot is never served from a stale local record overlaid on the
// restored bytes.
func (bs *Store) ResetLocalState(ctx context.Context) error {
	if err := bs.enter(); err != nil {
		return err
	}
	defer bs.closeMu.RUnlock()
	// Drop every file's local cached ranges so post-restore reads resolve
	// purely through the restored manifest + remote (there is no append-log
	// overlay to clear anymore — the journal IS the local tier).
	for _, payloadID := range bs.local.ListFiles(ctx) {
		if err := bs.local.Delete(ctx, payloadID); err != nil {
			return err
		}
	}
	return nil
}

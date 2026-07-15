package engine

import (
	"context"

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

// ResetLocalState clears the local store's per-payload append-log state so
// post-restore reads resolve purely through the restored CAS manifest.
// The snapshot-restore orchestration calls this BEFORE the metadata Reset()
// + Restore() (not after): clearing the overlay first leaves no dirty
// intervals for a background rollup worker to flush into the freshly-restored
// metadata, so a file modified in place after the snapshot is not served from
// a stale append-log record overlaid on the restored CAS bytes.
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

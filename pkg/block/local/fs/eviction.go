package fs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/local/logblob"
)

// ensureSpace enforces the local disk-capacity gate for the given number of
// bytes on the Put (read-through staging) path. When over capacity it frees
// space via whole-blob eviction of the oldest sealed, fully-synced log blob
// (blobEvictOne), then compaction of unsynced-pinned blobs (compactBlobOne),
// then a one-shot active-blob seal via Rotate; when nothing is evictable it
// applies backpressure or returns ErrDiskFull as described below.
//
// Critical invariant (LSL-08): the eviction path must NOT consult the
// FileChunkStore (the engine-level metadata store). Future changes to the
// engine API must not leak FileChunkStore calls back into local storage
// decisions.
//
// It MAY, however, consult the SyncedHashStore — a distinct, narrow
// interface that answers only per-hash sync state (IsSynced). blobEvictOne
// uses it to refuse evicting a blob holding unsynced chunks before their
// first mirror (evicting one destroys the only copy). SyncedHashStore is NOT
// the FileChunkStore and is not covered by LSL-08; do not collapse the two
// when editing this path.
//
// Pin mode and the eviction-disabled flag short-circuit to ErrDiskFull
// without evicting. Retention TTL with a non-positive duration behaves the
// same way: retention policy can keep blocks around regardless.
//
// Concurrent ReadChunk that races an evict surfaces as
// block.ErrChunkNotFound; the engine refetches from CAS
// (accept/refetch posture).
func (bc *FSStore) ensureSpace(ctx context.Context, needed int64) error {
	if bc.maxDisk <= 0 {
		return nil
	}

	ret := bc.getRetention()

	// Pin mode or eviction disabled: never evict, just check available space.
	if ret.policy == block.RetentionPin || !bc.evictionEnabled.Load() {
		if bc.usedBytes()+needed > bc.maxDisk {
			return ErrDiskFull
		}
		return nil
	}

	// TTL mode with invalid TTL: treat as non-evictable (same as pin).
	if ret.policy == block.RetentionTTL && ret.ttl <= 0 {
		if bc.usedBytes()+needed > bc.maxDisk {
			return ErrDiskFull
		}
		return nil
	}

	maxWait := bc.evictMaxWait
	if maxWait <= 0 {
		maxWait = 30 * time.Second
	}
	deadline := time.Now().Add(maxWait)

	// Backpressure stall bookkeeping. engaged is set the first time we hit
	// errLRUEmpty with a healthy remote (the remote-cache stall path) so we
	// log the eventual RELEASE exactly once and account the stall duration.
	var (
		engaged    bool
		stallStart time.Time
		// rotatedUnderPressure caps active-blob sealing at one Rotate per
		// ensureSpace call (see the blobEvictOne fall-through below).
		rotatedUnderPressure bool
	)
	release := func(reason string) {
		if !engaged {
			return
		}
		stall := time.Since(stallStart)
		bc.bpStallNanos.Add(int64(stall))
		if rec := bc.recordMetrics(); rec != nil {
			rec.RecordBackpressure(stall)
		}
		if bc.bpLogLimiter == nil || bc.bpLogLimiter.Allow() {
			logger.Info("local cache backpressure released",
				"store", bc.baseDir,
				"reason", reason,
				"disk_used", bc.usedBytes(),
				"max_disk", bc.maxDisk,
				"unsynced_bytes", bc.unsyncedBytesOrZero(),
				"remote_healthy", bc.remoteHealthyOrTrue(),
				"stall_ms", stall.Milliseconds())
		}
		engaged = false
	}

	for bc.usedBytes()+needed > bc.maxDisk {
		// Whole-blob eviction of the oldest sealed, fully-synced log blob
		// (#1527 — the local tier lives in log blobs). blobEvictOne decrements
		// logBlobDiskUsed itself; do NOT release() here — the stall (if
		// engaged) ends once the loop exits, keeping the engage/stall
		// bookkeeping to one segment per ensureSpace call.
		if bfreed, berr := bc.blobEvictOne(ctx); berr == nil && bfreed > 0 {
			continue
		} else if berr != nil && !errors.Is(berr, errLRUEmpty) {
			return fmt.Errorf("ensureSpace: %w", berr)
		}

		// No fully-synced blob to drop whole. Try compaction (#1497):
		// relocate a sealed blob's unsynced survivors into the active blob
		// and reclaim the dead remainder, so a few unsynced chunks no longer
		// pin a whole blob and --local-store-size stays enforceable.
		if cfreed, cerr := bc.compactBlobOne(ctx); cerr == nil && cfreed > 0 {
			continue
		} else if cerr != nil && !errors.Is(cerr, errLRUEmpty) {
			return fmt.Errorf("ensureSpace: %w", cerr)
		}

		// blobEvictOne found no sealed, fully-synced blob to reclaim, yet
		// log-blob bytes remain — they are sitting in the still-open active
		// blob. This is the read-through cache case: a small maxDisk fills
		// one active blob long before it reaches the roll threshold, so
		// blobEvictOne (sealed-only) can never touch it. Seal it via Rotate
		// so the next blobEvictOne can reclaim it. Capped at one rotation
		// per call: an active blob of *unsynced* bytes is still refused by
		// blobEvictOne and must fall through to backpressure below rather
		// than spin out empty blobs. (#1497 replaces this whole-blob seal
		// with finer-grained log-blob compaction.)
		if !rotatedUnderPressure && bc.logBlob != nil && bc.logBlobDiskUsed.Load() > 0 {
			if rerr := bc.logBlob.Rotate(); rerr == nil {
				rotatedUnderPressure = true
				continue
			}
		}

		// No evictable sealed blob: every remaining byte is still unsynced
		// (or lives in the active blob).
		// Branch on whether a remote-backed syncer is wired:
		//
		//   - remote-backed (bpSource != nil): the local tier is a
		//     write-through cache. If the remote is HEALTHY the syncer
		//     can still drain unsynced chunks and free space, so engage
		//     backpressure and stall up to the (longer) backpressure
		//     window. If the remote is UNHEALTHY the syncer cannot
		//     drain, so fail fast with ErrDiskFull rather than stalling
		//     a writer that cannot make progress.
		//   - local-only (bpSource == nil): wait the shorter evictMaxWait
		//     for new evictable bytes (async rollup) to land.
		if bc.bpSource != nil {
			if !bc.bpSource.IsRemoteHealthy() {
				// Remote cannot drain: fail fast (release if we had
				// been stalling on a previously-healthy remote).
				if engaged {
					release("remote_unhealthy")
				}
				return ErrDiskFull
			}
			if !engaged {
				// First time stalling on the remote-cache path for this
				// request: arm the longer deadline, count the engage, and
				// log it (rate-limited).
				maxWait := bc.effectiveBackpressureMaxWait()
				engaged = true
				stallStart = time.Now()
				deadline = stallStart.Add(maxWait)
				bc.bpEngageCount.Add(1)
				if bc.bpLogLimiter == nil || bc.bpLogLimiter.Allow() {
					logger.Info("local cache backpressure engaged: waiting for syncer to drain",
						"store", bc.baseDir,
						"disk_used", bc.usedBytes(),
						"max_disk", bc.maxDisk,
						"needed", needed,
						"unsynced_bytes", bc.bpSource.UnsyncedBytes(),
						"remote_healthy", true,
						"max_wait_ms", maxWait.Milliseconds())
				}
			}
		}

		if time.Now().After(deadline) {
			release("window_exceeded")
			return ErrDiskFull
		}
		select {
		case <-ctx.Done():
			release("ctx_cancelled")
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
			continue
		}
	}

	release("space_freed")
	return nil
}

// effectiveBackpressureMaxWait returns the remote-cache stall window,
// defaulting to 60s when unset.
func (bc *FSStore) effectiveBackpressureMaxWait() time.Duration {
	if bc.backpressureMaxWait > 0 {
		return bc.backpressureMaxWait
	}
	return 60 * time.Second
}

// unsyncedBytesOrZero reads the syncer's unsynced-byte counter, returning 0
// when no syncer is wired (local-only / fixtures).
func (bc *FSStore) unsyncedBytesOrZero() int64 {
	if bc.bpSource == nil {
		return 0
	}
	return bc.bpSource.UnsyncedBytes()
}

// remoteHealthyOrTrue reports remote health, defaulting to true when no
// syncer is wired (local-only stores have no remote to be unhealthy).
func (bc *FSStore) remoteHealthyOrTrue() bool {
	if bc.bpSource == nil {
		return true
	}
	return bc.bpSource.IsRemoteHealthy()
}

// blobEvictOne evicts the oldest sealed log blob whose bytes are all mirrored
// remotely, freeing its physical file. Returns the freed byte count (already
// subtracted from logBlobDiskUsed), or 0 + errLRUEmpty when there is no
// evictable blob (no substrate, only the active blob left, or the oldest
// sealed blob still holds unsynced chunks — blobs are strictly time-ordered,
// so if the oldest is unsynced, newer ones are too).
//
// Sync check: chunks appended in this process lifetime are recorded in
// blobChunks and verified per-hash via the SyncedHashStore (same source
// lruEvictOne uses; matching its semantics, a nil SyncedHashStore means every
// chunk is evictable — production local-only stores are protected by the
// evictionEnabled gate). Blobs from a previous process have no record and
// fall back to the coarse global gate: evictable only when the syncer reports
// zero unsynced bytes anywhere.
//
// After eviction the recorded index entries are pruned eagerly (skipping
// hashes re-staged into a newer blob); entries for pre-restart blobs are
// dropped lazily by ReadChunk when the read surfaces ErrEvicted /
// ErrBlobNotFound.
func (bc *FSStore) blobEvictOne(ctx context.Context) (int64, error) {
	if bc.logBlob == nil {
		return 0, errLRUEmpty
	}

	// Serialize the whole scan→evict→decrement sequence: EvictBlob returns
	// nil for an already-evicted blob, so without this two concurrent
	// callers could account the same blob twice (see blobEvictMu field doc).
	bc.blobEvictMu.Lock()
	defer bc.blobEvictMu.Unlock()

	infos, err := bc.logBlob.ListBlobs()
	if err != nil {
		return 0, fmt.Errorf("blob evict: list blobs: %w", err)
	}
	// ListBlobs is sorted by blob ID = creation order: scan oldest-first.
	for i := range infos {
		if infos[i].Active {
			continue
		}
		id := infos[i].LogBlobID
		// Already processed by this store but still on disk (unlink-failure
		// orphan, see below): its bytes stay counted; skip it.
		if _, done := bc.blobEvictedIDs[id]; done {
			continue
		}
		if !bc.blobSynced(ctx, id) {
			// Blobs are strictly time-ordered: if the oldest candidate is
			// unsynced, newer ones are too — stop scanning. The unsynced bytes
			// pinning this blob are reclaimed at sub-blob granularity by
			// compactBlobOne (#1497), which the caller tries next.
			return 0, errLRUEmpty
		}
		freed, err := bc.evictBlobLocked(ctx, id, infos[i].Size)
		if err != nil {
			return 0, err // errLRUEmpty (active blob) or a real evict failure
		}
		if freed == 0 {
			continue // orphan: unlink failed earlier, bytes stay counted
		}
		logger.Info("local store: evicted sealed log blob",
			"blob", id, "bytes", freed, "dir", bc.baseDir)
		return freed, nil
	}
	return 0, errLRUEmpty
}

// evictBlobLocked physically removes sealed blob id (size bytes) from disk,
// records it evicted, prunes its stale index entries, and settles disk
// accounting. It FORCES eviction (synced predicate always true), so callers
// must have already relocated every must-keep chunk in the blob — blobEvictOne
// gates on blobSynced first; compactBlobOne relocates unsynced survivors first.
//
// Caller MUST hold bc.blobEvictMu.
//
// Returns the bytes reclaimed, or 0 when the unlink failed and the file is
// still on disk (its bytes stay counted until a restart re-seeds and retries).
// EvictBlob is idempotent-nil for an already-evicted blob and never retries a
// failed unlink, so a nil error does not guarantee the file is gone — only
// subtract when it has actually left the disk, or usedBytes would undercount.
func (bc *FSStore) evictBlobLocked(ctx context.Context, id string, size int64) (int64, error) {
	if err := bc.logBlob.EvictBlob(ctx, id, func(string) bool { return true }); err != nil {
		if errors.Is(err, logblob.ErrActiveBlob) {
			return 0, errLRUEmpty
		}
		// The blob may still have transitioned to evicted in-memory (only the
		// unlink failed). Do NOT mark it processed or adjust the counter.
		return 0, fmt.Errorf("blob evict: %w", err)
	}
	bc.blobEvictedIDs[id] = struct{}{}
	bc.dropBlobIndexEntries(ctx, id)
	orphanPath := filepath.Join(bc.baseDir, "blobs", id+".blob")
	if _, statErr := os.Stat(orphanPath); statErr == nil || !os.IsNotExist(statErr) {
		logger.Warn("local store: evicted log blob still on disk (unlink failed earlier); keeping its bytes counted until restart",
			"blob", id, "bytes", size, "dir", bc.baseDir)
		return 0, nil
	}
	bc.subUsed(&bc.logBlobDiskUsed, size, "logblob")
	if rec := bc.recordMetrics(); rec != nil {
		rec.RecordEviction(size)
	}
	return size, nil
}

// blobSynced reports whether every chunk in blobID has been durably mirrored
// to the remote. See blobEvictOne for the tracked / untracked split.
func (bc *FSStore) blobSynced(ctx context.Context, blobID string) bool {
	bc.blobChunksMu.Lock()
	recorded, tracked := bc.blobChunks[blobID]
	hashes := make([]block.ContentHash, len(recorded))
	copy(hashes, recorded)
	bc.blobChunksMu.Unlock()

	if bc.syncedHashStore == nil {
		// Match lruEvictOne: without sync-state tracking every chunk is
		// considered evictable (local-only stores rely on evictionEnabled).
		return true
	}
	if !tracked {
		// Blob predates this process: no per-chunk record. Only evict when
		// the syncer reports nothing unsynced anywhere.
		return bc.bpSource != nil && bc.bpSource.UnsyncedBytes() == 0
	}
	for _, h := range hashes {
		synced, err := bc.syncedHashStore.IsSynced(ctx, h)
		if err != nil {
			logger.Warn("blob evict: IsSynced lookup failed, treating blob as unsynced",
				"blob", blobID, "hash", h.String(), "error", err)
			return false
		}
		if !synced {
			return false
		}
	}
	return true
}

// dropBlobIndexEntries removes the local-index entries recorded for blobID
// and forgets its blobChunks record. A hash whose current index entry points
// at a DIFFERENT blob was re-staged after this blob's copy became stale —
// its live entry is kept. Best-effort: a failed delete leaves a dangling
// entry that ReadChunk drops lazily (ErrEvicted → miss → remote refetch).
func (bc *FSStore) dropBlobIndexEntries(ctx context.Context, blobID string) {
	bc.blobChunksMu.Lock()
	hashes := bc.blobChunks[blobID]
	delete(bc.blobChunks, blobID)
	bc.blobChunksMu.Unlock()

	if bc.localChunkIndex == nil {
		return
	}
	for _, h := range hashes {
		loc, ok, err := bc.localChunkIndex.GetLocalLocation(ctx, h)
		if err != nil || !ok || loc.LogBlobID != blobID {
			continue
		}
		if err := bc.localChunkIndex.DeleteLocalLocation(ctx, h); err != nil {
			logger.Warn("blob evict: failed to drop index entry",
				"blob", blobID, "hash", h.String(), "error", err)
		}
	}
}

// compactBlobOne reclaims the dead space pinned inside a sealed log blob whose
// only surviving bytes are unsynced (crash-stranded) chunks — the exact case
// blobEvictOne refuses because the whole blob is not synced. Without this, one
// small unsynced chunk pins a whole ~1 GB blob and --local-store-size cannot be
// enforced.
//
// It relocates those unsynced survivors into the active blob, fsyncs them, and
// then drops the old blob whole — reclaiming every dead/synced byte at sub-blob
// granularity. Synced chunks are NOT relocated: dropping the old blob removes
// their index entries so the next read refetches them from the durable remote
// copy (they are hot-cache misses, never data loss).
//
// Returns the NET bytes reclaimed (the old blob's size minus the survivor bytes
// re-appended into the active blob), or (0, errLRUEmpty) when no sealed blob has
// any reclaimable dead weight: every candidate is either fully synced (handled
// by blobEvictOne), entirely unsynced-live (nothing can be reclaimed), or a
// pre-restart blob with no per-chunk record to enumerate.
//
// Durability invariant: the only durable copy of an unsynced chunk is its
// log-blob bytes. relocateSurvivors fsyncs the active blob BEFORE rewriting any
// durable index entry and BEFORE the old blob is deleted, so at every instant
// the chunk is readable from at least one durable location. A crash mid-compact
// leaves the index pointing at the still-present old blob (survivor re-appended
// but index not yet flipped) or at the fsynced new copy (index flipped, old
// blob not yet deleted) — never at lost bytes.
//
// ponytail: covers in-process blobs (those with a blobChunks record); a blob
// carried over from a previous process is left to the coarse whole-blob gate,
// matching blobEvictOne's tracked/untracked split. Add index-scan enumeration
// only if pre-restart pinning is observed in practice.
func (bc *FSStore) compactBlobOne(ctx context.Context) (int64, error) {
	if bc.logBlob == nil || bc.localChunkIndex == nil || bc.syncedHashStore == nil {
		return 0, errLRUEmpty
	}

	// Serialize with blobEvictOne (same accounting + evictedIDs bookkeeping).
	bc.blobEvictMu.Lock()
	defer bc.blobEvictMu.Unlock()

	infos, err := bc.logBlob.ListBlobs()
	if err != nil {
		return 0, fmt.Errorf("blob compact: list blobs: %w", err)
	}
	// Oldest-first, matching blobEvictOne's LRU-ish ordering.
	for i := range infos {
		if infos[i].Active {
			continue
		}
		id := infos[i].LogBlobID
		if _, done := bc.blobEvictedIDs[id]; done {
			continue
		}
		survivors, unsyncedBytes, ok := bc.blobSurvivors(ctx, id)
		if !ok {
			continue // pre-restart blob: no per-chunk record to enumerate
		}
		if unsyncedBytes >= infos[i].Size {
			// Entirely unsynced-live: relocating would copy the whole blob for
			// zero net reclaim. Nothing to gain — skip.
			continue
		}
		relocatedBytes, err := bc.relocateSurvivors(ctx, id, survivors)
		if err != nil {
			// A survivor's bytes were unreadable (torn/corrupt sealed blob).
			// Never delete a blob whose must-keep chunks we could not relocate:
			// skip it and try the next candidate. Any already-appended survivor
			// bytes are harmless orphans (index still points at the old blob).
			logger.Warn("local store: blob compaction relocate failed, skipping",
				"blob", id, "error", err, "dir", bc.baseDir)
			continue
		}
		freed, err := bc.evictBlobLocked(ctx, id, infos[i].Size)
		if err != nil {
			return 0, err // errLRUEmpty (active blob) or a real evict failure
		}
		if freed == 0 {
			continue // orphan: unlink failed earlier, bytes stay counted
		}
		// NET reclaimed: the old blob's bytes left disk, but the survivors were
		// re-appended into the active blob, so those bytes were not freed.
		net := freed - relocatedBytes
		logger.Info("local store: compacted sealed log blob",
			"blob", id, "net_reclaimed", net, "relocated_bytes", relocatedBytes,
			"blob_size", freed, "survivors", len(survivors), "dir", bc.baseDir)
		return net, nil
	}
	return 0, errLRUEmpty
}

// blobSurvivors returns the unsynced chunks still resident in blobID (index
// entry still points at blobID and IsSynced reports false) — the chunks
// compaction must relocate before the blob can be dropped — with their total
// byte size. ok is false when blobID has no per-chunk record (a pre-restart
// blob compaction cannot enumerate). A chunk whose sync state cannot be
// confirmed is treated as a survivor (never drop a chunk we cannot prove is
// durable elsewhere).
func (bc *FSStore) blobSurvivors(ctx context.Context, blobID string) (survivors []block.ContentHash, unsyncedBytes int64, ok bool) {
	bc.blobChunksMu.Lock()
	recorded, tracked := bc.blobChunks[blobID]
	hashes := make([]block.ContentHash, len(recorded))
	copy(hashes, recorded)
	bc.blobChunksMu.Unlock()
	if !tracked {
		return nil, 0, false
	}
	for _, h := range hashes {
		loc, present, err := bc.localChunkIndex.GetLocalLocation(ctx, h)
		if err != nil || !present || loc.LogBlobID != blobID {
			continue // deleted or re-staged elsewhere: dead weight here
		}
		synced, serr := bc.syncedHashStore.IsSynced(ctx, h)
		if serr == nil && synced {
			continue // durable on remote: drop-and-refetch, no relocate needed
		}
		survivors = append(survivors, h)
		unsyncedBytes += loc.RawLength
	}
	return survivors, unsyncedBytes, true
}

// survivorReadErr classifies the result of reading a survivor chunk's bytes out
// of the old blob. It returns a non-nil error whenever the bytes could not be
// fully read — INCLUDING a short read with a nil error (n < want, rerr == nil).
// That nil-error case is the trap: forwarding it as fmt.Errorf("...%w", rerr)
// yields a NIL error, which would let the caller evict the old blob after only
// partially relocating a must-keep (unsynced, only-copy) survivor — permanent
// data loss. os.File.ReadAt honors the io.ReaderAt contract (short read ⇒
// non-nil error), so the nil-short branch is defensive, but a silent nil here
// is unrecoverable, so it is guarded explicitly.
func survivorReadErr(n int, rerr error, want int64, h block.ContentHash, blobID string) error {
	if rerr != nil {
		return fmt.Errorf("read survivor %s from %s: %w", h.String(), blobID, rerr)
	}
	if int64(n) < want {
		return fmt.Errorf("read survivor %s from %s: torn tail, got %d of %d bytes",
			h.String(), blobID, n, want)
	}
	return nil
}

// relocateSurvivors copies each survivor chunk's bytes out of the old sealed
// blob into the active blob, fsyncs the active blob, then rewrites the durable
// index entry to the new location. The fsync BEFORE the index rewrite upholds
// the durability invariant: no durable index entry may reference un-fsynced
// bytes. Until the rewrite the old entry (and old blob) still resolve the
// chunk, so a crash never strands it.
//
// One chunk is held in RAM at a time (bounded by the FastCDC chunk size), never
// the whole blob. A read failure on any survivor returns an error so the caller
// leaves the old blob in place rather than deleting bytes it could not relocate.
//
// Returns the total bytes actually relocated (the second on-disk copy the caller
// must subtract from the evicted blob's size to report NET reclaimed).
func (bc *FSStore) relocateSurvivors(ctx context.Context, oldBlobID string, survivors []block.ContentHash) (int64, error) {
	if len(survivors) == 0 {
		return 0, nil
	}
	type moved struct {
		h   block.ContentHash
		loc block.LocalChunkLocation
	}
	relocated := make([]moved, 0, len(survivors))
	for _, h := range survivors {
		loc, present, err := bc.localChunkIndex.GetLocalLocation(ctx, h)
		if err != nil {
			return 0, fmt.Errorf("get local location %s: %w", h.String(), err)
		}
		if !present || loc.LogBlobID != oldBlobID {
			continue // moved/deleted since the survivor scan: no longer ours
		}
		if loc.RawLength == 0 {
			continue // zero-length chunk has no bytes in any blob
		}
		// One chunk in RAM at a time — bounded by the FastCDC chunk size, never
		// the whole log blob.
		dst := make([]byte, loc.RawLength)
		n, rerr := bc.logBlob.ReadAt(ctx, loc, dst)
		if err := survivorReadErr(n, rerr, loc.RawLength, h, oldBlobID); err != nil {
			return 0, err
		}
		newLoc, aerr := bc.logBlob.Append(ctx, dst)
		if aerr != nil {
			return 0, fmt.Errorf("append survivor %s: %w", h.String(), aerr)
		}
		relocated = append(relocated, moved{h: h, loc: newLoc})
	}
	if len(relocated) == 0 {
		return 0, nil
	}
	// Durability fence: fsync the active blob so every relocated survivor is on
	// stable storage BEFORE any durable index entry points at it. (Size-cap
	// rotations during the Append loop already fsynced the blobs they sealed.)
	if err := bc.logBlob.Sync(); err != nil {
		return 0, fmt.Errorf("sync active blob before index rewrite: %w", err)
	}
	var relocatedBytes int64
	for _, m := range relocated {
		if err := bc.localChunkIndex.PutLocalLocation(ctx, m.h, m.loc); err != nil {
			return 0, fmt.Errorf("rewrite index %s: %w", m.h.String(), err)
		}
		bc.logBlobDiskUsed.Add(m.loc.RawLength)
		bc.trackBlobChunk(m.loc.LogBlobID, m.h)
		relocatedBytes += m.loc.RawLength
	}
	return relocatedBytes, nil
}

// reclaimSpace is the write-path counterpart of ensureSpace: called after a
// rollup pass lands new log-blob bytes, it evicts synced CAS chunks and
// sealed synced blobs until the store is back under maxDisk. Best-effort and
// NON-BLOCKING — when nothing is evictable right now (unsynced tail, only the
// active blob left) it returns immediately rather than stalling the rollup
// worker; the next pass retries after the syncer has drained more chunks.
// Serialized via blobReclaimActive so concurrent rollup workers do not
// stampede; losers skip (the winner is already draining to the limit).
// DrainLocalSynced evicts every locally-resident block whose bytes are already
// durable on the remote, on demand — the operator-triggered counterpart to
// reclaimSpace (which runs only on the write/rollup path and only down to
// maxDisk). It drives the same CAS-LRU → sealed-blob → compaction ladder to
// exhaustion, ignoring the maxDisk cap and the pin / eviction-disabled *policy*
// gates: this is an explicit "evict local now" command, not automatic pressure.
//
// Data safety is unconditional and independent of those gates: blobEvictOne
// drops only fully-synced blobs and compactBlobOne relocates a blob's unsynced
// survivors before reclaiming its dead space, so unsynced (remote-missing)
// bytes are never lost. EvictBlockStore refuses to call this without a remote
// store for that reason. Returns the total bytes freed.
func (bc *FSStore) DrainLocalSynced(ctx context.Context) (int64, error) {
	var total int64
	// rotatedActive caps active-blob sealing at one Rotate per call (see the
	// blobEvictOne fall-through below), mirroring ensureSpace.
	var rotatedActive bool
	for {
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
		bfreed, err := bc.blobEvictOne(ctx)
		if err != nil {
			if !errors.Is(err, errLRUEmpty) {
				return total, fmt.Errorf("drain local synced: %w", err)
			}
			// No fully-synced blob to drop whole. Compact a blob pinned by
			// unsynced survivors (#1497); stop when nothing more can be freed.
			cfreed, cerr := bc.compactBlobOne(ctx)
			if cerr != nil && !errors.Is(cerr, errLRUEmpty) {
				return total, fmt.Errorf("drain local synced: %w", cerr)
			}
			if cfreed > 0 {
				total += cfreed
				continue
			}
			// Sealed-blob eviction and compaction are both exhausted,
			// yet log-blob bytes remain: they sit in the still-open ACTIVE blob
			// (a store below the 1 GiB roll threshold never seals — #1465). Seal
			// it once so the next blobEvictOne can reclaim it. Capped at one
			// Rotate per call: an active blob of *unsynced* bytes is still
			// refused by blobEvictOne and must not spin out empty blobs.
			if !rotatedActive && bc.logBlob != nil && bc.logBlobDiskUsed.Load() > 0 {
				if rerr := bc.logBlob.Rotate(); rerr == nil {
					rotatedActive = true
					continue
				}
			}
			return total, nil
		}
		if bfreed == 0 {
			return total, nil
		}
		total += bfreed
	}
}

// ReclaimDeadBlobs reclaims the physical bytes of log blobs whose chunks have
// all been removed from the local index — e.g. read-through-staged chunks the
// mark-sweep GC dropped after an unlink. DeleteChunk (the sweep) removes a
// chunk's index entry, but blob bytes are reclaimed only by blob-level
// eviction, which skips the still-open ACTIVE blob. A read-through-staged chunk
// (FSStore.Put) lands in the active blob, so after a GC sweep its bytes would
// otherwise leak: logBlobDiskUsed stays non-zero and never drains (only an
// explicit evict/DrainLocalSynced would seal the active blob).
//
// This seals a fully-dead active blob so its bytes become evictable, then
// evicts every SEALED blob that has no live index entry left. A blob with ANY
// live chunk (synced or not) is left untouched — no live data is dropped here;
// reclaiming a blob that still holds live chunks is compaction's job under
// memory pressure (#1497). Safe to call after each local GC sweep.
func (bc *FSStore) ReclaimDeadBlobs(ctx context.Context) (int64, error) {
	if bc.logBlob == nil || bc.localChunkIndex == nil {
		return 0, nil
	}

	// Seal the active blob when it is now fully dead so blobEvictOne can reclaim
	// it. Only seal a fully-dead active blob, so we never spin out empty blobs
	// or churn a blob that still backs live reads.
	infos, err := bc.logBlob.ListBlobs()
	if err != nil {
		return 0, fmt.Errorf("reclaim dead blobs: list blobs: %w", err)
	}
	for i := range infos {
		if infos[i].Active && bc.blobFullyDead(ctx, infos[i].LogBlobID) {
			if rerr := bc.logBlob.Rotate(); rerr != nil {
				return 0, fmt.Errorf("reclaim dead blobs: seal active blob: %w", rerr)
			}
			break
		}
	}

	bc.blobEvictMu.Lock()
	defer bc.blobEvictMu.Unlock()

	infos, err = bc.logBlob.ListBlobs()
	if err != nil {
		return 0, fmt.Errorf("reclaim dead blobs: list blobs: %w", err)
	}
	var total int64
	for i := range infos {
		if infos[i].Active {
			continue
		}
		id := infos[i].LogBlobID
		if _, done := bc.blobEvictedIDs[id]; done {
			continue
		}
		if !bc.blobFullyDead(ctx, id) {
			continue
		}
		freed, err := bc.evictBlobLocked(ctx, id, infos[i].Size)
		if err != nil {
			if errors.Is(err, errLRUEmpty) {
				continue // raced to active; skip
			}
			return total, fmt.Errorf("reclaim dead blobs: evict %s: %w", id, err)
		}
		total += freed
	}
	return total, nil
}

// blobFullyDead reports whether none of blobID's recorded chunks still has a
// live local-index entry pointing at blobID. A blob predating this process (no
// blobChunks record) is reported not-dead: without a per-chunk record we cannot
// prove it holds nothing live, and the coarse whole-blob gate already covers it.
func (bc *FSStore) blobFullyDead(ctx context.Context, blobID string) bool {
	bc.blobChunksMu.Lock()
	recorded, tracked := bc.blobChunks[blobID]
	hashes := make([]block.ContentHash, len(recorded))
	copy(hashes, recorded)
	bc.blobChunksMu.Unlock()
	if !tracked || len(hashes) == 0 {
		return false
	}
	for _, h := range hashes {
		loc, present, err := bc.localChunkIndex.GetLocalLocation(ctx, h)
		if err != nil {
			return false // uncertain: never evict
		}
		if present && loc.LogBlobID == blobID {
			return false // a live chunk still references this blob
		}
	}
	return true
}

func (bc *FSStore) reclaimSpace(ctx context.Context) {
	if bc.maxDisk <= 0 || !bc.evictionEnabled.Load() {
		return
	}
	ret := bc.getRetention()
	if ret.policy == block.RetentionPin || (ret.policy == block.RetentionTTL && ret.ttl <= 0) {
		return
	}
	if !bc.blobReclaimActive.CompareAndSwap(false, true) {
		return
	}
	defer bc.blobReclaimActive.Store(false)

	for bc.usedBytes() > bc.maxDisk {
		if ctx.Err() != nil {
			return
		}
		if bfreed, err := bc.blobEvictOne(ctx); err != nil {
			if !errors.Is(err, errLRUEmpty) {
				logger.Warn("reclaimSpace: blob eviction failed", "dir", bc.baseDir, "error", err)
				return
			}
			// No fully-synced blob to drop whole. Compact a blob pinned by
			// unsynced survivors (#1497); stop when nothing more can be freed.
			if cfreed, cerr := bc.compactBlobOne(ctx); cerr != nil {
				if !errors.Is(cerr, errLRUEmpty) {
					logger.Warn("reclaimSpace: blob compaction failed", "dir", bc.baseDir, "error", cerr)
				}
				return
			} else if cfreed == 0 {
				return
			}
		} else if bfreed == 0 {
			return
		}
	}
}

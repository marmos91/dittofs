package engine

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"golang.org/x/sync/errgroup"
	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
)

// inFlightKey returns the deterministic per-block dedup key used by
// the engine's in-flight map. Internal to the engine after
// block.FormatStoreKey was removed.
func inFlightKey(payloadID string, blockIdx uint64) string {
	return fmt.Sprintf("%s/%d", payloadID, blockIdx)
}

// fetchGroup returns an errgroup bounded to ParallelDownloads — the single
// limit on how many block fetches hit the remote at once. Both fetch fan-outs
// (the cold-read demand loop and WarmAll) share it so there is one place that
// decides remote-download concurrency. g.Go blocks once the limit is reached;
// the first task error cancels the rest via the returned context.
func (m *Syncer) fetchGroup(ctx context.Context) (*errgroup.Group, context.Context) {
	parallel := m.config.ParallelDownloads
	if parallel < 1 {
		parallel = 1
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(parallel)
	return g, gctx
}

// resolveFileChunk returns the FileChunk whose chunk range covers the
// byte window [blockIdx*BlockSize, (blockIdx+1)*BlockSize) for payloadID
// or (nil, nil) if no row covers that window (sparse / not yet uploaded).
//
// Post-Phase-18 the engine writers (ObjectIDPersister, ChunkEmitter)
// encode the chunk's absolute byte Offset in the trailing component of
// the FileChunk ID — not a synthetic blockIdx — because FastCDC chunk
// boundaries do not align to BlockSize. Looking up by
// "{payloadID}/{blockIdx*BlockSize}" therefore misses every non-first
// chunk in a multi-chunk file. We instead enumerate the per-payload row
// list and find the row whose [absOffset, absOffset+DataSize) interval
// covers blockIdx*BlockSize, mirroring readLocalByHash's
// findRowCoveringOffset walk.
//
// Post-Phase-17 the engine read path is CAS-only — fb.Hash MUST be non-
// zero for any reachable block; the dispatchRemoteFetch helper enforces
// this.
func (m *Syncer) resolveFileChunk(ctx context.Context, payloadID string, blockIdx uint64) (*block.FileChunk, error) {
	fb, _, err := resolveCovering(ctx, m.fileChunkStore, payloadID, blockIdx*uint64(BlockSize))
	return fb, err
}

// listFileChunksSnapshot returns a point-in-time snapshot of the whole
// FileChunk row list for payloadID with a single ListFileChunks store scan. A
// sparse / not-yet-uploaded payload (ErrFileChunkNotFound) yields (nil, nil).
// Used by whole-manifest consumers (warm); the read path resolves a single
// covering chunk via resolveCovering instead of enumerating.
func (m *Syncer) listFileChunksSnapshot(ctx context.Context, payloadID string) ([]*block.FileChunk, error) {
	rows, err := m.fileChunkStore.ListFileChunks(ctx, payloadID)
	if err != nil {
		if errors.Is(err, block.ErrFileChunkNotFound) {
			return nil, nil // Sparse — not an error
		}
		return nil, fmt.Errorf("list file blocks for %s: %w", payloadID, err)
	}
	return rows, nil
}

// blockIsLocal reports whether the bytes for (payloadID, blockIdx) are
// currently held in the unified local CAS chunk store. It resolves the
// FileChunk row (which carries the BLAKE3 content hash populated by
// rollup) and asks the local store whether the chunk is present under
// that hash. Returns false when the FileChunk row is sparse / not yet
// produced by rollup, when the hash is unknown (pre-CAS migration
// drift), or when local.Has surfaces an error — the caller treats any
// non-true outcome as "must round-trip to remote".
func (m *Syncer) blockIsLocal(ctx context.Context, payloadID string, blockIdx uint64) bool {
	fb, err := m.resolveFileChunk(ctx, payloadID, blockIdx)
	if err != nil {
		return false
	}
	return m.blockIsLocalFromRow(ctx, fb)
}

// blockIsLocalFromRow reports whether the resolved FileChunk's CAS chunk is
// present in the local store. A nil row, a zero hash (sparse / pre-CAS drift),
// or a local.Has error all yield false — the caller treats any non-true outcome
// as "must round-trip to remote".
func (m *Syncer) blockIsLocalFromRow(ctx context.Context, fb *block.FileChunk) bool {
	if fb == nil || fb.Hash.IsZero() {
		return false
	}
	has, err := m.local.Has(ctx, fb.Hash)
	if err != nil {
		return false
	}
	return has
}

// dispatchRemoteFetch routes a per-block S3 GET through the CAS verified-
// read path. Post-Phase-17 there is no legacy fallback: any FileChunk
// surfacing here with a zero Hash is migration drift and the boot guard
// (cmd/dfs/start) should have refused to start. If a stray row
// reaches this code path at runtime, refuse the read instead of returning
// silent zeros.
//
// Returns ("", nil, nil) if the FileChunk has no actionable key (sparse
// or never-uploaded). Errors from the remote store flow through unchanged.
func (m *Syncer) dispatchRemoteFetch(ctx context.Context, fb *block.FileChunk) (string, []byte, error) {
	if fb == nil {
		return "", nil, nil
	}
	if fb.Hash.IsZero() {
		// Legacy path deleted (subsumes A6). Any
		// FileChunk surfacing here without a CAS hash is migration
		// drift — refuse the read instead of returning silent zeros.
		// Boot guard (cmd/dfs/start) refuses to start against an un-
		// migrated store; if this triggers at runtime, the sentinel
		// file was lost or hand-removed.
		logger.Error("legacy zero-hash FileChunk encountered post-migration — refusing read",
			"block_id", fb.ID)
		return "", nil, fmt.Errorf("blockstore: legacy zero-hash FileChunk encountered post-migration: block_id=%s", fb.ID)
	}

	key, data, err := m.resolveAndReadChunk(ctx, fb)
	if err != nil && errors.Is(err, block.ErrChunkNotFound) {
		// Stale-locator window (#1487 compaction, and the cas→blocks migration /
		// refcount reclaim paths): a concurrent maintenance pass relocated this
		// chunk into a fresh block and deleted the old one AFTER we resolved its
		// locator, so the GET 404s against bytes that moved. Re-resolve ONCE — a
		// fresh GetLocator now points at the new block, so a merely-relocated live
		// chunk reads through instead of a spurious EIO. A second miss (locator
		// unchanged, or the chunk is genuinely gone) is returned so the caller
		// fails closed. Single bounded retry — never a loop, to avoid livelock.
		// This is the shared chokepoint for BOTH read paths (fetchResolvedBlock's
		// background prefetch/warm and inlineFetchOrWait's client demand read), so
		// the guard lives here rather than in either caller.
		key, data, err = m.resolveAndReadChunk(ctx, fb)
	}
	return key, data, err
}

// resolveAndReadChunk resolves fb.Hash's current remote block locator and does
// one verified ranged read. Split out of dispatchRemoteFetch so the stale-
// locator retry there can re-resolve from scratch (fresh GetLocator).
//
// Two distinct non-read outcomes, both returned to the caller unchanged:
//
//   - No synced marker at all (synced==false): the chunk has not been uploaded
//     yet, so it has no remote copy. NOT drift — the bytes are still local-only
//     (a read that raced the async carve). Returns ("", nil, nil) so the caller
//     falls back to the local read path rather than failing closed.
//   - Synced marker present but empty BlockID: post-#1493 every synced hash
//     carries a block locator (the startup migration repacked all legacy
//     standalone chunks), so a synced hash with no BlockID is genuine metadata
//     drift. Refuse the read.
func (m *Syncer) resolveAndReadChunk(ctx context.Context, fb *block.FileChunk) (string, []byte, error) {
	loc, synced, err := m.resolveLocator(ctx, fb.Hash)
	if err != nil {
		return "", nil, err
	}
	if !synced {
		return "", nil, nil // not on remote yet — caller serves from local
	}
	if loc.BlockID == "" {
		logger.Error("synced chunk has no block locator — refusing remote fetch (post-migration drift)",
			"block_id", fb.ID,
			"hash", fb.Hash.String())
		return "", nil, fmt.Errorf("blockstore: no block locator recorded for synced chunk %s (post-migration drift)", fb.Hash)
	}
	key := block.FormatBlockKey(loc.BlockID)
	data, perr := m.readChunkVerified(ctx, loc, fb.Hash)
	return key, data, perr
}

// resolveLocator returns the recorded remote locator for hash and whether the
// hash is synced (has a marker at all). synced==false means the chunk has not
// been uploaded yet (still local-only); dispatchRemoteFetch treats that as
// "not on remote" and falls back to local, NOT as drift. A synced hash with an
// empty BlockID is the drift case the caller fails closed on. With no
// SyncedHashStore wired (test fixtures) the hash is reported not synced.
func (m *Syncer) resolveLocator(ctx context.Context, hash block.ContentHash) (block.ChunkLocator, bool, error) {
	m.mu.RLock()
	hs := m.syncedHashStore
	m.mu.RUnlock()
	if hs == nil {
		return block.ChunkLocator{}, false, nil
	}
	loc, ok, err := hs.GetLocator(ctx, hash)
	if err != nil {
		return block.ChunkLocator{}, false, fmt.Errorf("resolve locator %s: %w", hash, err)
	}
	if !ok {
		return block.ChunkLocator{}, false, nil
	}
	return loc, true, nil
}

// readChunkVerified fetches a block-resident chunk through the remote store's
// ChunkReader capability and verifies its BLAKE3 matches hash. Verification
// happens here (not in the store stack) because no single decorator layer holds
// both the chunk's wire bytes and its plaintext-hash domain — ReadChunk
// returns decrypted/decompressed plaintext, and we recompute over it so a
// corrupt ranged read can never be served.
func (m *Syncer) readChunkVerified(ctx context.Context, loc block.ChunkLocator, hash block.ContentHash) ([]byte, error) {
	// remote.RemoteStore embeds ChunkReader, so ranged block reads are always
	// available — no capability probe needed.
	data, err := m.remoteStore.ReadChunk(ctx, loc.BlockID, loc.WireOffset, loc.WireLength, hash)
	if err != nil {
		return nil, err
	}
	computed := block.ContentHash(blake3.Sum256(data))
	if computed != hash {
		if dm := m.dataplaneMetrics(); dm != nil {
			dm.RecordRemoteCorruption(1)
		}
		return nil, fmt.Errorf("%w: block %s chunk %s computed %s",
			block.ErrChunkContentMismatch, loc.BlockID, hash, computed)
	}
	if dm := m.dataplaneMetrics(); dm != nil {
		dm.RecordBlockRangeRead(len(data))
	}
	return data, nil
}

// fetchBlock downloads a single block from the remote store and writes it to the local store.
// Returns nil data for sparse blocks (no FileChunk entry or missing S3 object).
// Returns nil data when remoteStore is nil (local-only mode -- no remote data exists).
func (m *Syncer) fetchBlock(ctx context.Context, payloadID string, blockIdx uint64) ([]byte, error) {
	if !m.canProcess(ctx) {
		return nil, ErrClosed
	}

	if m.remoteStore == nil {
		logger.Debug("syncer: skipping fetchBlock, no remote store")
		return nil, nil // No remote data exists
	}

	// Health gate: fail fast when remote is unreachable
	if !m.IsRemoteHealthy() {
		m.offlineReadsBlocked.Add(1)
		m.logOfflineRead("fetchBlock", payloadID, blockIdx)
		return nil, m.remoteUnavailableError()
	}

	fb, err := m.resolveFileChunk(ctx, payloadID, blockIdx)
	if err != nil {
		return nil, err
	}
	if fb == nil {
		return nil, nil
	}

	return m.fetchResolvedBlock(ctx, fb)
}

// fetchResolvedBlock downloads the already-resolved FileChunk row from the
// remote store, persists it to the local CAS tier, and marks it
// fetched-synced. It is the post-resolve body shared by fetchBlock (which
// resolves by blockIdx round-trip) and WarmAll (which already holds the row
// from enumeration, so it must NOT re-resolve by blockIdx — FastCDC chunks
// start at arbitrary, non-BlockSize-aligned offsets, and a blockIdx lookup
// would miss every non-aligned chunk and silently skip it). Returns nil data
// when the row has no actionable remote key (sparse / never-uploaded).
func (m *Syncer) fetchResolvedBlock(ctx context.Context, fb *block.FileChunk) ([]byte, error) {
	if fb == nil {
		return nil, nil
	}

	// dispatchRemoteFetch carries the stale-locator re-resolve retry (#1487), so
	// a chunk relocated by compaction/migration reads through before we ever get
	// here; a surviving ErrChunkNotFound is genuine live-data-loss.
	storeKey, data, err := m.dispatchRemoteFetch(ctx, fb)
	if err != nil {
		if errors.Is(err, block.ErrChunkNotFound) {
			// fail-closed on the CAS path. A row
			// with a non-zero hash is a live reference to a CAS
			// object; if that object is missing from the remote, the
			// invariant has been violated (GC fail-closed
			// should make this impossible). Returning silent zeros
			// here would corrupt the caller's read with no log trace.
			// Surface ErrChunkNotFound so the caller sees the data
			// loss explicitly. Post-Phase-17 the legacy zero-hash
			// branch is gone, so the !IsZero guard is implicit —
			// any successful dispatchRemoteFetch return implies a
			// CAS row.
			logger.Error("CAS object missing for live FileChunk — possible GC race or live-data-loss",
				"block_id", fb.ID, "store_key", storeKey, "hash", fb.Hash.String())
			return nil, fmt.Errorf("CAS object missing for live row %s (key %s): %w",
				fb.ID, storeKey, block.ErrChunkNotFound)
		}
		return nil, fmt.Errorf("download block %s: %w", storeKey, err)
	}
	if storeKey == "" || data == nil {
		return nil, nil
	}

	// CAS rewire: persist the downloaded bytes to the local CAS chunk
	// store under fb.Hash (verified by readChunkVerified above). The
	// previous WriteFromRemote method buffered into the legacy memBlock
	// + .blk file layout; the unified post-Phase-17 read path resolves
	// (payloadID, blockIdx) → FileChunk.Hash → local.Get(hash), so the
	// downloaded bytes only need to land in the CAS chunk store.
	if err := m.local.Put(ctx, fb.Hash, data); err != nil {
		return nil, fmt.Errorf("store downloaded block %s locally: %w", storeKey, err)
	}
	// The bytes came from remote, so the chunk is already durable there:
	// cancel its redundant re-upload and make it immediately evictable (#1362).
	m.markFetchedSynced(ctx, fb.Hash)

	return data, nil
}

// blockRange returns the start and end block indices for a byte range.
func blockRange(offset uint64, length uint32) (start, end uint64) {
	return offset / uint64(BlockSize), (offset + uint64(length) - 1) / uint64(BlockSize)
}

// EnsureAvailableAndRead downloads blocks and copies data directly to dest, avoiding
// a second local ReadAt. Demanded blocks are downloaded inline in the caller's goroutine
// prefetch uses the worker pool. Returns (filled, error).
func (m *Syncer) EnsureAvailableAndRead(ctx context.Context, payloadID string, offset uint64, length uint32, dest []byte) (bool, error) {
	if length == 0 {
		return false, nil
	}
	if !m.canProcess(ctx) {
		return false, ErrClosed
	}
	if m.remoteStore == nil {
		return false, nil // Local-only: all data must be in local store, no downloads possible
	}

	startBlockIdx, endBlockIdx := blockRange(offset, length)

	// Resolve the covering chunk per block via the indexed lookup
	// (resolveCovering) rather than enumerating the whole manifest. Each block
	// is a cheap single-chunk lookup, so the all-local probe and the download
	// loop each resolve independently; a genuine store error now propagates
	// (the prior in-memory snapshot could not surface one mid-loop).
	allLocal := true
	for blockIdx := startBlockIdx; blockIdx <= endBlockIdx; blockIdx++ {
		fb, err := m.resolveFileChunk(ctx, payloadID, blockIdx)
		if err != nil {
			return false, err
		}
		if !m.blockIsLocalFromRow(ctx, fb) {
			allLocal = false
			break
		}
	}
	if allLocal {
		return false, nil
	}

	// Health gate: fail fast when remote is unreachable
	if !m.IsRemoteHealthy() {
		m.offlineReadsBlocked.Add(1)
		m.logOfflineRead("EnsureAvailableAndRead", payloadID, startBlockIdx)
		return false, m.remoteUnavailableError()
	}

	var filledFlag, needLocalFlag atomic.Bool

	// Fetch the missing blocks concurrently rather than one S3 round-trip at a
	// time. A cold sequential read spans many blocks, and a serial demand loop
	// pins throughput at blockSize/latency (one GET per RTT) — the cold-read
	// wall. fetchGroup bounds the fan-out by ParallelDownloads (the same knob
	// and helper the warm path uses). Each block writes a DISJOINT region of
	// dest, and inlineFetchOrWait's in-flight map dedups concurrent callers, so
	// the fan-out is race-free; the first error cancels the rest via gctx.
	g, gctx := m.fetchGroup(ctx)
	for blockIdx := startBlockIdx; blockIdx <= endBlockIdx; blockIdx++ {
		blockIdx := blockIdx
		g.Go(func() error {
			fb, err := m.resolveFileChunk(gctx, payloadID, blockIdx)
			if err != nil {
				return err
			}
			if m.blockIsLocalFromRow(gctx, fb) {
				needLocalFlag.Store(true)
				return nil
			}
			data, downloaded, err := m.inlineFetchOrWait(gctx, payloadID, blockIdx, fb)
			if err != nil {
				return err
			}
			if !downloaded {
				needLocalFlag.Store(true)
				return nil
			}
			if data == nil {
				zeroBlockRegion(dest, blockIdx, offset, uint64(length))
				filledFlag.Store(true)
				return nil
			}
			if copyBlockToDest(dest, data, blockIdx, offset, uint64(length)) {
				filledFlag.Store(true)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return false, err
	}
	filled := filledFlag.Load()
	needLocalReadAt := needLocalFlag.Load()

	// Prefetch ahead only as far as the payload's access pattern justifies:
	// a sequential run ramps the window up to PrefetchBlocks, a random read
	// prefetches nothing (planReadahead returns 0) so we never spend remote
	// GETs on blocks a random reader will not touch.
	depth := m.planReadahead(payloadID, startBlockIdx, endBlockIdx)
	for i := 0; i < depth; i++ {
		m.enqueuePrefetch(payloadID, endBlockIdx+1+uint64(i))
	}

	if needLocalReadAt {
		return false, nil // Some blocks were in local store -- caller should use local store ReadAt
	}
	return filled, nil
}

// inlineFetchOrWait downloads a block inline or waits for an in-flight download.
// Returns (data, true, nil) for inline download, (nil, false, nil) if piggybacked on existing.
//
// fb is the caller's already-resolved covering FileChunk for the block; a nil
// fb is a sparse block (nothing to fetch).
func (m *Syncer) inlineFetchOrWait(ctx context.Context, payloadID string, blockIdx uint64, fb *block.FileChunk) ([]byte, bool, error) {
	key := inFlightKey(payloadID, blockIdx)

	m.inFlightMu.Lock()
	if existing, ok := m.inFlight[key]; ok {
		m.inFlightMu.Unlock()
		select {
		case <-existing.done:
			existing.mu.Lock()
			err := existing.err
			existing.mu.Unlock()
			return nil, false, err
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
	}

	result := &fetchResult{done: make(chan struct{})}
	m.inFlight[key] = result
	m.inFlightMu.Unlock()

	// Guarantee inFlight cleanup on all exit paths (including panics).
	// The deferred completeInFlight uses completionErr which is set by
	// each exit path before returning.
	var completionErr error
	completed := false
	defer func() {
		if !completed {
			m.completeInFlight(key, result, completionErr)
		}
	}()

	if fb == nil {
		return nil, true, nil
	}

	// Caller (EnsureAvailableAndRead) already verified remoteStore != nil.
	// CAS verified-read dispatch — legacy branch has been removed.
	storeKey, data, err := m.dispatchRemoteFetch(ctx, fb)
	if err != nil {
		if errors.Is(err, block.ErrChunkNotFound) {
			// fail-closed on the CAS path. See
			// fetchBlock for the rationale — a non-zero-hash row that
			// resolves to a missing CAS object is a live-data-loss
			// signal that must NOT silently return zeros. Post-Phase-17
			// every reachable row is CAS-shaped.
			logger.Error("CAS object missing for live FileChunk — possible GC race or live-data-loss",
				"block_id", fb.ID, "store_key", storeKey, "hash", fb.Hash.String())
			wrapped := fmt.Errorf("CAS object missing for live row %s (key %s): %w",
				fb.ID, storeKey, block.ErrChunkNotFound)
			completionErr = wrapped
			return nil, false, wrapped
		}
		// Mirror the ErrChunkNotFound branch above: piggyback waiters
		// read completionErr after result.done closes (via the deferred
		// completeInFlight), so we MUST set completionErr to the same
		// wrapped error the direct caller sees — otherwise the waiter
		// receives the raw err and the error chain is inconsistent
		// between the two return paths.
		completionErr = fmt.Errorf("download block %s: %w", storeKey, err)
		return nil, false, completionErr
	}
	if storeKey == "" || data == nil {
		return nil, true, nil
	}

	// Store locally synchronously; data is already downloaded so there's no
	// reason to hold it in a background goroutine. Under high concurrency
	// background goroutines each holding 8MB data caused OOM.
	//
	// CAS rewire: write under fb.Hash (verified by readChunkVerified). The
	// unified post-Phase-17 read path resolves (payloadID, blockIdx) →
	// FileChunk.Hash → local.Get(hash), so the downloaded bytes only need
	// to land in the CAS chunk store.
	//
	// A Put failure here previously logged at Warn and returned success —
	// bytes were never persisted, callers + every inflight waiter saw a
	// hit, and the next read silently re-fetched from the remote (disk-full
	// / local-IO failure → permanent S3 amplification). Propagate the
	// wrapped error to the caller AND to every waiter via completionErr so
	// no consumer treats the unpersisted bytes as a successful download.
	if writeErr := m.local.Put(ctx, fb.Hash, data); writeErr != nil {
		logger.Error("inline download: local write failed",
			"block", key, "error", writeErr)
		completionErr = fmt.Errorf("inline fetch: persist locally %s: %w", key, writeErr)
		return nil, false, completionErr
	}
	// The bytes came from remote, so the chunk is already durable there:
	// cancel its redundant re-upload and make it immediately evictable (#1362).
	m.markFetchedSynced(ctx, fb.Hash)
	completed = true
	m.completeInFlight(key, result, nil)

	return data, true, nil
}

// completeInFlight signals completion to all waiters and cleans up tracking.
func (m *Syncer) completeInFlight(key string, result *fetchResult, err error) {
	result.mu.Lock()
	result.err = err
	result.mu.Unlock()
	close(result.done)

	m.inFlightMu.Lock()
	delete(m.inFlight, key)
	m.inFlightMu.Unlock()
}

// blockRegion computes the source offset within a block and destination offset within
// the read buffer for a given block, read offset, and read length.
// Returns (srcOffset, destOffset, copyLen). copyLen=0 means no overlap.
func blockRegion(blockIdx, readOffset, readLength, blockDataLen uint64) (srcOff, destOff, copyLen uint64) {
	blockStart := blockIdx * uint64(BlockSize)
	if readOffset > blockStart {
		srcOff = readOffset - blockStart
	}
	if blockStart > readOffset {
		destOff = blockStart - readOffset
	}
	if srcOff >= blockDataLen || destOff >= readLength {
		return 0, 0, 0
	}
	available := blockDataLen - srcOff
	remaining := readLength - destOff
	copyLen = available
	if remaining < copyLen {
		copyLen = remaining
	}
	return srcOff, destOff, copyLen
}

// zeroBlockRegion zeroes the portion of dest that corresponds to a sparse block.
func zeroBlockRegion(dest []byte, blockIdx, offset, length uint64) {
	_, destOff, n := blockRegion(blockIdx, offset, length, uint64(BlockSize))
	if n > 0 && int(destOff+n) <= len(dest) {
		clear(dest[destOff : destOff+n])
	}
}

// copyBlockToDest copies the relevant portion of block data into dest.
func copyBlockToDest(dest, data []byte, blockIdx, offset, length uint64) bool {
	srcOff, destOff, n := blockRegion(blockIdx, offset, length, uint64(len(data)))
	if n > 0 && int(destOff+n) <= len(dest) && int(srcOff+n) <= len(data) {
		copy(dest[destOff:destOff+n], data[srcOff:srcOff+n])
		return true
	}
	return false
}

// enqueuePrefetch enqueues a prefetch request (non-blocking, best effort).
func (m *Syncer) enqueuePrefetch(payloadID string, blockIdx uint64) {
	if m.blockIsLocal(context.Background(), payloadID, blockIdx) {
		return
	}

	// Suppress prefetch when remote is unreachable
	if !m.IsRemoteHealthy() {
		return
	}

	key := inFlightKey(payloadID, blockIdx)
	m.inFlightMu.Lock()
	_, inFlight := m.inFlight[key]
	m.inFlightMu.Unlock()
	if inFlight {
		return
	}

	m.queue.EnqueuePrefetch(TransferRequest{
		Type:       TransferPrefetch,
		PayloadID:  payloadID,
		BlockIndex: blockIdx,
	})
}

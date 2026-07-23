package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

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

// fetchGroup returns an errgroup bounded to ParallelDownloads — the per-call
// limit on concurrent remote block fetches. The two synchronous fan-out
// fetchers (the cold-read demand loop and WarmAll) share it so they bound
// download concurrency the same way; the background SyncQueue prefetch pool is
// separate. g.Go blocks once the limit is reached; the first task error cancels
// the rest via the returned context.
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

// hydrateChunk writes a fetched chunk's verified plaintext back into the local
// journal at its file offset so a subsequent read serves it warm. The chunk is
// already durable on the remote, so Hydrate marks the record clean (immediately
// evictable). The (payloadID, offset) are parsed from the row ID
// "<payloadID>/<offset>" (split on the last '/'). A malformed ID is a hard
// error — an inconsistent manifest, not a benign miss.
func (m *Syncer) hydrateChunk(ctx context.Context, fb *block.FileChunk, data []byte) error {
	i := strings.LastIndexByte(fb.ID, '/')
	off, ok := block.ParseChunkOffset(fb.ID)
	if i <= 0 || !ok {
		// No parseable "payloadID/offset" ID (e.g. a hash-only synthetic row):
		// skip hydration and let the caller serve the already-fetched bytes.
		// Real cold-read rows always carry a valid ID (engineBlockSink writes
		// "<payloadID>/<offset>"), so this only affects hash-only unit fixtures.
		return nil
	}
	return m.local.Hydrate(ctx, fb.ID[:i], int64(off), data)
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
//   - Synced marker present but empty BlockID: a pre-flip standalone chunk the
//     background cas→blocks migration has not repacked yet. Served through the
//     legacy CAS fallback (readStandaloneChunk); only a chunk that is resident
//     nowhere yields an error.
func (m *Syncer) resolveAndReadChunk(ctx context.Context, fb *block.FileChunk) (string, []byte, error) {
	loc, synced, err := m.resolveLocator(ctx, fb.Hash)
	if err != nil {
		return "", nil, err
	}
	if !synced {
		return "", nil, nil // not on remote yet — caller serves from local
	}
	if loc.BlockID == "" {
		// Pre-flip standalone locator: the cas→blocks repack runs as a
		// background pass, so a synced hash can still carry the legacy layout
		// while the repack is in flight. Serve it from the legacy CAS objects
		// (local-first, then remote, BLAKE3-verified); the background migration
		// repacks it and rewrites the locator. Only when the chunk is resident
		// nowhere is this genuine drift / live-data-loss — surfaced (as
		// ErrChunkNotFound) so the caller fails closed rather than serving zeros.
		data, rerr := m.readStandaloneChunk(ctx, fb.Hash)
		if rerr != nil {
			logger.Error("standalone chunk unreadable (no local copy, no legacy object)",
				"block_id", fb.ID, "hash", fb.Hash.String(), "error", rerr)
			return "", nil, rerr
		}
		// Non-empty key so the caller persists the bytes (an empty key is its
		// sparse/never-uploaded sentinel); the hash is the natural CAS key here.
		return fb.Hash.String(), data, nil
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

// fetchBlock downloads a single block from the remote store and writes it to the
// local store. It backs the SyncQueue's prefetch/download workers, so it is the
// engine's readahead fetch path (scheduleReadahead).
// Returns nil data for sparse blocks (no FileChunk entry or missing S3 object).
// Returns nil data when remoteStore is nil (local-only mode -- no remote data
// exists) or when the block is already resident locally (nothing to fetch).
//
// The fetch is routed through inlineFetchOrWait so it registers in the in-flight
// dedup map: a concurrent demand read for the same block piggybacks on this
// prefetch instead of issuing its own S3 GET. That shared budget is what keeps
// total remote concurrency bounded when the readahead window overlaps demand.
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

	// ponytail: no local-presence probe — the journal is (payloadID,offset)-
	// keyed, not hash-keyed, so there is no cheap per-hash Has(). Prefetch just
	// fetches; the in-flight dedup collapses concurrent duplicates and Hydrate
	// is idempotent, so a re-fetch of an already-warm block is at worst a
	// redundant GET (best-effort readahead). Add a journal residency probe here
	// if redundant prefetch GETs ever show up in profiles.
	data, _, err := m.inlineFetchOrWait(ctx, payloadID, blockIdx, fb)
	return data, err
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

	// Hydrate the verified bytes into the local journal at the chunk's file
	// offset (parsed from fb.ID) so a subsequent read serves them warm. The
	// bytes are already durable on the remote, so Hydrate marks the record clean
	// (immediately evictable).
	if err := m.hydrateChunk(ctx, fb, data); err != nil {
		return nil, fmt.Errorf("hydrate downloaded block %s locally: %w", storeKey, err)
	}

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

	end := offset + uint64(length)

	// Resolve EVERY chunk covering [offset, end), not just the chunk at each
	// 8 MiB block-aligned offset. FastCDC chunks are typically smaller than
	// BlockSize, so a block holds several chunks and a read window routinely
	// spans chunk boundaries. The old loop iterated block indices and resolved
	// only the chunk covering blockIdx*BlockSize, so every read window past a
	// block's first chunk went unfetched — served as zeros/stale bytes — and
	// only the block-aligned chunks were staged locally. Walk the actual
	// covering chunks (mirrors readLocalByHash / fillFromCASManifest) so the
	// whole window is downloaded.
	type pending struct {
		blockIdx uint64
		fb       *block.FileChunk
	}
	var toFetch []pending
	for cur := offset; cur < end; {
		fb, absOff, err := resolveCovering(ctx, m.fileChunkStore, payloadID, cur)
		if err != nil {
			return false, err
		}
		if fb == nil {
			// Sparse hole at cur: no chunk covers it. Probe the next block
			// boundary — real files are fully written (no holes). The caller's
			// re-read zero-fills any bytes left uncovered.
			cur = (cur/uint64(BlockSize) + 1) * uint64(BlockSize)
			continue
		}
		// ponytail: no per-hash local-presence probe (journal is not hash-keyed);
		// the caller only reaches here after journal.ReadAt reported the window
		// cold, so fetch every covering chunk. Hydrate is idempotent for any
		// already-warm sub-range.
		toFetch = append(toFetch, pending{blockIdx: absOff / uint64(BlockSize), fb: fb})
		next := absOff + uint64(fb.DataSize)
		if next <= cur {
			next = cur + 1 // guard: a zero/short DataSize row must still advance
		}
		cur = next
	}
	if len(toFetch) == 0 {
		// Nothing to fetch (pure hole) — the caller re-reads and zero-fills.
		return false, nil
	}

	// Health gate: fail fast when remote is unreachable
	if !m.IsRemoteHealthy() {
		m.offlineReadsBlocked.Add(1)
		m.logOfflineRead("EnsureAvailableAndRead", payloadID, offset/uint64(BlockSize))
		return false, m.remoteUnavailableError()
	}

	// Download the missing chunks concurrently rather than one S3 round-trip at
	// a time. A cold sequential read spans many chunks, and a serial demand loop
	// pins throughput at chunkSize/latency (one GET per RTT) — the cold-read
	// wall. fetchGroup bounds the fan-out by ParallelDownloads; inlineFetchOrWait
	// stages each chunk into the local tier and dedups concurrent callers (now
	// keyed per chunk), so the fan-out is race-free and the first error cancels
	// the rest via gctx. We deliberately do NOT copy to dest here: a chunk can
	// start mid-window, so a block-relative copy is wrong — the caller's
	// readLocalByHash does the correct per-offset assembly from the now-local
	// chunks. The extra local pass is cheap next to the S3 GETs just eliminated.
	//
	// Readahead is driven from Store.ReadAt on EVERY read (scheduleReadahead),
	// so the demand path no longer schedules prefetch here.
	//
	// Bound the client-blocking fan-out to DemandFetchTimeout. The health gate
	// above is only a pre-check: a remote can stall AFTER it passes, and the
	// remote client's own retry budget (per-request timeout times max attempts)
	// runs to minutes — far past a protocol client's "server not responding"
	// deadline, so an unbounded fetch here wedges the mount. The bound derives
	// from the caller's context, so a real client cancel still wins; only when
	// our budget fires while the caller is still live do we treat it as an
	// outage. Background prefetch and explicit warm are NOT bounded by this —
	// they never block a client.
	fetchCtx := ctx
	if d := m.config.DemandFetchTimeout; d > 0 {
		var cancel context.CancelFunc
		fetchCtx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	g, gctx := m.fetchGroup(fetchCtx)
	for _, p := range toFetch {
		if gctx.Err() != nil {
			break // first error/cancel: stop scheduling the remaining chunks
		}
		p := p
		g.Go(func() error {
			_, _, err := m.inlineFetchOrWait(gctx, payloadID, p.blockIdx, p.fb)
			return err
		})
	}
	if err := g.Wait(); err != nil {
		// Distinguish "our demand budget fired" from every other failure by the
		// DERIVED context, not by matching the error: fetchCtx.Err() is non-nil
		// only when the budget deadline (or a parent cancel) tripped, and pairing
		// it with a still-live caller context isolates the budget case from a
		// caller-initiated cancel. Matching errors.Is(err, DeadlineExceeded)
		// instead would also catch a deadline surfaced from inside the remote
		// client and mislabel its origin. When our budget fired the remote
		// stalled mid-fetch, so surface it as unavailability (fast client error)
		// rather than a generic read failure; anything else is returned unchanged.
		if fetchCtx.Err() != nil && ctx.Err() == nil {
			m.offlineReadsBlocked.Add(1)
			m.logOfflineRead("EnsureAvailableAndRead", payloadID, offset/uint64(BlockSize))
			return false, m.remoteUnavailableError()
		}
		return false, err
	}

	// Bytes are now local (or genuinely sparse); the caller re-reads via
	// readLocalByHash for the correct assembly.
	return false, nil
}

// inlineFetchOrWait downloads a block inline or waits for an in-flight download.
// Returns (data, true, nil) for inline download, (nil, false, nil) if piggybacked on existing.
//
// fb is the caller's already-resolved covering FileChunk for the block; a nil
// fb is a sparse block (nothing to fetch).
func (m *Syncer) inlineFetchOrWait(ctx context.Context, payloadID string, blockIdx uint64, fb *block.FileChunk) ([]byte, bool, error) {
	// Dedup key must be per-CHUNK, not per-block: a read window can span several
	// chunks that live in the same 8 MiB block (FastCDC chunks are typically
	// smaller than BlockSize), so keying by blockIdx alone would make the second
	// chunk piggyback on the first's in-flight slot and never get downloaded.
	// fb.ID is "<payloadID>/<absOffset>" — unique per chunk — and demand and
	// prefetch resolve the same fb.ID for the same chunk, so they still dedup.
	key := inFlightKey(payloadID, blockIdx)
	if fb != nil {
		key = fb.ID
	}

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
	// Hydrate the verified bytes into the local journal at the chunk's file
	// offset so a subsequent read serves them warm. A Hydrate failure must NOT
	// be treated as a successful download: propagate it to the caller AND every
	// in-flight waiter via completionErr so no consumer trusts unpersisted bytes
	// (disk-full / local-IO failure → permanent remote re-fetch otherwise).
	if writeErr := m.hydrateChunk(ctx, fb, data); writeErr != nil {
		logger.Error("inline download: local hydrate failed",
			"block", key, "error", writeErr)
		completionErr = fmt.Errorf("inline fetch: hydrate locally %s: %w", key, writeErr)
		return nil, false, completionErr
	}
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

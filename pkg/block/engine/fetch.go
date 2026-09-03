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

// coveringChunk pairs a manifest row with the block index its first byte falls
// in, which is the in-flight dedup slot the fetch registers under, and with the
// absolute byte range of the window that this row — and no later-starting row —
// holds.
type coveringChunk struct {
	blockIdx uint64
	fb       *block.FileChunk
	span     hydrateSpan
}

// hydrateSpan is the absolute byte range of a chunk that may be written back
// into the local tier. Coverage resolves an overlap to the greatest start, so a
// row that straddles a later row's start no longer holds the bytes past that
// start; hydrating its full extent would put the older bytes over the newer
// row's head, and the local tier keeps whichever landed last rather than
// whichever row coverage prefers. A zero To means the row's whole extent.
//
// At is the local store's WriteVersion as it stood before the manifest rows were
// resolved. The local tier drops the write-back where the range changed since,
// so a fetch stalled in the remote read while a write, truncate or punch lands
// cannot put the pre-mutation bytes back. Zero leaves that gate off.
type hydrateSpan struct {
	From uint64
	To   uint64
	At   uint64
}

// collectCoveringChunks returns every manifest row covering [start, end), in
// ascending offset order. It is the shared window walk behind the cold-read
// demand fetch and the readahead prefetch.
//
// Resolving one row per BlockSize-aligned offset is not enough: the engine
// writers encode a chunk's absolute byte offset in the trailing component of its
// FileChunk ID, and FastCDC boundaries do not align to BlockSize, so an 8 MiB
// block routinely holds several chunks. A walk that resolved only the row at the
// block-aligned offset would see the block's first chunk and silently drop every
// chunk behind it.
//
// A hole steps to the next chunk that STARTS after the uncovered offset, not to
// the next block boundary, because a hole can be followed by real data inside
// the same block. A row that claims zero or too few bytes still advances the
// cursor by one byte, so a degenerate manifest cannot spin the walk; it can only
// make the walk re-probe, never make it terminate early and report the rest of
// the window as hole.
//
// The cursor advances to the end of the resolved row's CLAIM, not to the end of
// its extent, so a row that straddles a later row's start hands the walk over at
// that start instead of consuming the bytes the later row holds. Each entry
// carries the range it was resolved for, which is what the fetch writes back.
//
// The walk begins at the start of the row covering start, not at start itself,
// so a window opening mid-chunk still hydrates that chunk whole and any row
// hidden between the chunk's start and the window's is resolved rather than
// written over.
func (m *Syncer) collectCoveringChunks(ctx context.Context, payloadID string, start, end uint64) ([]coveringChunk, error) {
	if m.fileChunkStore == nil {
		return nil, nil
	}
	// One resolver for the whole walk: on a backend without the chunk-offset
	// index every lookup falls back to a full per-payload manifest scan, and the
	// resolver's snapshot turns K of those into one.
	res := newChunkWindowResolver(m.fileChunkStore, payloadID)
	// Sampled before the first row is read: every span this walk produces may
	// only write back over bytes that already existed when it started looking.
	at := m.local.WriteVersion()
	if fb, absOff, err := res.coveringRow(ctx, start); err == nil && fb != nil && absOff < start {
		start = absOff
	}
	var out []coveringChunk
	for cur := start; cur < end; {
		fb, absOff, claimEnd, err := res.covering(ctx, cur)
		if err != nil {
			return nil, err
		}
		if fb == nil {
			next, ok, err := res.nextStart(ctx, cur)
			if err != nil {
				return nil, err
			}
			if !ok || next >= end {
				break // nothing starts inside the window past cur — the rest is hole
			}
			cur = next
			continue
		}
		if claimEnd <= cur {
			claimEnd = cur + 1 // a zero/short DataSize row must still advance the walk
		}
		out = append(out, coveringChunk{
			blockIdx: absOff / uint64(BlockSize),
			fb:       fb,
			span:     hydrateSpan{From: cur, To: claimEnd, At: at},
		})
		cur = claimEnd
	}
	return out, nil
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
//
// Only the bytes the row claims are written (see FileChunk.DataSize and
// FileChunk.StartOffset). A remote read returns the whole chunk, so on a row a
// narrow cut down to its surviving stretch, writing the rest would restore
// bytes the row gave up — past the file's new end, above
// the version of the marker that moved it, and a later re-extend would serve
// them where a zero hole is due. A row claiming nothing therefore writes
// nothing: the clamp fails closed, since a claim of zero reaching the local tier
// as "write the whole chunk" is that same resurrection by another route.
//
// span narrows that further to the part of the extent the row still holds, which
// is the range the caller's walk resolved it for (see hydrateSpan). A zero span
// writes the whole claimed extent, which is what a caller holding a row but no
// window wants.
func (m *Syncer) hydrateChunk(ctx context.Context, fb *block.FileChunk, data []byte, span hydrateSpan) error {
	// The claim is [StartOffset, StartOffset+DataSize) of the chunk, and the
	// row's ID names the file offset of its FIRST claimed byte, so the write-back
	// stays aligned only if the head is trimmed off too. A claim the chunk cannot
	// satisfy — one starting past the bytes the remote returned, or an empty one —
	// places nothing rather than the wrong bytes.
	start := uint64(fb.StartOffset)
	end := min(start+uint64(fb.DataSize), uint64(len(data)))
	if start >= end {
		return nil
	}
	data = data[start:end]
	i := strings.LastIndexByte(fb.ID, '/')
	off, ok := block.ParseChunkOffset(fb.ID)
	if i <= 0 || !ok {
		// No parseable "payloadID/offset" ID (e.g. a hash-only synthetic row):
		// skip hydration and let the caller serve the already-fetched bytes.
		// Real cold-read rows always carry a valid ID (engineBlockSink writes
		// "<payloadID>/<offset>"), so this only affects hash-only unit fixtures.
		return nil
	}
	if span.To > 0 {
		lo, hi := clampSpan(span, off, uint64(len(data)))
		if lo >= hi {
			return nil
		}
		off, data = off+lo, data[lo:hi]
	}
	return m.local.Hydrate(ctx, fb.ID[:i], int64(off), data, span.At)
}

// clampSpan converts an absolute hydrate span into offsets within a chunk's
// downloaded bytes, which start at absolute offset chunkStart and run for n. A
// span reaching outside the chunk is trimmed to it rather than rejected: the
// window a walk resolved and the extent the remote returned are two independent
// facts, and only their intersection is safe to write.
func clampSpan(span hydrateSpan, chunkStart, n uint64) (lo, hi uint64) {
	if span.From > chunkStart {
		lo = span.From - chunkStart
	}
	if span.To > chunkStart {
		hi = span.To - chunkStart
	}
	if hi > n {
		hi = n
	}
	return lo, hi
}

// errPreBlockFormatLocator tags the deterministic refusal of a synced locator
// with no block id, so dispatchRemoteFetch's stale-locator retry — which exists
// for locators that moved, not for locators nothing can read — skips it.
var errPreBlockFormatLocator = errors.New("pre-block-format locator")

// dispatchRemoteFetch routes a per-block S3 GET through the CAS verified-read
// path. There is no fallback for the two shapes that predate the packed-block
// format — a zero Hash, or a synced locator with no block id. Nothing converts
// either one any more, and both would otherwise resolve to a bogus key, so each
// refuses the read rather than returning silent zeros.
//
// Returns ("", nil, nil) if the FileChunk has no actionable key (sparse
// or never-uploaded). Errors from the remote store flow through unchanged.
func (m *Syncer) dispatchRemoteFetch(ctx context.Context, fb *block.FileChunk) (string, []byte, error) {
	if fb == nil {
		return "", nil, nil
	}
	if fb.Hash.IsZero() {
		// A row with no content hash predates content addressing entirely.
		// Nothing can locate its bytes, so refuse the read instead of
		// returning silent zeros.
		logger.Error("zero-hash FileChunk has no locatable remote bytes — refusing read",
			"block_id", fb.ID)
		return "", nil, fmt.Errorf("blockstore: zero-hash FileChunk has no locatable remote bytes: block_id=%s", fb.ID)
	}

	key, data, err := m.resolveAndReadChunk(ctx, fb)
	if err != nil && errors.Is(err, block.ErrChunkNotFound) && !errors.Is(err, errPreBlockFormatLocator) {
		// Stale-locator window (compaction and the refcount reclaim paths): a
		// concurrent maintenance pass relocated this
		// chunk into a fresh block and deleted the old one AFTER we resolved its
		// locator, so the GET 404s against bytes that moved. Re-resolve ONCE — a
		// fresh GetLocator now points at the new block, so a merely-relocated live
		// chunk reads through instead of a spurious EIO. A second miss (locator
		// unchanged, or the chunk is genuinely gone) is returned so the caller
		// fails closed. Single bounded retry — never a loop, to avoid livelock.
		// This is the shared chokepoint for BOTH read paths (fetchResolvedBlock's
		// background prefetch/warm and inlineFetchOrWait's client demand read), so
		// the guard lives here rather than in either caller. A pre-block-format
		// locator is excluded above: it is deterministic, so a retry only repeats
		// the refusal and its log line.
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
//   - Synced marker present but empty BlockID: a locator written by a release
//     that predates the packed-block format. Nothing can read it any more, so it
//     fails closed rather than falling through to an empty block key, tagged
//     errPreBlockFormatLocator so the caller's retry does not repeat it.
func (m *Syncer) resolveAndReadChunk(ctx context.Context, fb *block.FileChunk) (string, []byte, error) {
	loc, synced, err := m.resolveLocator(ctx, fb.Hash)
	if err != nil {
		return "", nil, err
	}
	if !synced {
		return "", nil, nil // not on remote yet — caller serves from local
	}
	if loc.BlockID == "" {
		// A locator with no block id was written by a release predating the
		// packed-block format. The reader for it is gone, so refuse rather than
		// fall through to an empty block key — an empty key would resolve to a
		// bogus object and could surface as zeros.
		logger.Error("this share still holds v0.16-v0.21 standalone-CAS locators, which this build "+
			"cannot read; the automatic migration was removed. Stage the upgrade through a release "+
			"that still ships it, or re-ingest the data",
			"block_id", fb.ID, "hash", fb.Hash.String())
		return "", nil, fmt.Errorf("%w: hash %s has a pre-block-format locator: %w", block.ErrChunkNotFound, fb.Hash, errPreBlockFormatLocator)
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

// fetchBlock stages every chunk of one block into the local store. It backs the
// SyncQueue's prefetch/download workers, so it is the engine's readahead fetch
// path (scheduleReadahead), and the downloaded bytes are consumed by the
// subsequent local read, not returned here.
//
// It stages the whole block, not just the chunk at the block-aligned offset. A
// block spans BlockSize bytes while FastCDC chunks are typically far smaller, so
// resolving a single covering row left everything behind that row's first chunk
// cold: readahead promised a block of lookahead and delivered one chunk of it,
// and the demand read then paid the remote round-trips readahead exists to hide.
//
// A sparse block (no covering rows) and local-only mode (nil remoteStore) are
// both nothing-to-do successes. Each chunk's fetch routes through
// inlineFetchOrWait so it registers in the in-flight dedup map: a concurrent
// demand read for the same chunk piggybacks on this prefetch instead of issuing
// its own S3 GET. That shared budget is what keeps total remote concurrency
// bounded when the readahead window overlaps demand.
func (m *Syncer) fetchBlock(ctx context.Context, payloadID string, blockIdx uint64) error {
	if !m.canProcess(ctx) {
		return ErrClosed
	}

	if m.remoteStore == nil {
		logger.Debug("syncer: skipping fetchBlock, no remote store")
		return nil // No remote data exists
	}

	// Health gate: fail fast when remote is unreachable
	if !m.IsRemoteHealthy() {
		m.offlineReadsBlocked.Add(1)
		m.logOfflineRead("fetchBlock", payloadID, blockIdx)
		return m.remoteUnavailableError()
	}

	start := blockIdx * uint64(BlockSize)
	chunks, err := m.collectCoveringChunks(ctx, payloadID, start, start+uint64(BlockSize))
	if err != nil {
		return err
	}

	// ponytail: no local-presence probe — the journal is (payloadID,offset)-
	// keyed, not hash-keyed, so there is no cheap per-hash Has(). Prefetch just
	// fetches; the in-flight dedup collapses concurrent duplicates and Hydrate
	// is idempotent, so a re-fetch of an already-warm chunk is at worst a
	// redundant GET (best-effort readahead). Add a journal residency probe here
	// if redundant prefetch GETs ever show up in profiles.
	//
	// Chunks are staged serially: this already runs on a SyncQueue worker, and
	// fanning out here would multiply the pool's concurrency by the chunks per
	// block behind the window that bounds it.
	for _, c := range chunks {
		if _, _, err := m.inlineFetchOrWait(ctx, payloadID, c.blockIdx, c.fb, c.span); err != nil {
			return err
		}
	}
	return nil
}

// fetchResolvedBlock downloads the already-resolved FileChunk row from the
// remote store, persists it to the local CAS tier, and marks it
// fetched-synced. It is the post-resolve body shared by fetchBlock (which
// resolves by blockIdx round-trip) and WarmAll (which already holds the row
// from enumeration, so it must NOT re-resolve by blockIdx — FastCDC chunks
// start at arbitrary, non-BlockSize-aligned offsets, and a blockIdx lookup
// would miss every non-aligned chunk and silently skip it). Returns nil data
// when the row has no actionable remote key (sparse / never-uploaded).
func (m *Syncer) fetchResolvedBlock(ctx context.Context, fb *block.FileChunk, span hydrateSpan) ([]byte, error) {
	if fb == nil {
		return nil, nil
	}

	// dispatchRemoteFetch carries the stale-locator re-resolve retry, so
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
			// loss explicitly. There is no legacy zero-hash branch, so
			// the !IsZero guard is implicit — any successful
			// dispatchRemoteFetch return implies a CAS row.
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
	if err := m.hydrateChunk(ctx, fb, data, span); err != nil {
		return nil, fmt.Errorf("hydrate downloaded block %s locally: %w", storeKey, err)
	}

	return data, nil
}

// blockRange returns the start and end block indices for a byte range.
func blockRange(offset uint64, length uint32) (start, end uint64) {
	return offset / uint64(BlockSize), (offset + uint64(length) - 1) / uint64(BlockSize)
}

// EnsureAvailable hydrates the local tier for [offset, offset+length) so
// the caller's subsequent local read is served warm. Demanded chunks are
// downloaded inline in the caller's goroutine; prefetch uses the worker pool.
func (m *Syncer) EnsureAvailable(ctx context.Context, payloadID string, offset uint64, length uint32) error {
	if length == 0 {
		return nil
	}
	if !m.canProcess(ctx) {
		return ErrClosed
	}
	if m.remoteStore == nil {
		return nil // Local-only: all data must be in local store, no downloads possible
	}

	end := offset + uint64(length)

	// Resolve EVERY chunk covering [offset, end), not just the chunk at each
	// 8 MiB block-aligned offset (see collectCoveringChunks).
	//
	// ponytail: no per-hash local-presence probe (journal is not hash-keyed);
	// the caller only reaches here after journal.ReadAt reported the window
	// cold, so fetch every covering chunk. Hydrate is idempotent for any
	// already-warm sub-range.
	toFetch, err := m.collectCoveringChunks(ctx, payloadID, offset, end)
	if err != nil {
		return err
	}
	if len(toFetch) == 0 {
		// Nothing to fetch (pure hole) — the caller re-reads and zero-fills.
		return nil
	}

	// Health gate: fail fast when remote is unreachable
	if !m.IsRemoteHealthy() {
		m.offlineReadsBlocked.Add(1)
		m.logOfflineRead("EnsureAvailable", payloadID, offset/uint64(BlockSize))
		return m.remoteUnavailableError()
	}

	// Download the missing chunks concurrently rather than one S3 round-trip at
	// a time. A cold sequential read spans many chunks, and a serial demand loop
	// pins throughput at chunkSize/latency (one GET per RTT) — the cold-read
	// wall. fetchGroup bounds the fan-out by ParallelDownloads; inlineFetchOrWait
	// stages each chunk into the local tier and dedups concurrent callers (now
	// keyed per chunk), so the fan-out is race-free and the first error cancels
	// the rest via gctx. Hydration never fills the caller's buffer: a chunk can
	// start mid-window, so a block-relative copy would be wrong — the caller's
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
		g.Go(func() error {
			_, _, err := m.inlineFetchOrWait(gctx, payloadID, p.blockIdx, p.fb, p.span)
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
			m.logOfflineRead("EnsureAvailable", payloadID, offset/uint64(BlockSize))
			return m.remoteUnavailableError()
		}
		return err
	}

	// Bytes are now local (or genuinely sparse); the caller re-reads via
	// readLocalByHash for the correct assembly.
	return nil
}

// inlineFetchOrWait downloads a block inline or waits for an in-flight download.
// Returns (data, true, nil) for inline download, (nil, false, nil) if piggybacked on existing.
//
// fb is the caller's already-resolved covering FileChunk for the block; a nil
// fb is a sparse block (nothing to fetch). span is the part of the chunk the
// caller resolved this row for, which is what gets written back locally.
func (m *Syncer) inlineFetchOrWait(ctx context.Context, payloadID string, blockIdx uint64, fb *block.FileChunk, span hydrateSpan) ([]byte, bool, error) {
	// Dedup key must be per-CHUNK, not per-block: a read window can span several
	// chunks that live in the same 8 MiB block (FastCDC chunks are typically
	// smaller than BlockSize), so keying by blockIdx alone would make the second
	// chunk piggyback on the first's in-flight slot and never get downloaded.
	// fb.ID is "<payloadID>/<absOffset>" — unique per chunk — and demand and
	// prefetch resolve the same fb.ID for the same chunk, so they still dedup.
	//
	// The span joins the key because a row overlapped by a later one holds two
	// disjoint pieces of a window, and each piece has to be written back; a
	// piggyback would drop the second and leave those bytes cold. Both the
	// demand walk and the prefetch walk open a chunk at its own start, so the
	// ordinary single-piece case still shares one slot.
	key := inFlightKey(payloadID, blockIdx)
	if fb != nil {
		key = fmt.Sprintf("%s@%d", fb.ID, span.From)
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

	// Caller (EnsureAvailable) already verified remoteStore != nil.
	// CAS verified-read dispatch — legacy branch has been removed.
	storeKey, data, err := m.dispatchRemoteFetch(ctx, fb)
	if err != nil {
		if errors.Is(err, block.ErrChunkNotFound) {
			// fail-closed on the CAS path. See
			// fetchBlock for the rationale — a non-zero-hash row that
			// resolves to a missing CAS object is a live-data-loss
			// signal that must NOT silently return zeros. Every
			// reachable row is CAS-shaped.
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
	if writeErr := m.hydrateChunk(ctx, fb, data, span); writeErr != nil {
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

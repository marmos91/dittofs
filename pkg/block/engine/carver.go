package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	gosync "sync"
	"sync/atomic"
	"time"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/blockcodec"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// publishCarveQueueDepth publishes the carve backlog to the
// upload-queue-depth gauge so the metric reflects all not-yet-durable chunks.
func (m *Syncer) publishCarveQueueDepth() {
	mx := m.dataplaneMetrics()
	if mx == nil {
		return
	}
	m.pendingMu.Lock()
	n := len(m.pendingCarveHashes)
	m.pendingMu.Unlock()
	mx.SetUploadQueueDepth(n)
}

// signalCarveWake performs a non-blocking, coalescing send on carveWake.
func (m *Syncer) signalCarveWake() {
	select {
	case m.carveWake <- struct{}{}:
	default:
	}
}

// carveBlockSize returns the configured target block size, falling back to the
// default when unset.
func (m *Syncer) carveBlockSize() int64 {
	if m.config.BlockCarveBytes > 0 {
		return m.config.BlockCarveBytes
	}
	return DefaultBlockCarveBytes
}

// carveDispatcher is the background carve loop. It waits for a wake (a chunk was
// queued) or an idle timeout, then packs blocks: while a full block's worth of
// chunks is available it seals them into target-sized blocks; after UploadDelay
// of quiescence it flushes the partial remainder so a trickle of writes still
// reaches the remote. Runs only when a remote exists and not in ManualSync mode
// (where Flush/SyncNow are the sole carve drivers).
func (m *Syncer) carveDispatcher(ctx context.Context) {
	logger.Info("Carve dispatcher started")
	idle := m.config.UploadDelay
	if idle <= 0 {
		idle = 10 * time.Second
	}
	timer := time.NewTimer(idle)
	defer timer.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ctx.Done():
			return
		case <-m.carveWake:
			// A chunk arrived: drain full blocks now, leave any partial for the
			// idle flush. Reset the idle window.
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			if m.IsRemoteHealthy() {
				if err := m.carveFlush(ctx, false); err != nil && ctx.Err() == nil {
					logger.Warn("Carve dispatcher: size-triggered flush failed; will retry", "error", err)
				}
			}
			timer.Reset(idle)
		case <-timer.C:
			// Quiescent: flush the partial remainder so trickle writes sync.
			if m.IsRemoteHealthy() {
				if err := m.carveFlush(ctx, true); err != nil && ctx.Err() == nil {
					logger.Warn("Carve dispatcher: idle flush failed; will retry", "error", err)
				}
			}
			timer.Reset(idle)
		}
	}
}

// carveFlush packs pending carve chunks into one or more blocks. When drainAll
// is false it emits only full (>= target) blocks and leaves a sub-target
// remainder pending for the idle flush; when true it also emits the final
// partial block. Serialized by carveMu so the background loop and an explicit
// Flush/SyncNow never build a block from the same chunk twice. Each block is
// idempotent on its fresh random blockID: a crash after PutBlock but before the
// commit leaves an orphan remote block (reclaimed by block GC) and re-carves the
// chunks into a new block, never losing or double-committing them.
func (m *Syncer) carveFlush(ctx context.Context, drainAll bool) error {
	if !m.carveActive.Load() {
		// No remote or the carve substrate is not wired: nothing can be
		// packed. Pending chunks stay in the set; Flush/SyncNow report the
		// condition honestly.
		return nil
	}
	// Created outside carveMu to avoid a carveMu→m.mu nesting; idempotent.
	m.ensureUploadLimiter()

	m.carveMu.Lock()
	defer m.carveMu.Unlock()

	target := m.carveBlockSize()

	var (
		wg       gosync.WaitGroup
		errOnce  gosync.Once
		firstErr error
		failed   atomic.Bool
	)
	fail := func(err error) {
		errOnce.Do(func() { firstErr = err })
		failed.Store(true)
	}

	for {
		if err := ctx.Err(); err != nil {
			fail(err)
			break
		}
		// Stop claiming more once any in-flight block failed: a failing remote
		// should not keep draining the queue this pass (it retries next pass).
		if failed.Load() {
			break
		}
		batch, batchBytes := m.claimCarveBatch(target, drainAll)
		if len(batch) == 0 {
			break
		}
		// Bound concurrent PutBlocks by the pinned or adaptive window. Acquire
		// blocks for a slot and returns ctx.Err() on cancellation, so a
		// cancelled flush stops dispatching without stranding the claimed batch.
		if err := m.uploadLimiter.Acquire(ctx); err != nil {
			m.requeueCarveBatch(batch)
			fail(err)
			break
		}
		wg.Add(1)
		go func(batch []block.ContentHash, batchBytes int64) {
			defer wg.Done()
			defer m.uploadLimiter.Release()
			// Each block is independent — distinct chunks, a fresh random
			// blockID, an idempotent PutBlock, an atomic per-block commit — so
			// blocks carve and upload concurrently. claimCarveBatch and
			// requeueCarveBatch serialize carveQ under pendingMu; carveMu (held
			// by the parent) serializes whole flushes so a background pass and an
			// explicit Flush never build a block from the same chunk twice.
			if err := m.carveAndCommitBlock(ctx, batch, batchBytes); err != nil {
				// Return the batch to the queue for a later retry and surface the
				// first error so an explicit Flush reports non-durability.
				m.requeueCarveBatch(batch)
				fail(err)
			}
		}(batch, batchBytes)
	}
	// Block until every dispatched block is durable (or errored). Flush/SyncNow
	// must not report success while a PutBlock is still in flight.
	wg.Wait()
	return firstErr
}

// ensureUploadLimiter lazily creates the upload limiter for Syncers built
// directly (test fixtures) rather than via NewSyncer, which always wires it. A
// fixture has no adaptive control goroutine, so the limiter holds a fixed
// window: the pinned ParallelUploads if set, else the adaptive ceiling so
// fixtures still get full concurrency. Idempotent under m.mu.
func (m *Syncer) ensureUploadLimiter() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.uploadLimiter != nil {
		return
	}
	start := m.config.ParallelUploads
	if start <= 0 {
		start = AdaptiveUploadCeiling
	}
	m.uploadLimiter = newDynamicSemaphore(start)
}

// claimCarveBatch pops chunks from the carve FIFO (validating against the
// authoritative map) until their accumulated size reaches target, or the queue
// drains. When drainAll is false and the accumulated bytes are below target it
// returns nil — leaving the sub-target remainder pending — so the background
// loop never emits an undersized block ahead of the idle flush. Claimed hashes
// are removed from carveQ but remain in pendingCarveHashes until their block
// commits (or are requeued on failure). Caller holds carveMu (so no concurrent
// claim races the same chunks).
// The returned total is the summed body size of the claimed chunks, so the
// caller can pre-size the block buffer exactly (avoiding grow-and-copy churn).
func (m *Syncer) claimCarveBatch(target int64, drainAll bool) ([]block.ContentHash, int64) {
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()

	// Peek: walk the FIFO prefix accumulating the size of still-pending hashes
	// WITHOUT detaching them. An under-target trickle then bails out leaving
	// carveQ untouched, avoiding the O(n) pop-then-prepend copy that made
	// frequent carveWake churn quadratic.
	var (
		batch    []block.ContentHash
		total    int64
		consumed int // number of carveQ entries the claimed prefix spans
	)
	for i, h := range m.carveQ {
		size, ok := m.pendingCarveHashes[h]
		if !ok {
			continue // stale: already committed out from under the queue
		}
		batch = append(batch, h)
		total += size
		if total >= target {
			consumed = i + 1
			break
		}
	}

	if total < target && !drainAll {
		// Not enough for a full block yet: leave carveQ untouched and wait for
		// more chunks or the idle flush. Nothing was detached, so FIFO order and
		// the pending set are exactly as the caller left them.
		return nil, 0
	}

	// Committing to carve: detach the claimed prefix now. When target was hit
	// mid-queue, keep the untouched suffix; otherwise (drainAll drained the tail,
	// or target landed on the last entry) the queue is empty — release its
	// backing array.
	if total >= target && consumed < len(m.carveQ) {
		m.carveQ = m.carveQ[consumed:]
	} else {
		m.carveQ = nil
	}
	return batch, total
}

// requeueCarveBatch returns a failed batch's hashes to the carve queue (only
// those still pending) so a later pass retries them.
func (m *Syncer) requeueCarveBatch(batch []block.ContentHash) {
	m.pendingMu.Lock()
	for _, h := range batch {
		if _, ok := m.pendingCarveHashes[h]; ok {
			m.carveQ = append(m.carveQ, h)
		}
	}
	m.pendingMu.Unlock()
}

// carveByteState classifies the outcome of resolving a carve-claimed chunk's
// bytes: found (pack it), transient (bytes not resolvable yet — leave pending
// and retry a later pass), or gone (bytes genuinely lost — drop it).
type carveByteState int

const (
	carveBytesFound carveByteState = iota
	carveBytesTransient
	carveBytesGone
)

// carveChunkBytes resolves the plaintext for one carve-claimed hash. The
// log-blob location is the primary source (fs-backed local tier); when the
// local index has no entry — or no log-blob reader is wired at all (memory
// local stores have no log-blob substrate) — it falls back to the hash-keyed
// local store read. The returned location is the zero value on the fallback
// path, which DefaultCommitBlock treats as "no local index entry to write".
//
// A rolled-up chunk is registered for carve (via OnChunkComplete) BEFORE its
// log-blob index entry is committed in the rollup's Phase C, so an index miss
// with no hash-keyed copy is almost always "not yet committed" — reported as
// carveBytesTransient so the caller requeues it for a later pass rather than
// dropping it (which would silently lose the upload). Only a positively-located
// entry whose blob bytes have vanished (ReadLocalAt → ErrChunkNotFound) is
// carveBytesGone.
func (m *Syncer) carveChunkBytes(
	ctx context.Context,
	committer blockCommitter,
	reader localBlobReader,
	h block.ContentHash,
) ([]byte, block.LocalChunkLocation, carveByteState, error) {
	var zero block.LocalChunkLocation
	if reader != nil {
		loc, ok, err := committer.GetLocalLocation(ctx, h)
		if err != nil {
			return nil, zero, carveBytesGone, fmt.Errorf("carve: get local location %s: %w", h, err)
		}
		if ok {
			dst := make([]byte, loc.RawLength)
			if _, err := reader.ReadLocalAt(ctx, loc, dst); err != nil {
				if errors.Is(err, block.ErrChunkNotFound) {
					return nil, zero, carveBytesGone, nil // located but bytes vanished
				}
				return nil, zero, carveBytesGone, fmt.Errorf("carve: read local %s: %w", h, err)
			}
			return dst, loc, carveBytesFound, nil
		}
	}
	// Hash-keyed fallback: the only byte source for local stores without a
	// log-blob substrate, and the last resort for a missing index entry.
	data, err := m.local.Get(ctx, h)
	if err != nil {
		if errors.Is(err, block.ErrChunkNotFound) {
			// No index entry and no hash-keyed copy: the pre-Phase-C window.
			// Requeue rather than drop so the imminent index commit is picked
			// up next pass.
			return nil, zero, carveBytesTransient, nil
		}
		return nil, zero, carveBytesGone, fmt.Errorf("carve: read local %s: %w", h, err)
	}
	return data, zero, carveBytesFound, nil
}

// carveAndCommitBlock reads each chunk's raw bytes from the log-blob substrate,
// seals it through the per-chunk transform, frames it into one block via
// blockcodec, uploads the assembled block with PutBlock, and atomically commits
// the block record + per-chunk locators + synced markers. On success the
// committed hashes leave pendingCarveHashes and the bytes leave unsyncedBytes.
func (m *Syncer) carveAndCommitBlock(ctx context.Context, batch []block.ContentHash, batchBytes int64) error {
	m.mu.RLock()
	rbs := m.remoteBlockStore
	sealer := m.chunkSealer
	committer := m.blockCommitter
	reader := m.localBlobReader
	m.mu.RUnlock()
	if rbs == nil || committer == nil {
		return errors.New("carve: substrate not wired")
	}

	// Defense-in-depth (secondary): drop any hash that is already fully synced
	// before packing. CommitBlock is atomic (a failed commit leaves nothing
	// synced), but an already-synced hash can still appear here — e.g. a hash
	// requeued after a post-commit crash, or one retired concurrently by
	// markFetchedSynced. Packing such a hash again would inflate the new
	// block's LiveChunkCount (since MarkSynced is idempotent and the locator
	// would not update). Drop it from the carve set instead.
	filtered := batch[:0]
	for _, h := range batch {
		synced, err := committer.IsSynced(ctx, h)
		if err != nil {
			// Cannot determine: include and let MarkSynced idempotency handle it.
			filtered = append(filtered, h)
			continue
		}
		if synced {
			// Already synced: retire from the carve set without re-packing.
			m.dropCarveHash(h)
			continue
		}
		filtered = append(filtered, h)
	}
	batch = filtered
	if len(batch) == 0 {
		return nil
	}

	blockID, err := newBlockID()
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	// Pre-size so the block is written into one backing array instead of being
	// doubled-and-copied as it grows — #1555 profiling showed that growth was the
	// streamer's dominant allocation (~346 MB per 64 MiB). batchBytes is the raw
	// claimed body size; per chunk we add 256 B of headroom to cover the codec
	// record/header (~38 B) plus a decorated sealer's per-frame wire inflation
	// (encryption's magic + wrapped-key + nonce + AEAD tag ≈ 80 B). For the
	// identity sealer this is an exact upper bound (and the sync filter only
	// drops chunks, never adds). A sealer that inflates past the headroom (e.g. a
	// pathologically large wrapped key) costs at most one regrow — still correct,
	// just less optimal. Computed in int64 and range-checked: the pre-grow is
	// best-effort, so on an absurd block-size config we skip it rather than risk
	// a negative int conversion panicking bytes.Buffer.Grow.
	if grow := batchBytes + int64(len(batch))*256 + 512; grow > 0 && grow <= math.MaxInt {
		buf.Grow(int(grow))
	}
	builder, err := blockcodec.NewBuilder(&buf, blockID, nil)
	if err != nil {
		return fmt.Errorf("carve: new builder: %w", err)
	}

	commits := make([]block.BlockChunkCommit, 0, len(batch))
	for _, h := range batch {
		dst, loc, state, err := m.carveChunkBytes(ctx, committer, reader, h)
		if err != nil {
			return err
		}
		switch state {
		case carveBytesTransient:
			// Bytes not resolvable yet (the chunk's log-blob index entry commits
			// in the rollup's imminent Phase C). Leave it in the carve set so a
			// later pass packs it; do NOT drop (that would lose the upload).
			continue
		case carveBytesGone:
			// Bytes genuinely lost. Drop from the carve set; a write-again or the
			// reconcile sweep re-discovers it if it still exists somewhere.
			logger.Error("carve: chunk bytes lost — dropped", "hash", h.String())
			m.dropCarveHash(h)
			continue
		}

		wire := dst
		if sealer != nil {
			wire, err = sealer.SealChunk(ctx, h, dst)
			if err != nil {
				return fmt.Errorf("carve: seal chunk %s: %w", h, err)
			}
		}

		chunkLoc, err := builder.Add(h, wire)
		if err != nil {
			return fmt.Errorf("carve: frame chunk %s: %w", h, err)
		}
		commits = append(commits, block.BlockChunkCommit{
			Hash:   h,
			Remote: chunkLoc,
			Local:  loc,
		})
	}
	if len(commits) == 0 {
		return nil // every chunk in the batch was dropped
	}
	if _, err := builder.Finish(); err != nil {
		return fmt.Errorf("carve: finish block: %w", err)
	}

	blockBytes := buf.Bytes()
	blockHash := block.ContentHash(blake3.Sum256(blockBytes))

	// Upload the assembled block, then atomically commit. The order matters for
	// crash-safety: PutBlock first means a crash before the commit leaves an
	// orphan block (GC reclaims it) but never an unbacked record.
	mx := m.dataplaneMetrics()
	uploadStart := time.Now()
	if mx != nil {
		mx.UploadStarted()
	}
	err = rbs.PutBlock(ctx, blockID, bytes.NewReader(blockBytes))
	if mx != nil {
		mx.UploadFinished()
		result := "ok"
		if err != nil {
			result = "error"
		}
		mx.RecordUpload(len(blockBytes), result, time.Since(uploadStart))
	}
	if err != nil {
		m.failedSyncs.Add(1)
		// Feed the adaptive controller: an upload error this interval signals
		// server pushback, so the controller backs the window off next tick.
		m.uploadErrWindow.Add(1)
		return fmt.Errorf("carve: put block %s: %w", blockID, err)
	}
	// Count delivered bytes for the adaptive controller's goodput sample. The
	// control goroutine swaps this to zero each tick to compute bytes/sec.
	m.uploadedBytesWindow.Add(int64(len(blockBytes)))

	rec := block.BlockRecord{
		BlockID:        blockID,
		BlockHash:      blockHash,
		Length:         int64(len(blockBytes)),
		LiveChunkCount: uint32(len(commits)),
		SyncState:      block.BlockStateRemote,
	}
	// DefaultCommitBlock is fully atomic: record, local locations, and synced
	// locators land in one transaction, so an error means NOTHING was
	// committed. Just return it — the caller's existing requeue logic
	// (requeueCarveBatch) re-drives the whole batch, and the uploaded block
	// object becomes an orphan the GC sweep reclaims.
	if err := metadata.DefaultCommitBlock(ctx, committer, rec, commits, nil); err != nil {
		return fmt.Errorf("carve: commit block %s: %w", blockID, err)
	}

	// Retire the committed chunks from the carve set and the backpressure
	// counter; count them for stats.
	m.pendingMu.Lock()
	for _, c := range commits {
		if size, ok := m.pendingCarveHashes[c.Hash]; ok {
			delete(m.pendingCarveHashes, c.Hash)
			m.unsyncedBytes.Add(-size)
		}
	}
	m.pendingMu.Unlock()
	m.completedSyncs.Add(int64(len(commits)))
	m.publishCarveQueueDepth()

	logger.Debug("Carve: committed block",
		"blockID", blockID, "chunks", len(commits), "bytes", len(blockBytes))
	return nil
}

// dropCarveHash removes a hash whose local bytes vanished from the carve set and
// the backpressure counter.
func (m *Syncer) dropCarveHash(h block.ContentHash) {
	m.pendingMu.Lock()
	if size, ok := m.pendingCarveHashes[h]; ok {
		delete(m.pendingCarveHashes, h)
		m.unsyncedBytes.Add(-size)
	}
	m.pendingMu.Unlock()
}

// CarvePendingCount returns the number of chunks awaiting carve into a block.
func (m *Syncer) CarvePendingCount() int {
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()
	return len(m.pendingCarveHashes)
}

// newBlockID returns a fresh, unguessable block object key. crypto/rand keeps it
// collision-free under concurrent carvers (unlike a timestamp) and unrelated to
// the block's content hash, so a re-carve after a crash always targets a new
// object.
func newBlockID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("carve: generate block id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

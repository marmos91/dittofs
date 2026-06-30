package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/blockcodec"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// addPendingCarveHash registers a freshly-completed log-blob chunk for the next
// carve pass. O(1); charges unsyncedBytes once per distinct hash (re-adding
// refreshes the recorded size only). Signals the carve dispatcher so packing
// overlaps the rollup of later chunks.
func (m *Syncer) addPendingCarveHash(h block.ContentHash, size int64) {
	m.pendingMu.Lock()
	prev, already := m.pendingCarveHashes[h]
	m.pendingCarveHashes[h] = size
	m.unsyncedBytes.Add(size - prev)
	if !already {
		m.carveQ = append(m.carveQ, h)
	}
	m.pendingMu.Unlock()

	if !already {
		m.publishCarveQueueDepth()
		m.signalCarveWake()
	}
}

// publishCarveQueueDepth folds the carve backlog into the upload-queue-depth
// gauge alongside the legacy pending set so the metric reflects all
// not-yet-durable chunks.
func (m *Syncer) publishCarveQueueDepth() {
	mx := m.dataplaneMetrics()
	if mx == nil {
		return
	}
	m.pendingMu.Lock()
	n := len(m.pendingHashes) + len(m.pendingCarveHashes)
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
	m.carveMu.Lock()
	defer m.carveMu.Unlock()

	target := m.carveBlockSize()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		batch := m.claimCarveBatch(target, drainAll)
		if len(batch) == 0 {
			return nil
		}
		if err := m.carveAndCommitBlock(ctx, batch); err != nil {
			// Return the batch to the queue for a later retry and surface the
			// error so an explicit Flush reports non-durability.
			m.requeueCarveBatch(batch)
			return err
		}
	}
}

// claimCarveBatch pops chunks from the carve FIFO (validating against the
// authoritative map) until their accumulated size reaches target, or the queue
// drains. When drainAll is false and the accumulated bytes are below target it
// returns nil — leaving the sub-target remainder pending — so the background
// loop never emits an undersized block ahead of the idle flush. Claimed hashes
// are removed from carveQ but remain in pendingCarveHashes until their block
// commits (or are requeued on failure). Caller holds carveMu (so no concurrent
// claim races the same chunks).
func (m *Syncer) claimCarveBatch(target int64, drainAll bool) []block.ContentHash {
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()

	var (
		batch []block.ContentHash
		total int64
	)
	for len(m.carveQ) > 0 {
		h := m.carveQ[0]
		m.carveQ = m.carveQ[1:]
		size, ok := m.pendingCarveHashes[h]
		if !ok {
			continue // already committed out from under the queue
		}
		batch = append(batch, h)
		total += size
		if total >= target {
			break
		}
	}
	if len(m.carveQ) == 0 {
		m.carveQ = nil // release the backing array once fully drained
	}
	if !drainAll && total < target {
		// Not enough for a full block yet: put the claimed hashes back at the
		// front (preserving order) and wait for more or the idle flush.
		if len(batch) > 0 {
			m.carveQ = append(batch, m.carveQ...)
		}
		return nil
	}
	return batch
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

// carveAndCommitBlock reads each chunk's raw bytes from the log-blob substrate,
// seals it through the per-chunk transform, frames it into one block via
// blockcodec, uploads the assembled block with PutBlock, and atomically commits
// the block record + per-chunk locators + synced markers. On success the
// committed hashes leave pendingCarveHashes and the bytes leave unsyncedBytes.
func (m *Syncer) carveAndCommitBlock(ctx context.Context, batch []block.ContentHash) error {
	m.mu.RLock()
	rbs := m.remoteBlockStore
	sealer := m.chunkSealer
	committer := m.blockCommitter
	reader := m.localBlobReader
	m.mu.RUnlock()
	if rbs == nil || committer == nil || reader == nil {
		return errors.New("carve: substrate not wired")
	}

	blockID, err := newBlockID()
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	builder, err := blockcodec.NewBuilder(&buf, blockID, nil)
	if err != nil {
		return fmt.Errorf("carve: new builder: %w", err)
	}

	commits := make([]block.BlockChunkCommit, 0, len(batch))
	for _, h := range batch {
		loc, ok, err := committer.GetLocalLocation(ctx, h)
		if err != nil {
			return fmt.Errorf("carve: get local location %s: %w", h, err)
		}
		if !ok {
			// Not log-blob-resident — a stray legacy CAS hash that was routed
			// here. Hand it to the legacy mirror path and drop it from carve.
			m.rerouteCarveMiss(h)
			continue
		}

		dst := make([]byte, loc.RawLength)
		if _, err := reader.ReadLocalAt(ctx, loc, dst); err != nil {
			if errors.Is(err, block.ErrChunkNotFound) {
				// The chunk's local bytes vanished before carve. Drop it from
				// the carve set; a write-again or T4 reconcile re-discovers it.
				logger.Error("carve: chunk bytes lost before carve — dropped", "hash", h.String())
				m.dropCarveHash(h)
				continue
			}
			return fmt.Errorf("carve: read local %s: %w", h, err)
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
		return nil // every chunk in the batch was rerouted/dropped
	}
	if _, err := builder.Finish(); err != nil {
		return fmt.Errorf("carve: finish block: %w", err)
	}

	blockBytes := buf.Bytes()
	blockHash := block.ContentHash(blake3.Sum256(blockBytes))

	// Upload the assembled block, then atomically commit. The order matters for
	// crash-safety: PutBlock first means a crash before the commit leaves an
	// orphan block (GC reclaims it) but never an unbacked record.
	if err := rbs.PutBlock(ctx, blockID, bytes.NewReader(blockBytes)); err != nil {
		m.uploadErrWindow.Add(1)
		return fmt.Errorf("carve: put block %s: %w", blockID, err)
	}

	rec := block.BlockRecord{
		BlockID:        blockID,
		BlockHash:      blockHash,
		Length:         int64(len(blockBytes)),
		LiveChunkCount: uint32(len(commits)),
		SyncState:      block.BlockStateRemote,
	}
	if err := metadata.DefaultCommitBlock(ctx, committer, rec, commits); err != nil {
		return fmt.Errorf("carve: commit block %s: %w", blockID, err)
	}

	// Retire the committed chunks from the carve set and the backpressure
	// counter; count them and the uploaded bytes for stats/adaptive feedback.
	m.pendingMu.Lock()
	for _, c := range commits {
		if size, ok := m.pendingCarveHashes[c.Hash]; ok {
			delete(m.pendingCarveHashes, c.Hash)
			m.unsyncedBytes.Add(-size)
		}
	}
	m.pendingMu.Unlock()
	m.completedSyncs.Add(int64(len(commits)))
	m.uploadedBytesWindow.Add(int64(len(blockBytes)))
	m.publishCarveQueueDepth()

	logger.Debug("Carve: committed block",
		"blockID", blockID, "chunks", len(commits), "bytes", len(blockBytes))
	return nil
}

// rerouteCarveMiss moves a hash that turned out not to be log-blob-resident from
// the carve set to the legacy standalone-mirror path.
func (m *Syncer) rerouteCarveMiss(h block.ContentHash) {
	m.pendingMu.Lock()
	size, ok := m.pendingCarveHashes[h]
	if ok {
		delete(m.pendingCarveHashes, h)
		// Hand to the legacy pending set; the dispatcher mirrors it as a
		// standalone CAS object. unsyncedBytes is unchanged (it moves between
		// sets, still counted once).
		if _, dup := m.pendingHashes[h]; !dup {
			m.pendingHashes[h] = size
			m.readyQ = append(m.readyQ, h)
		}
	}
	m.pendingMu.Unlock()
	if ok {
		logger.Warn("carve: non-log-blob hash rerouted to legacy mirror", "hash", h.String())
		m.signalWake()
	}
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

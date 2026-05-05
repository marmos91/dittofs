package engine

import (
	"context"
	"errors"
	"fmt"
	gosync "sync"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/local"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
	// API-02 justification: per-share BlockLayout enum gates the
	// dual-read shim inside dispatchRemoteFetch (Plan 14-02 / MIG-03 /
	// D-A8). The engine never opens a metadata txn against this type —
	// it is a config-time read of the value stamped on the share record
	// at AddShare time. Plan 15 (A6) removes the shim and this import.
	"github.com/marmos91/dittofs/pkg/metadata"
)

// defaultShutdownTimeout is the maximum time to wait for the transfer queue
// to finish processing during graceful shutdown.
const defaultShutdownTimeout = 30 * time.Second

// fetchResult is a broadcast-capable result for in-flight download deduplication.
// When the download completes, err is set and done is closed. Multiple waiters can
// safely read the result because closing a channel notifies ALL receivers.
type fetchResult struct {
	done chan struct{} // Closed when download completes
	err  error         // Result of the download (set before closing done)
	mu   gosync.Mutex  // Protects err during write
}

// Syncer handles async local-to-remote transfers with eager upload,
// parallel download, prefetch, in-flight dedup, and content-addressed dedup.
type Syncer struct {
	local          local.LocalStore
	remoteStore    remote.RemoteStore
	// Phase 12 (META-03 / D-09): the syncer is one of the engine-internal
	// callers that still reaches into the wider EngineFileBlockStore
	// surface (GetFileBlock for dual-read resolve, ListFileBlocks for
	// GetFileSize/Exists). Phase 13/14 routes reads through
	// FileAttr.Blocks and lets us drop the wider interface.
	fileBlockStore blockstore.EngineFileBlockStore // Required: enables content-addressed deduplication

	// coordinator is the post-Flush + file-level-dedup seam (Phase 12
	// D-37 / D-20; Phase 13 BSCAS-04 / BSCAS-05 / Plans 13-12 + 13-13).
	//
	// Wiring (production state, post-Plans 13-12 + 13-13):
	//   - Syncer.Flush invokes coordinator.GetFileObjectID + Syncer.
	//     TrySpeculativeFileLevelDedup BEFORE the per-block drain.
	//     On hit, applyFileLevelDedupHit performs RefCount swap +
	//     PersistFileBlocks + cache invalidate + log truncate (one
	//     metadata txn) and returns Finalized:true. Per-block pump is
	//     skipped — zero new CAS PUTs.
	//   - On miss, Syncer.Flush runs drainPayloadToRemote (per-block
	//     uploadOne pump) and finalizes with persistFileBlocksAfterFlush
	//     so FileAttr.Blocks AND FileAttr.ObjectID are written in one
	//     metadata txn via the runtime coordinator's PersistFileBlocks.
	//
	// uploadOne and SyncNow remain block-scoped — they do NOT invoke
	// the post-Flush hook. The hook is the responsibility of Syncer.
	// Flush, which has the per-payloadID context the hook needs.
	//
	// May be nil in unit tests; production callers always wire a real
	// coordinator via SetCoordinator.
	coordinator MetadataCoordinator

	// bs is a back-reference to the owning BlockStore. Phase 13 BSCAS-05
	// (Plan 07): the file-level dedup short-circuit needs to reach
	// BlockStore.cache to fire InvalidateFile on orphaned speculative
	// chunks. Reading through the back-reference (rather than copying a
	// `cache` field on the Syncer at construction time) lets test code
	// swap `bs.cache = rec` after construction and still observe the
	// invalidation — mirrors the TestClose_ClosesCache pattern. May be
	// nil in pre-wiring tests; callers must nil-check before use.
	bs *BlockStore

	config         SyncerConfig

	queue *SyncQueue // Transfer queue for non-blocking operations

	inFlight   map[string]*fetchResult // In-flight download dedup (store key -> broadcast)
	inFlightMu gosync.Mutex

	stopCh chan struct{} // Signals periodic uploader to stop
	closed bool
	mu     gosync.RWMutex

	periodicStarted bool        // true once periodicUploader goroutine is launched
	uploading       atomic.Bool // Guards against overlapping periodic upload ticks

	healthMonitor   *HealthMonitor           // Monitors remote store health (nil when no remote)
	onHealthChanged HealthTransitionCallback // Callback invoked on health state transitions

	firstOfflineRead    atomic.Bool  // Tracks if WARN was already logged since last healthy->unhealthy transition
	offlineReadsBlocked atomic.Int64 // Count of read operations blocked by remote unavailability

	// blockLayout is the per-share BlockLayout (legacy | cas-only) read
	// from the share's metadata.ShareOptions at AddShare time and frozen
	// for the lifetime of this Syncer. Plan 14-02 (MIG-03 / D-A6 / D-A8):
	// `cas-only` shares refuse the legacy fallback inside
	// dispatchRemoteFetch with ErrLegacyReadOnCASOnly. Plan 14-05 will
	// reload the engine on cutover so a runtime flip from legacy to
	// cas-only forces a fresh Syncer to pick up the new value.
	blockLayout metadata.BlockLayout
}

// NewSyncer creates a new Syncer. The fileBlockStore is required for content-addressed dedup.
func NewSyncer(local local.LocalStore, remoteStore remote.RemoteStore, fileBlockStore blockstore.EngineFileBlockStore, config SyncerConfig) *Syncer {
	if fileBlockStore == nil {
		panic("fileBlockStore is required for Syncer")
	}
	if config.ParallelUploads <= 0 {
		config.ParallelUploads = DefaultParallelUploads
	}
	if config.ParallelDownloads <= 0 {
		config.ParallelDownloads = DefaultParallelDownloads
	}
	if config.PrefetchBlocks <= 0 {
		config.PrefetchBlocks = DefaultPrefetchBlocks
	}
	// Phase 11 Plan 02 (D-13/D-14/D-25) — apply CAS-path defaults.
	if config.ClaimBatchSize <= 0 {
		config.ClaimBatchSize = 32
	}
	if config.UploadConcurrency <= 0 {
		config.UploadConcurrency = 8
	}
	if config.ClaimTimeout <= 0 {
		config.ClaimTimeout = 10 * time.Minute
	}

	// Plan 14-02 (D-A6): coerce empty/unknown BlockLayout to legacy at
	// construction time so pre-Phase-14 callers (and any path that
	// forgets to thread the field) keep the dual-read shim active. The
	// metadata layer already enforces the same coercion via
	// ParseBlockLayout on the read path; the engine repeats it as
	// defense-in-depth so a zero-valued SyncerConfig.BlockLayout never
	// surfaces as "" inside dispatchRemoteFetch.
	layout := config.BlockLayout
	if layout != metadata.BlockLayoutLegacy && layout != metadata.BlockLayoutCASOnly {
		layout = metadata.BlockLayoutLegacy
	}

	m := &Syncer{
		local:          local,
		remoteStore:    remoteStore,
		fileBlockStore: fileBlockStore,
		config:         config,
		inFlight:       make(map[string]*fetchResult),
		stopCh:         make(chan struct{}),
		blockLayout:    layout,
	}

	queueConfig := DefaultSyncQueueConfig()
	queueConfig.Workers = config.ParallelUploads
	queueConfig.DownloadWorkers = config.ParallelDownloads
	m.queue = NewSyncQueue(m, queueConfig)

	return m
}

// Queue returns the transfer queue for stats inspection.
func (m *Syncer) Queue() *SyncQueue { return m.queue }

// BlockLayout returns the per-share BlockLayout this Syncer was
// constructed with. Plan 14-02 (MIG-03): exposes the engine's view of
// the share's block-key scheme for the runtime auto-cutover path
// (Plan 14-05) and for wiring/observability tests. The value is frozen
// at NewSyncer time and never mutated during the Syncer's lifetime —
// runtime flips require an engine reload.
func (m *Syncer) BlockLayout() metadata.BlockLayout { return m.blockLayout }

// SetCoordinator wires the MetadataCoordinator the post-Flush path
// invokes (Phase 12 D-37 / D-20). engine.New plumbs the BlockStore's
// coordinator into the syncer so PersistFileBlocks runs in the
// caller's metadata txn after each successful uploadOne batch. Idempotent.
func (m *Syncer) SetCoordinator(c MetadataCoordinator) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.coordinator = c
}

// TrySpeculativeFileLevelDedup is the public seam for the Phase 13
// BSCAS-05 file-level dedup short-circuit. Higher layers (the per-share
// adapter-common Flush path that owns the chunker output) MUST invoke
// this BEFORE any per-block GetByHash + WriteBlockWithHash loop:
//
//	hit, err := bs.Syncer().TrySpeculativeFileLevelDedup(
//	    ctx, payloadID, speculativeBlocks, currentObjectID, blockStates,
//	)
//	if err != nil { return err }
//	if hit { return nil }       // upload bypassed — target re-used
//	// fall through to per-block upload path
//
// On hit the file's BlockRef list is replaced with the target's,
// refcounts are swapped under the caller's metadata txn, the per-file
// append log is truncated, and the cache is invalidated for orphaned
// speculative chunks. On miss, the post-Flush coordinator hook
// (persistFileBlocksAfterFlush) finalizes the ObjectID after the
// per-block path completes.
//
// The current per-FileBlock claimBatch + uploadOne loop is not the
// natural call site (it has no per-file BlockRef context); the
// integration point lives in the adapter-common write path that already
// owns the FastCDC chunker output. This public method exposes the
// engine seam so that path can drive the short-circuit without reaching
// into private symbols.
func (m *Syncer) TrySpeculativeFileLevelDedup(
	ctx context.Context,
	payloadID string,
	speculativeBlocks []blockstore.BlockRef,
	fileObjectID blockstore.ObjectID,
	blockStates []blockstore.BlockState,
) (bool, error) {
	return m.trySpeculativeFileLevelDedup(ctx, payloadID, speculativeBlocks, fileObjectID, blockStates)
}

// persistFileBlocksAfterFlush is the post-Flush hook (Phase 13 D-05).
// Invokes coordinator.PersistFileBlocks with the BlockRef list AND the
// BLAKE3 Merkle-root ObjectID computed from those blocks.
//
// D-06 invariant: this hook fires ONLY when every block is Remote
// (full quiesce). Partial flushes never reach here — Flush() returns
// Finalized:false. The ObjectID written here therefore always reflects
// a fully-Remote consistent state (no in-flight blocks).
//
// Empty blocks list: ComputeObjectID returns the canonical empty-file
// constant (BLAKE3 of the domain-separation prefix alone — D-03). The
// runtime coordinator writes whatever the syncer passed; the lookup
// index treats only all-zero ObjectIDs as "never quiesced", so the
// canonical empty-file fingerprint is a real, queryable identity.
//
// Phase 13 D-20: success path logs at DEBUG (matches Phase 11/12
// cadence; no new Prometheus surface this phase).
//
// WR-04 (Phase 13 review iteration 1): the runtime coordinator's
// PersistFileBlocks is now fully wired (CR-01) so
// ErrPersistFileBlocksNotWired should never surface in production. We
// keep the sentinel itself as a defensive type — but we no longer
// swallow it: a wired coordinator returning NotWired indicates a real
// regression (e.g., a future refactor reintroducing the stub) and the
// caller MUST see it as a hard error rather than a per-Flush log line.
func (m *Syncer) persistFileBlocksAfterFlush(ctx context.Context, payloadID string, blocks []blockstore.BlockRef) error {
	if m.coordinator == nil {
		return nil
	}
	objectID := blockstore.ComputeObjectID(blocks)
	err := m.coordinator.PersistFileBlocks(ctx, payloadID, blocks, objectID)
	if err == nil {
		logger.Debug("post-flush ObjectID persisted",
			"payloadID", payloadID,
			"blocks", len(blocks),
			"objectID", objectID.String())
	}
	return err
}

// SetHealthCallback sets the callback invoked when the remote store health state changes.
// If the HealthMonitor is already running, the callback is forwarded to it immediately.
func (m *Syncer) SetHealthCallback(fn HealthTransitionCallback) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onHealthChanged = fn
	if m.healthMonitor != nil {
		m.healthMonitor.SetTransitionCallback(fn)
	}
}

// IsRemoteHealthy returns the health state of the remote store.
// Returns true when there is no HealthMonitor (local-only mode).
func (m *Syncer) IsRemoteHealthy() bool {
	if m.healthMonitor == nil {
		return true
	}
	return m.healthMonitor.IsHealthy()
}

// RemoteOutageDuration returns how long the remote store has been unhealthy.
// Returns 0 when healthy or when there is no HealthMonitor.
func (m *Syncer) RemoteOutageDuration() time.Duration {
	if m.healthMonitor == nil {
		return 0
	}
	return m.healthMonitor.OutageDuration()
}

// remoteUnavailableError returns an ErrRemoteUnavailable wrapped with outage duration context.
func (m *Syncer) remoteUnavailableError() error {
	dur := m.RemoteOutageDuration()
	return fmt.Errorf("remote store unavailable (offline for %s): %w", dur.Truncate(time.Second), blockstore.ErrRemoteUnavailable)
}

// OfflineReadsBlocked returns the count of read operations that failed
// because the requested blocks were remote-only during an outage.
func (m *Syncer) OfflineReadsBlocked() int64 {
	return m.offlineReadsBlocked.Load()
}

// logOfflineRead logs a read failure due to remote unavailability.
// First failure after a healthy->unhealthy transition logs at WARN level;
// subsequent failures log at DEBUG to avoid log spam.
func (m *Syncer) logOfflineRead(method, payloadID string, blockIdx uint64) {
	if m.firstOfflineRead.CompareAndSwap(false, true) {
		logger.Warn("Read blocked: remote store unavailable",
			"method", method,
			"payloadID", payloadID,
			"blockIdx", blockIdx,
			"outage_duration", m.RemoteOutageDuration().Truncate(time.Second))
	} else {
		logger.Debug("Read blocked: remote store unavailable",
			"method", method,
			"payloadID", payloadID,
			"blockIdx", blockIdx)
	}
}

// checkReady returns nil if the syncer can process requests.
// Returns ctx.Err() if the context is cancelled, or ErrClosed if the syncer is closed.
func (m *Syncer) checkReady(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return ErrClosed
	}
	return nil
}

// canProcess returns false if the syncer is closed or context is cancelled.
func (m *Syncer) canProcess(ctx context.Context) bool {
	return m.checkReady(ctx) == nil
}

// Flush writes dirty in-memory blocks to local store, optionally
// short-circuits the per-block upload pump via the file-level dedup
// path (Phase 13 BSCAS-05) when the D-09 trigger condition holds, and
// otherwise drains every Pending/Syncing block belonging to payloadID
// to Remote and invokes the post-Flush coordinator hook so
// FileAttr.Blocks AND FileAttr.ObjectID are persisted in a single
// metadata txn (CR-01 wired the runtime coordinator; Plan 13-12 closed
// the post-Flush hop; Plan 13-13 closes the file-level dedup hop —
// see 13-VERIFICATION.md must-haves #1/#2 / Phase 13 D-05 / D-06 /
// D-09 / D-10).
//
// D-06 invariant: the post-Flush hook fires ONLY on full quiesce
// (every block of payloadID is Remote). On any drain failure, the
// method returns the error without invoking the hook —
// FileAttr.ObjectID remains at its prior value (zero for fresh
// files; the previous Merkle root for re-flushed files). The next
// successful Flush recomputes.
//
// D-09 trigger (file-level dedup short-circuit): when
// len(speculativeBlocks)>0 AND every block.State==Pending AND the
// file's prior ObjectID is zero, the syncer computes a provisional
// Merkle root over the Pending FileBlock projection and consults
// FindByObjectID. On hit, the metadata-side swap (RefCount++ on
// target hashes, FileAttr.Blocks/ObjectID write, log truncation)
// commits inside applyFileLevelDedupHit and the per-block upload pump
// is BYPASSED entirely (zero new CAS PUTs). On miss / partial state
// the per-block path runs as before.
//
// The drain loop mirrors SyncNow's claim-and-upload shape but is
// scoped to payloadID via ListFileBlocks (Phase 12 D-08). When there
// are no blocks for payloadID the method short-circuits to a no-op
// success (a pre-write Flush, e.g. CLOSE on an opened-but-untouched
// file, must not error — the runtime coordinator's PersistFileBlocks
// would reject the unknown payloadID).
func (m *Syncer) Flush(ctx context.Context, payloadID string) (*blockstore.FlushResult, error) {
	if err := m.checkReady(ctx); err != nil {
		return nil, err
	}

	// 1. Local-side flush (existing behavior — moves dirty in-memory
	//    state to .blk files; rollup pump may run async). Unchanged.
	if _, err := m.local.Flush(ctx, payloadID); err != nil {
		return nil, fmt.Errorf("local store flush failed: %w", err)
	}

	// 2. Phase 13 BSCAS-05 (Plan 13-13): file-level dedup short-circuit
	//    BEFORE the per-block upload pump. Trigger condition (D-09) is
	//    enforced inside trySpeculativeFileLevelDedup:
	//      - len(speculativeBlocks) > 0
	//      - every block.State == Pending
	//      - fileObjectID == zero (file never quiesced)
	//    On hit, applyFileLevelDedupHit commits the metadata swap and
	//    the per-block drain is bypassed entirely (zero CAS PUTs). On
	//    miss the path falls through to the existing per-block drain +
	//    post-Flush hook so FileAttr.Blocks/ObjectID are still
	//    finalized at the end of Flush.
	//
	//    The lookup is gated on a non-nil coordinator: pre-wiring
	//    test fixtures (e.g. TestSyncer_Flush_NilCoordinatorIsNoop)
	//    must continue to behave as a no-op.
	if m.coordinator != nil {
		specBlocks, blockStates, err := m.snapshotPendingBlockRefs(ctx, payloadID)
		if err != nil {
			return nil, fmt.Errorf("snapshot pending block refs for %s: %w", payloadID, err)
		}
		if len(specBlocks) > 0 {
			fileObjectID, err := m.coordinator.GetFileObjectID(ctx, payloadID)
			if err != nil {
				return nil, fmt.Errorf("get file ObjectID for %s: %w", payloadID, err)
			}
			hit, err := m.TrySpeculativeFileLevelDedup(ctx, payloadID, specBlocks, fileObjectID, blockStates)
			if err != nil {
				return nil, fmt.Errorf("file-level dedup attempt for %s: %w", payloadID, err)
			}
			if hit {
				// applyFileLevelDedupHit has already committed the
				// metadata swap (Blocks + ObjectID + RefCount++ on
				// target hashes + best-effort decrement on
				// speculative-only hashes + cache invalidation +
				// append-log truncation). The per-block drain is
				// bypassed; no CAS PUTs were issued.
				return &blockstore.FlushResult{Finalized: true}, nil
			}
		}
	}

	// 3. Drain every Pending/Syncing block belonging to payloadID to
	//    Remote. Bounded loop; re-queries each pass to absorb
	//    concurrent appends from a periodic uploader tick.
	if err := m.drainPayloadToRemote(ctx, payloadID); err != nil {
		return nil, fmt.Errorf("drain payload %s to remote: %w", payloadID, err)
	}

	// 4. Build canonical sorted-by-Offset BlockRef snapshot
	//    (D-01 / Phase 12 META-01 D-10). ListFileBlocks already
	//    returns blocks ordered by block index ascending — which is
	//    Offset ascending given Offset = blockIdx*BlockSize.
	blocks, err := m.snapshotBlockRefs(ctx, payloadID)
	if err != nil {
		return nil, fmt.Errorf("snapshot block refs for %s: %w", payloadID, err)
	}
	if len(blocks) == 0 {
		// No blocks belong to this payload — nothing to quiesce.
		// Silent skip preserves the no-op semantics for pre-write
		// Flushes (the coordinator would error on an unknown
		// payloadID).
		return &blockstore.FlushResult{Finalized: false}, nil
	}

	// 5. Post-Flush hook: persist FileAttr.Blocks AND
	//    FileAttr.ObjectID in one metadata txn (the runtime
	//    coordinator owns the txn).
	if err := m.persistFileBlocksAfterFlush(ctx, payloadID, blocks); err != nil {
		return nil, fmt.Errorf("post-flush metadata persist for %s: %w", payloadID, err)
	}
	return &blockstore.FlushResult{Finalized: true}, nil
}

// MaxFlushPasses is the upper bound on drain-loop passes
// drainPayloadToRemote performs before declaring non-convergence. Each
// pass drains every Pending/Syncing block currently visible to
// ListFileBlocks(payloadID); the loop re-queries to absorb concurrent
// appends. 16 passes accommodates a periodic uploader tick interleaving
// with bounded in-progress writers; if the set is still non-empty after
// that, the periodic janitor will reconcile.
const MaxFlushPasses = 16

// drainPayloadToRemote synchronously walks every FileBlock belonging to
// payloadID and ensures each reaches BlockStateRemote. Returns nil on
// full quiesce; on any uploadOne error returns immediately so the
// caller (Flush) propagates without firing the post-Flush hook. Per
// D-14, idempotency of CAS keys makes any rolled-back row safe to
// re-upload on the next Flush.
//
// WR-01 (Phase 13 review iteration 2 — deliberate non-serialization):
// unlike SyncNow, this drain does NOT acquire the m.uploading
// CompareAndSwap gate. A periodic uploader tick may race with this
// drain on the same payloadID — both can observe a Pending row, claim
// it (one via this drain's per-row Put, the other via claimBatch), and
// both call uploadOne. This is TOLERATED by the same contract that
// permits cross-syncer races on claimBatch (see claimBatch's
// "Serialization scope (D-13)" doc): CAS keys are content-defined
// (D-11 / INV-03), so the duplicate PUT is byte-identical at a
// byte-identical key and the metadata Put is idempotent. Acquiring
// m.uploading here would block the periodic uploader for the entire
// drain (potentially many passes on a slow remote) for no correctness
// gain — the cost of the rare duplicate PUT is bounded and well
// understood.
//
// The per-row guard at line ~457 (`if fb.State == BlockStatePending`)
// avoids re-claiming a row a concurrent uploader has just flipped to
// Syncing; uploadOne is then invoked on that Syncing row, producing
// the at-most-twice upload described above.
func (m *Syncer) drainPayloadToRemote(ctx context.Context, payloadID string) error {
	for pass := 0; pass < MaxFlushPasses; pass++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		blocks, err := m.fileBlockStore.ListFileBlocks(ctx, payloadID)
		if err != nil {
			return fmt.Errorf("list file blocks: %w", err)
		}
		allRemote := true
		for _, fb := range blocks {
			if fb.State == blockstore.BlockStateRemote {
				continue
			}
			allRemote = false
			// Flip Pending → Syncing in metadata so the row is
			// owned by this drain pass (mirrors claimBatch).
			// Idempotent for already-Syncing rows (uploadOne also
			// rejects non-Syncing rows; the explicit transition
			// keeps the contract local).
			if fb.State == blockstore.BlockStatePending {
				fb.State = blockstore.BlockStateSyncing
				fb.LastSyncAttemptAt = time.Now()
				if err := m.fileBlockStore.Put(ctx, fb); err != nil {
					return fmt.Errorf("claim block %s: %w", fb.ID, err)
				}
			}
			if err := m.uploadOne(ctx, fb); err != nil {
				return fmt.Errorf("upload block %s: %w", fb.ID, err)
			}
		}
		if allRemote {
			return nil
		}
	}
	return fmt.Errorf("drain did not converge after %d passes for payload %s", MaxFlushPasses, payloadID)
}

// snapshotPendingBlockRefs returns the speculativeBlocks list +
// parallel blockStates slice for the Phase 13 BSCAS-05 / D-09 trigger
// evaluation. Projection: ListFileBlocks(payloadID) yields the FastCDC
// chunker output already produced by the local-store rollup
// (pkg/blockstore/local/fs/rollup.go::rollupFile populates Pending
// FileBlocks at chunk boundaries with the BLAKE3-256 chunk hash as
// FileBlock.Hash). Phase 13 D-09 expects the trigger to fire only when
// every projected block is Pending; this helper returns the FULL
// projection (every block, regardless of state) so the trigger
// guard inside trySpeculativeFileLevelDedup can veto on any non-Pending
// row without an extra read.
//
// The returned slice is in block-index ascending order (= Offset
// ascending given Offset = blockIdx * BlockSize), so
// ComputeObjectID(refs) over the result matches the canonical Merkle
// root the post-Flush hook would compute over the same blocks once
// they reach Remote.
//
// Skips rows whose ID does not parse as a {payloadID}/{idx} pair —
// such rows are foreign to this payload (defense-in-depth; the
// per-payload index returned by ListFileBlocks should never include
// them but the parse check guarantees correctness if a future change
// widens the scan).
func (m *Syncer) snapshotPendingBlockRefs(ctx context.Context, payloadID string) ([]blockstore.BlockRef, []blockstore.BlockState, error) {
	blocks, err := m.fileBlockStore.ListFileBlocks(ctx, payloadID)
	if err != nil {
		return nil, nil, err
	}
	refs := make([]blockstore.BlockRef, 0, len(blocks))
	states := make([]blockstore.BlockState, 0, len(blocks))
	for _, fb := range blocks {
		_, blockIdx, ok := parseBlockID(fb.ID, payloadID)
		if !ok {
			continue
		}
		refs = append(refs, blockstore.BlockRef{
			Hash:   fb.Hash,
			Offset: blockIdx * uint64(BlockSize),
			Size:   fb.DataSize,
		})
		states = append(states, fb.State)
	}
	return refs, states, nil
}

// snapshotBlockRefs returns the canonical sorted-by-Offset BlockRef
// list for payloadID at the moment of the call. Built from the current
// ListFileBlocks() projection: Offset = blockIdx*BlockSize, Size =
// DataSize. Caller MUST have ensured every block is Remote (the
// drainPayloadToRemote precondition) — Pending blocks have an empty
// Hash and would corrupt the Merkle root.
//
// ListFileBlocks already returns blocks ordered by block index
// ascending → ascending Offset; no defensive sort is added here. If
// the contract ever drifts the storetest BlockRef SortStability
// scenario catches it before the engine sees the misordered slice.
func (m *Syncer) snapshotBlockRefs(ctx context.Context, payloadID string) ([]blockstore.BlockRef, error) {
	blocks, err := m.fileBlockStore.ListFileBlocks(ctx, payloadID)
	if err != nil {
		return nil, err
	}
	out := make([]blockstore.BlockRef, 0, len(blocks))
	for _, fb := range blocks {
		// WR-03 (Phase 13 review iteration 2): parse the block ID FIRST
		// so the precondition error below can only mention rows that
		// genuinely belong to this payload. A foreign-payload row that
		// somehow surfaced in ListFileBlocks(payloadID) is silently
		// skipped here regardless of its State, mirroring the structure
		// of snapshotPendingBlockRefs.
		_, blockIdx, ok := parseBlockID(fb.ID, payloadID)
		if !ok {
			continue
		}
		if fb.State != blockstore.BlockStateRemote {
			return nil, fmt.Errorf("snapshot precondition violated: block %s is %v, expected Remote",
				fb.ID, fb.State)
		}
		out = append(out, blockstore.BlockRef{
			Hash:   fb.Hash,
			Offset: blockIdx * uint64(BlockSize),
			Size:   fb.DataSize,
		})
	}
	return out, nil
}

// DrainAllUploads performs an immediate synchronous upload of every local
// block to remote, bypassing the UploadDelay. Returns nil when every block
// reached remote, ctx.Err() on cancellation, or an aggregated error naming
// the blocks that failed to upload.
//
// Exposed via the REST API for the benchmark runner to call between test
// phases, and used by Close() to ensure no blocks are left stranded in the
// local store at shutdown.
func (m *Syncer) DrainAllUploads(ctx context.Context) error {
	if err := m.SyncNow(ctx); err != nil {
		return err
	}
	return ctx.Err()
}

// GetFileSize returns the total size of a file from the remote store.
//
// Phase 11 (CAS): blocks are stored under content-addressed keys
// (cas/XX/YY/<hash>), so the legacy {payloadID}/block-{N} prefix scan no
// longer finds them. We resolve via FileBlock metadata: enumerate every
// block belonging to payloadID, find the highest-indexed Remote block,
// and compute size = maxIdx*BlockSize + lastBlock.DataSize. DataSize is
// stamped by uploadOne before flipping State to Remote, so no extra S3
// round-trip is needed.
func (m *Syncer) GetFileSize(ctx context.Context, payloadID string) (uint64, error) {
	if err := m.checkReady(ctx); err != nil {
		return 0, err
	}

	if m.remoteStore == nil {
		logger.Debug("syncer: skipping GetFileSize, no remote store")
		return 0, nil
	}

	// Health gate: fail fast when remote is unreachable
	if !m.IsRemoteHealthy() {
		return 0, m.remoteUnavailableError()
	}

	blocks, err := m.fileBlockStore.ListFileBlocks(ctx, payloadID)
	if err != nil {
		return 0, fmt.Errorf("list file blocks for %s: %w", payloadID, err)
	}
	if len(blocks) == 0 {
		return 0, nil
	}

	// ListFileBlocks returns blocks ordered by block index. Walk from the
	// end to find the highest-indexed Remote block (skip any trailing
	// Pending/Syncing rows that haven't been confirmed in the remote
	// store yet — they MUST NOT contribute to the remote-side size).
	for i := len(blocks) - 1; i >= 0; i-- {
		fb := blocks[i]
		if fb.State != blockstore.BlockStateRemote {
			continue
		}
		_, blockIdx, ok := parseBlockID(fb.ID, payloadID)
		if !ok {
			continue
		}
		return blockIdx*uint64(BlockSize) + uint64(fb.DataSize), nil
	}
	return 0, nil
}

// Exists checks if any blocks exist for a file in the remote store.
//
// Phase 11 (CAS): metadata-driven existence check. Returns true iff at
// least one FileBlock for payloadID has reached BlockStateRemote (i.e.
// the engine has confirmed at least one PUT into CAS storage). Pending
// and Syncing rows do NOT count — those are still local-only.
func (m *Syncer) Exists(ctx context.Context, payloadID string) (bool, error) {
	if err := m.checkReady(ctx); err != nil {
		return false, err
	}
	if m.remoteStore == nil {
		logger.Debug("syncer: skipping Exists, no remote store")
		return false, nil
	}

	// Health gate: fail fast when remote is unreachable
	if !m.IsRemoteHealthy() {
		return false, m.remoteUnavailableError()
	}

	blocks, err := m.fileBlockStore.ListFileBlocks(ctx, payloadID)
	if err != nil {
		return false, fmt.Errorf("list file blocks for %s: %w", payloadID, err)
	}
	for _, fb := range blocks {
		if fb.State == blockstore.BlockStateRemote {
			return true, nil
		}
	}
	return false, nil
}

// parseBlockID extracts the block index from a FileBlock.ID of the form
// "{payloadID}/{blockIdx}". Returns (payloadID, blockIdx, true) on a
// well-formed match for the expected payloadID; (_, 0, false) otherwise.
func parseBlockID(blockID, expectedPayloadID string) (string, uint64, bool) {
	prefix := expectedPayloadID + "/"
	if len(blockID) <= len(prefix) || blockID[:len(prefix)] != prefix {
		return "", 0, false
	}
	var idx uint64
	for _, c := range blockID[len(prefix):] {
		if c < '0' || c > '9' {
			return "", 0, false
		}
		idx = idx*10 + uint64(c-'0')
	}
	return expectedPayloadID, idx, true
}

// Truncate removes blocks beyond the new size from the remote store.
func (m *Syncer) Truncate(ctx context.Context, payloadID string, newSize uint64) error {
	if err := m.checkReady(ctx); err != nil {
		return err
	}
	if m.remoteStore == nil {
		logger.Debug("syncer: skipping Truncate, no remote store")
		return nil
	}
	// Health gate: skip remote cleanup when unhealthy. Local cache is the
	// source of truth for metadata; remote orphans are cleaned up later.
	if !m.IsRemoteHealthy() {
		logger.Warn("Truncate: skipping remote cleanup, remote store unhealthy",
			"payloadID", payloadID, "newSize", newSize)
		return nil
	}

	prefix := payloadID + "/"
	if newSize == 0 {
		return m.remoteStore.DeleteByPrefix(ctx, prefix)
	}

	keepBlockIdx := (newSize - 1) / uint64(BlockSize)

	blocks, err := m.remoteStore.ListByPrefix(ctx, prefix)
	if err != nil {
		return fmt.Errorf("list blocks: %w", err)
	}

	for _, bk := range blocks {
		pid, blockIdx, ok := blockstore.ParseStoreKey(bk)
		if !ok || pid != payloadID {
			continue
		}
		if blockIdx > keepBlockIdx {
			if err := m.remoteStore.DeleteBlock(ctx, bk); err != nil {
				return fmt.Errorf("delete block %s: %w", bk, err)
			}
		}
	}

	return nil
}

// Delete removes all blocks for a file from the remote store.
func (m *Syncer) Delete(ctx context.Context, payloadID string) error {
	if err := m.checkReady(ctx); err != nil {
		return err
	}

	if m.remoteStore == nil {
		logger.Debug("syncer: skipping Delete, no remote store")
		return nil
	}
	// Health gate: skip remote cleanup when unhealthy. Remote blocks become
	// orphans that garbage collection will clean up after recovery.
	if !m.IsRemoteHealthy() {
		logger.Warn("Delete: skipping remote cleanup, remote store unhealthy",
			"payloadID", payloadID)
		return nil
	}

	return m.remoteStore.DeleteByPrefix(ctx, payloadID+"/")
}

// Start begins background upload processing and periodic uploader.
// Must be called after New() to enable async uploads.
// When remoteStore is nil (local-only mode), the periodic syncer is skipped.
func (m *Syncer) Start(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.queue != nil {
		m.queue.Start(ctx)
	}

	if m.remoteStore == nil {
		logger.Info("Syncer started in local-only mode (no remote store)")
		return
	}

	// Phase 11 D-14: one-shot janitor pass before the periodic uploader
	// starts. Requeues Syncing rows abandoned by a previous instance.
	// Failure here is logged at WARN — a bad metadata read should not
	// prevent the syncer from running its periodic loop.
	if err := m.recoverStaleSyncing(ctx); err != nil {
		logger.Warn("Syncer janitor: recoverStaleSyncing failed", "error", err)
	}

	m.startHealthMonitor(ctx)
	m.startPeriodicUploader(ctx)
}

// startHealthMonitor creates and starts the health monitor for the remote store.
// Must be called with m.mu held.
func (m *Syncer) startHealthMonitor(ctx context.Context) {
	m.healthMonitor = NewHealthMonitor(m.remoteStore.HealthCheck, m.config)
	// Wrap the user's callback to also reset the offline-read WARN flag
	// on each healthy->unhealthy transition.
	userCallback := m.onHealthChanged
	m.healthMonitor.SetTransitionCallback(func(healthy bool) {
		if !healthy {
			m.firstOfflineRead.Store(false)
		}
		if userCallback != nil {
			userCallback(healthy)
		}
	})
	m.healthMonitor.Start(ctx)
}

// startPeriodicUploader launches the periodic uploader goroutine if not already running.
// Must be called with m.mu held.
func (m *Syncer) startPeriodicUploader(ctx context.Context) {
	if m.periodicStarted {
		return
	}
	m.periodicStarted = true

	interval := m.config.UploadInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	go m.periodicUploader(ctx, interval)
}

// SyncNow triggers an immediate upload cycle for all local blocks,
// bypassing any age filter. Blocks until all eligible blocks are uploaded
// or the context is cancelled. Returns nil on full success, ctx.Err() on
// cancellation, or a joined error listing every block that failed to upload —
// callers such as the REST /drain-uploads endpoint and Close() rely on this
// signal.
//
// Phase 11 Plan 02 (D-13/D-15/D-25): each cycle calls claimBatch to flip up
// to ClaimBatchSize Pending rows to Syncing via per-row PutFileBlock writes
// (no batched/transactional FileBlockStore API exists today — Phase 12+).
// Each row's CAS Pending→Syncing flip is atomic on its own, but the batch
// is NOT collectively atomic; a syncer crash mid-batch leaves a mix of
// Syncing and Pending rows. CAS idempotency tolerates that: on restart,
// reconciler reclaims orphaned Syncing rows back to Pending, and a second
// uploadOne over the same hash is a no-op against the immutable CAS object.
// A bounded pool of UploadConcurrency goroutines then drives uploadOne for
// every claimed block. The cycle repeats until claimBatch returns an empty
// batch (no more Pending work).
//
// SyncNow also serializes against the periodic uploader via the m.uploading
// gate so concurrent callers do not double-claim the same row.
func (m *Syncer) SyncNow(ctx context.Context) error {
	if m.remoteStore == nil {
		return nil
	}
	for !m.uploading.CompareAndSwap(false, true) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer m.uploading.Store(false)

	// Flush queued FileBlock metadata to the store so ListLocalBlocks can find them.
	m.local.SyncFileBlocks(ctx)

	var uploadErrs []error
	for {
		if err := ctx.Err(); err != nil {
			if len(uploadErrs) > 0 {
				return errors.Join(append(uploadErrs, err)...)
			}
			return err
		}
		batch, err := m.claimBatch(ctx, m.config.ClaimBatchSize)
		if err != nil {
			return fmt.Errorf("claim batch: %w", err)
		}
		if len(batch) == 0 {
			break
		}

		// Bounded share-wide upload pool (D-25).
		sem := make(chan struct{}, m.config.UploadConcurrency)
		var wg gosync.WaitGroup
		var errMu gosync.Mutex
		for _, fb := range batch {
			fb := fb
			if fb.LocalPath == "" {
				continue
			}
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				if err := m.uploadOne(ctx, fb); err != nil {
					// Per D-14, the row is left in Syncing on failure —
					// the janitor (or the next SyncNow after ClaimTimeout)
					// will requeue it. Logged at DEBUG.
					logger.Debug("uploadOne failed; row remains Syncing",
						"blockID", fb.ID, "error", err)
					errMu.Lock()
					uploadErrs = append(uploadErrs, err)
					errMu.Unlock()
				}
			}()
		}
		wg.Wait()
	}
	return errors.Join(uploadErrs...)
}

// claimBatch flips up to max Pending blocks to Syncing via per-row
// PutFileBlock writes (FileBlockStore exposes no batched/transactional
// claim API today; Phase 12+). Each row's transition is atomic on its own
// but the batch is NOT collectively atomic — a syncer crash mid-batch
// leaves a mix of Syncing and Pending rows. CAS idempotency tolerates the
// resulting partial state: recoverStaleSyncing returns the abandoned
// Syncing rows to Pending, and any duplicate uploadOne over the same hash
// is a no-op against the immutable CAS object. Stamps
// LastSyncAttemptAt = now on every claimed row.
//
// Serialization scope (D-13): WITHIN ONE syncer instance, PutFileBlock is
// applied per row before the next iteration sees it, so two concurrent
// claimBatch callers in the same process cannot both observe + claim the
// same row (the m.uploading gate also serializes SyncNow against the
// periodic uploader at a coarser layer).
//
// ACROSS syncer instances (multi-process / multi-node) this method does
// NOT serialize: two syncers can each ListLocalBlocks the same Pending
// row before either calls PutFileBlock, both flip it to Syncing, and
// both upload. This is TOLERATED because CAS keys are content-defined
// (D-11 / INV-03) — the duplicate PUT is byte-identical to the same
// key and the second PutFileBlock simply overwrites the first with the
// same payload. A future ChangeStream / SELECT FOR UPDATE / WHERE-state
// guard could close the cross-process window without coordination, but
// the CAS idempotency makes the current contract correct.
func (m *Syncer) claimBatch(ctx context.Context, max int) ([]*blockstore.FileBlock, error) {
	if max <= 0 {
		max = m.config.ClaimBatchSize
	}
	pending, err := m.fileBlockStore.ListPending(ctx, 0, max)
	if err != nil {
		return nil, fmt.Errorf("list local blocks: %w", err)
	}
	if len(pending) == 0 {
		return nil, nil
	}
	now := time.Now()
	claimed := make([]*blockstore.FileBlock, 0, len(pending))
	for _, fb := range pending {
		if fb == nil || fb.State != blockstore.BlockStatePending {
			continue
		}
		fb.State = blockstore.BlockStateSyncing
		fb.LastSyncAttemptAt = now
		if err := m.fileBlockStore.Put(ctx, fb); err != nil {
			return nil, fmt.Errorf("claim block %s: %w", fb.ID, err)
		}
		claimed = append(claimed, fb)
	}
	return claimed, nil
}

// recoverStaleSyncing requeues blocks left in Syncing by a previous run
// (e.g., process killed mid-upload). Per D-14, any Syncing row whose
// LastSyncAttemptAt is older than cfg.ClaimTimeout is flipped back to
// Pending with LastSyncAttemptAt cleared. CAS idempotency makes the
// re-upload safe even if the original upload eventually completes — both
// writes target byte-identical bytes at byte-identical keys.
//
// Backends that opt in to syncingEnumerator return precise candidates;
// others degrade to a no-op (safe — claimBatch will not double-claim a
// Syncing row).
func (m *Syncer) recoverStaleSyncing(ctx context.Context) error {
	if m.fileBlockStore == nil {
		return nil
	}
	enum, ok := m.fileBlockStore.(syncingEnumerator)
	if !ok {
		return nil
	}
	candidates, err := enum.EnumerateSyncingBlocks(ctx)
	if err != nil {
		return fmt.Errorf("enumerate syncing blocks: %w", err)
	}
	cutoff := time.Now().Add(-m.config.ClaimTimeout)
	requeued := 0
	failed := 0
	var firstErr error
	for _, fb := range candidates {
		if fb.State != blockstore.BlockStateSyncing {
			continue
		}
		if !fb.LastSyncAttemptAt.IsZero() && fb.LastSyncAttemptAt.After(cutoff) {
			continue
		}
		fb.State = blockstore.BlockStatePending
		fb.LastSyncAttemptAt = time.Time{}
		if err := m.fileBlockStore.Put(ctx, fb); err != nil {
			// Phase 11 IN-02: elevate per-row failure to ERROR and track
			// counts so a fully-broken metadata path produces a non-nil
			// return error visible to the caller (Start logs it at WARN).
			logger.Error("janitor: requeue failed", "blockID", fb.ID, "error", err)
			failed++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		requeued++
	}
	if requeued > 0 {
		logger.Info("Syncer janitor requeued stale Syncing rows",
			"count", requeued, "claim_timeout", m.config.ClaimTimeout)
	}
	if failed > 0 {
		return fmt.Errorf("janitor: %d of %d candidate rows failed to requeue (first error: %w)",
			failed, failed+requeued, firstErr)
	}
	return nil
}

// syncingEnumerator is an optional capability a FileBlockStore may
// implement so the syncer's restart-recovery janitor can find stale
// Syncing rows without a full table scan.
type syncingEnumerator interface {
	EnumerateSyncingBlocks(ctx context.Context) ([]*blockstore.FileBlock, error)
}

// periodicUploader runs every interval, scanning for blocks to upload.
// Uses an atomic guard to prevent overlapping ticks: if the previous upload
// batch is still running when the ticker fires, the tick is skipped. This
// prevents unbounded memory growth when uploads take longer than the interval
// (e.g., 8 blocks x 2-3s S3 upload = 16-24s, but interval is only 2s).
func (m *Syncer) periodicUploader(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	logger.Info("Periodic syncer started", "interval", interval, "upload_delay", m.config.UploadDelay)

	for {
		select {
		case <-ticker.C:
			if !m.canProcess(ctx) {
				logger.Info("Periodic syncer: canProcess=false, exiting")
				return
			}
			// Skip this tick if the previous upload batch is still running.
			// This prevents overlapping ticks from multiplying memory usage.
			if !m.uploading.CompareAndSwap(false, true) {
				logger.Debug("Periodic syncer: previous tick still running, skipping")
				continue
			}
			func() {
				defer m.uploading.Store(false)
				// Circuit breaker: skip uploads when remote store is unhealthy
				if !m.IsRemoteHealthy() {
					logger.Warn("Periodic syncer: remote unhealthy, skipping upload cycle",
						"outage_duration", m.RemoteOutageDuration(),
						"hint", "check S3 credentials, endpoint, and bucket configuration")
					return
				}
				m.syncLocalBlocks(ctx)
			}()
		case <-m.stopCh:
			logger.Info("Periodic syncer: stopCh received, exiting")
			return
		case <-ctx.Done():
			logger.Info("Periodic syncer: context cancelled, exiting")
			return
		}
	}
}

// Close shuts down the syncer and waits for pending uploads.
func (m *Syncer) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.mu.Unlock()

	close(m.stopCh)

	if m.healthMonitor != nil {
		m.healthMonitor.Stop()
	}

	// Wait for in-flight uploads and flushes to complete before closing.
	// This prevents "store is closed" races when the remote store is closed
	// immediately after the syncer.
	ctx, cancel := context.WithTimeout(context.Background(), defaultShutdownTimeout)
	defer cancel()
	_ = m.DrainAllUploads(ctx)

	// Stop transfer queue with graceful shutdown timeout
	if m.queue != nil {
		m.queue.Stop(defaultShutdownTimeout)
	}

	return nil
}

// HealthCheck verifies the remote store is accessible.
// Returns nil (healthy) when remoteStore is nil -- local-only mode is valid.
func (m *Syncer) HealthCheck(ctx context.Context) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.closed {
		return ErrClosed
	}

	if m.remoteStore == nil {
		return nil // Local-only mode is healthy
	}

	return m.remoteStore.HealthCheck(ctx)
}

// SetRemoteStore transitions the syncer from local-only mode to remote-backed mode.
// This is a one-shot operation -- calling it again returns an error.
// It sets the remoteStore, enables local store eviction, and starts the periodic syncer.
func (m *Syncer) SetRemoteStore(ctx context.Context, remoteStore remote.RemoteStore) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return ErrClosed
	}
	if m.remoteStore != nil {
		return errors.New("remote store already set")
	}
	if remoteStore == nil {
		return errors.New("remoteStore must not be nil")
	}

	m.remoteStore = remoteStore
	m.local.SetEvictionEnabled(true)

	m.startHealthMonitor(ctx)
	m.startPeriodicUploader(ctx)

	logger.Info("Remote store attached, periodic syncer started")
	return nil
}

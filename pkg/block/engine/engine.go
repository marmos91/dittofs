package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/local"
	"github.com/marmos91/dittofs/pkg/block/remote"
	"github.com/marmos91/dittofs/pkg/metadata"
	mderrors "github.com/marmos91/dittofs/pkg/metadata/errors"
)

// Compile-time interface satisfaction check.
var _ block.ComposedStore = (*Store)(nil)

// BlockStoreConfig holds the components that make up a Store.
type BlockStoreConfig struct {
	// Local is the on-node block store (required).
	Local local.LocalStore

	// Remote is the durable backend store (nil for local-only mode).
	Remote remote.RemoteStore

	// Syncer handles async local-to-remote transfers (required).
	Syncer *Syncer

	// FileBlockStore provides block metadata for block store statistics
	// AND the engine-internal lookups (GetFileBlock, ListFileBlocks) the
	// dual-read resolver and populateBlockCounts still consume —
	// block.EngineFileBlockStore. When set, GetStats() populates
	// BlocksLocal/BlocksRemote/BlocksTotal.
	FileBlockStore block.EngineFileBlockStore

	// Coordinator handles all metadata-store operations the engine
	// needs (RefCount mutations, BlockRef-list persistence).
	// keeps pkg/metadata out of the engine hot path. May be nil in
	// tests; production wiring (pkg/controlplane/runtime/shares/
	// service.go) MUST inject a real impl. See coordinator.go for the
	// contract.
	Coordinator MetadataCoordinator

	// SyncedHashStore persists per-CAS-hash local→remote mirror state.
	// Sourced from the same per-share metadata-store handle the
	// Coordinator wraps. Threaded through to the Syncer so the mirror
	// loop in Flush can call MarkSynced after each successful
	// remote.Put. Nil is accepted (local-only / no-remote fixtures)
	// the Syncer's mirror loop early-exits in that mode.
	SyncedHashStore metadata.SyncedHashStore

	// ReadBufferBytes is the memory budget for the read buffer per share.
	// 0 disables the read buffer. Passed directly to NewReadBuffer as byte budget.
	ReadBufferBytes int64

	// PrefetchWorkers is the number of goroutines for sequential prefetch.
	// 0 disables prefetching.
	PrefetchWorkers int
}

// Store is the central orchestrator for block storage. It composes a local
// store, optional remote store, and syncer into the block.Store
// interface. All protocol adapters and runtime code use Store for I/O.
//
// Read operations check the read buffer first, then the local store, falling
// back to remote download via the syncer on miss. Write operations go
// directly to the local store and invalidate the read buffer; the syncer
// handles background upload to remote.
type Store struct {
	local  local.LocalStore
	remote remote.RemoteStore
	syncer *Syncer

	// widened to EngineFileBlockStore so populateBlockCounts
	// can call ListFileBlocks (engine-internal method not on the public
	// FileBlockStore surface).
	fileBlockStore block.EngineFileBlockStore // optional: for block count stats

	// coordinator handles all metadata-store operations the engine
	// needs (RefCount mutations, BlockRef-list persistence). May be nil
	// in tests; production wiring (pkg/controlplane/runtime/shares/
	// service.go) MUST inject a real impl. See coordinator.go for the
	// contract.
	coordinator MetadataCoordinator

	// syncedHashStore persists per-CAS-hash local→remote mirror state.
	// Held alongside the coordinator so the engine constructor can
	// thread it into both the Syncer (via SetSyncedHashStore) and the
	// FSStore (via the ObjectIDPersister callback target's sibling
	// SetSyncedHashStore on the local store). May be nil in tests.
	syncedHashStore metadata.SyncedHashStore

	// cache is the CAS-keyed cache (CACHE-01..05). The block-coord
	// ReadBuffer + standalone Prefetcher are folded into this single
	// Cache type. Never nil — the constructor substitutes nullCache{}
	// for a disabled budget so engine code does not need defensive
	// nil-checks (Null Object pattern).
	//
	// The field is mutated after construction — Start swaps in the real
	// Cache and DestroyCache swaps in nullCache{} — while concurrent data
	// ops and the OnChunkComplete rollup goroutine read it. Those mutations
	// race the closeMu.RLock'd readers (RLock does not serialize against
	// other RLock holders), so every access goes through cacheMu, not the
	// raw field. cacheMu is a leaf lock taken only for the field swap, never
	// held across cache operations.
	cache   cacheInterface
	cacheMu sync.RWMutex

	readBufferBytes int64 // budget for the cache (0 = disabled / Null Object)
	prefetchWorkers int   // stored from config, used in Start()

	// closeMu is the lifecycle gate. Every public data op (WriteAt,
	// ReadAt, Flush, Truncate, Delete, …) takes closeMu.RLock() at entry
	// and holds it for the op's full duration. Close takes closeMu.Lock(),
	// which blocks until all in-flight ops drop their RLocks, then performs
	// teardown. This makes Store.Close safe to call concurrently with
	// in-flight ops (area-7 H-A use-after-close): an op either runs fully
	// against a live store, or — if it arrives after closed is set — fails
	// fast with ErrStoreClosed. The RLock side is shared, so concurrent
	// data ops still run in parallel; only Close serializes.
	//
	// Re-entrancy: public ops never call another RLock-gated public op on
	// the same Store (internal helpers in read_internal.go / the syncer are
	// ungated), so the non-reentrant Go RWMutex read side is safe here.
	closeMu  sync.RWMutex
	closed   bool  // guarded by closeMu; true once teardown has run
	closeErr error // memoized result of the first Close (idempotent)
}

// New creates a new Store from the given configuration.
// Local store and syncer are required; remote may be nil for local-only mode.
func New(cfg BlockStoreConfig) (*Store, error) {
	if cfg.Local == nil {
		return nil, errors.New("local store is required")
	}
	if cfg.Syncer == nil {
		return nil, errors.New("syncer is required")
	}

	bs := &Store{
		local:           cfg.Local,
		remote:          cfg.Remote,
		syncer:          cfg.Syncer,
		fileBlockStore:  cfg.FileBlockStore,
		coordinator:     cfg.Coordinator,
		syncedHashStore: cfg.SyncedHashStore,
		readBufferBytes: cfg.ReadBufferBytes,
		prefetchWorkers: cfg.PrefetchWorkers,
	}
	// Cache is created later in Start (loadFn closure captures bs and
	// NewCache spawns workers immediately). For now mount the Null Object
	// so engine code can call bs.cache.* without nil-checks even before
	// Start runs.
	bs.cache = nullCache{}
	// Thread the coordinator into the syncer so the file-level dedup
	// short-circuit (engine.Flush's pre-rollup hook) can call into
	// trySpeculativeFileLevelDedup with a real coordinator. cfg.Syncer
	// is guaranteed non-nil by the required-field check above.
	if cfg.Coordinator != nil {
		cfg.Syncer.SetCoordinator(cfg.Coordinator)
	}
	// Thread the SyncedHashStore into the Syncer so the mirror loop in
	// Flush can call MarkSynced after each remote.Put. Nil is accepted
	// (local-only / no-remote fixtures); the mirror loop early-exits in
	// that mode.
	if cfg.SyncedHashStore != nil {
		cfg.Syncer.SetSyncedHashStore(cfg.SyncedHashStore)
	}
	// Wire the per-store chunk-lifecycle callbacks via the named
	// [local.ChunkLifecycleHooks] capability surface. Three setters
	// install closures that downstream FileBlock metadata and the read
	// cache depend on; each implementation may treat any setter as a
	// no-op when its data path doesn't reach that hook (FSStore's
	// rollup-completion path covers ObjectIDPersister + OnChunkComplete;
	// the in-memory backend covers ChunkEmitter). Foreign local stores
	// that don't satisfy the interface silently skip all three installs.
	hooks, hasHooks := cfg.Local.(local.ChunkLifecycleHooks)
	if hasHooks {
		// (1) Install the rollup-completion ObjectIDPersister callback.
		// The callback writes per-block FileBlock rows so the engine's
		// CAS read path (readLocalByHash → resolveFileBlock) can
		// resolve (payloadID, blockIdx) → hash and delegates to the
		// coordinator's PersistFileBlocks so FileAttr.Blocks and
		// FileAttr.ObjectID land in a single metadata txn at rollup
		// time. Stores that don't drive a rollup-completion path
		// (in-memory / fixtures use the parallel ChunkEmitter hook
		// below) install a no-op setter — ObjectID compute still runs
		// inside rollup but the persist step is no-op.
		fbs := cfg.FileBlockStore
		hooks.SetObjectIDPersister(func(ctx context.Context, payloadID string, blocks []block.BlockRef, objectID block.ObjectID) error {
			// (1) Per-chunk FileBlock rows. The row ID encodes the
			//     chunk's absolute byte Offset directly (rather than
			//     a synthetic blockIdx = Offset / BlockSize); the
			//     engine's CAS read path uses the parsed Offset to
			//     locate which chunk covers a given byte range under
			//     FastCDC's variable chunk geometry. The trailing
			//     numeric component is the chunk Offset in bytes.
			// #953: snapshot the per-file FileBlock row offsets that exist
			// BEFORE this pass writes its new rows. After the new rows are
			// durable we reap any prior row that this pass's re-chunk
			// SUPERSEDED — a row whose offset falls inside the byte region
			// this pass rewrote but is NOT one of the new chunk offsets.
			// Without this the per-file CAS manifest accumulates stale,
			// overlapping rows from multiple write generations; the cold
			// read path (fillFromCASManifest / readLocalByHash) then mixes
			// generations and a stale row clobbers freshly-written bytes
			// (silent corruption after log compaction / local-state
			// eviction). Snapshot now, reap after PersistFileBlocks below.
			var priorOffsets []uint64
			if bs.fileBlockStore != nil {
				priorRows, lerr := bs.fileBlockStore.ListFileBlocks(ctx, payloadID)
				if lerr != nil {
					return fmt.Errorf("ObjectIDPersister: list prior blocks for %s: %w", payloadID, lerr)
				}
				priorOffsets = make([]uint64, 0, len(priorRows))
				for _, pr := range priorRows {
					if pr == nil {
						continue
					}
					if off, ok := block.ParseChunkOffset(pr.ID); ok {
						priorOffsets = append(priorOffsets, off)
					}
				}
			}

			if fbs != nil {
				for _, b := range blocks {
					if b.Hash.IsZero() {
						continue
					}
					fb := &block.FileBlock{
						ID:       fmt.Sprintf("%s/%d", payloadID, b.Offset),
						Hash:     b.Hash,
						DataSize: b.Size,
						State:    block.BlockStatePending,
					}
					// Crash-consistency (#583): surface the put error so
					// a failed FileBlock row write fails the rollup pass
					// rather than silently losing the manifest entry.
					// Without this surfacing the engine continues into
					// PersistFileBlocks below, the rollup_offset advances
					// past records whose manifest row never landed, and
					// subsequent reads hit the sparse-block zero-fill
					// branch — silent data loss.
					if err := fbs.Put(ctx, fb); err != nil {
						return fmt.Errorf("ObjectIDPersister: FileBlock.Put(%s): %w", fb.ID, err)
					}
				}
			}
			// (2) Manifest + ObjectID coordinator txn.
			if bs.coordinator == nil {
				return nil
			}
			// The persister fires once per rollup PASS, carrying only the
			// chunks rolled in THAT pass. FileAttr.Blocks (the snapshot
			// manifest source, read by metadata Backup) must reflect the
			// COMPLETE file, so a multi-pass rollup (append, or a large
			// write split across stabilization windows) cannot persist the
			// partial pass list here — that REPLACES FileAttr.Blocks with
			// only the last pass's chunks, silently dropping every prior
			// pass from the manifest (#789, backend-agnostic — Postgres
			// just hits multi-pass more often via slower txns).
			//
			// Merge this pass's blocks into the already-persisted list
			// (read back via the coordinator — file_block_refs / encoded
			// FileAttr.Blocks, both of which store the content hash, unlike
			// the per-file FileBlock index whose Pending rows carry a NULL
			// hash on Postgres). The merge is offset-keyed: this pass's
			// blocks overlay any overlapping byte ranges (in-place rewrite)
			// and extend the rest (append). Recompute the Merkle-root
			// ObjectID over the COMPLETE list so it matches a single-pass
			// write of identical content and file-level dedup still resolves.
			persistBlocks, persistObjID := blocks, objectID
			existing, gerr := bs.coordinator.GetPersistedBlocks(ctx, payloadID)
			if gerr != nil {
				return fmt.Errorf("ObjectIDPersister: read persisted blocks for %s: %w", payloadID, gerr)
			}
			if len(existing) > 0 {
				persistBlocks = block.MergeBlockRefsByOffset(existing, blocks)
				persistObjID = block.ComputeObjectID(persistBlocks)
			}
			if err := bs.persistRollupBlocksConverging(ctx, payloadID, persistBlocks, persistObjID); err != nil {
				return err
			}
			return bs.reapSupersededFileBlocks(ctx, payloadID, priorOffsets, blocks)
		})

		// (2) Install the chunk-completion callback (production
		// *fs.FSStore wires this on the chunkstore hot path; the
		// in-memory backend no-ops since its writes don't materialize
		// through the CAS chunkstore.StoreChunk + lruTouch path). The
		// closure delegates every successful chunkstore write to
		// bs.cache.Put: the engine Cache becomes warm on the write
		// side, so the NFS COMMIT-then-READ pattern never goes back to
		// disk for the just-written chunk. The closure captures bs
		// (not bs.cache) so the Null-Object→real-Cache swap performed
		// by Store.Start is observed transparently. The path arg
		// is intentionally discarded (`_ string`) — Cache.Put doesn't
		// consume it; the firing-site contract still passes it to
		// enable future mmap-or-copy strategies. Cache.Put is
		// nil-safe, closed-safe, and max-bytes-safe, so this binding
		// is the canonical safe wiring (RAM ceiling bounded by Cache's
		// existing LRU). Same lifecycle precedent as the persister
		// above — install once at construction; FSStore guarantees no
		// chunk activity fires before Start completes.
		hooks.SetOnChunkComplete(func(hash block.ContentHash, data []byte, _ string) {
			bs.loadCache().Put(hash, data)
			// Register the freshly-stored chunk for upload without a
			// directory walk (B1). The syncer drains this set on each
			// mirror pass; harmless when no remote is configured. The byte
			// size feeds the syncer's unsynced-bytes backpressure counter.
			bs.syncer.addPendingHash(hash, int64(len(data)))
		})

		// (3) Install the per-chunk emitter (the in-memory backend
		// uses this; *fs.FSStore no-ops since it drives the equivalent
		// rollup-side path through the ObjectIDPersister callback
		// above). The emitter mirrors each freshly-emitted CAS chunk
		// into a FileBlock row keyed by {payloadID}/{chunkStart} so the
		// engine's CAS read path (readLocalByHash) can resolve
		// (payloadID, offset) → hash without a separate manifest.
		// Requires a FileBlockStore — fixtures running without one
		// rely on the no-op default.
		if cfg.FileBlockStore != nil {
			fbs := cfg.FileBlockStore
			hooks.SetChunkEmitter(func(payloadID string, chunkStart uint64, size uint32, hash block.ContentHash) {
				fb := &block.FileBlock{
					ID:       fmt.Sprintf("%s/%d", payloadID, chunkStart),
					Hash:     hash,
					DataSize: size,
					State:    block.BlockStatePending,
				}
				// Crash-consistency (#583): emitter signature is void
				// by contract; a put failure here means the manifest
				// row never landed and reads will sparse-zero that
				// range. Log at Error so operators see the loss
				// instead of silently swallowing it. Follow-up:
				// promote emitter to return an error so the caller
				// can fail the rollup pass.
				if err := fbs.Put(context.Background(), fb); err != nil {
					logger.Error("ChunkEmitter: FileBlock.Put failed", "id", fb.ID, "error", err)
				}
				// Register for upload (B1). The in-memory backend creates
				// chunks via this emitter rather than onChunkComplete, so
				// without this the mirror loop's pending set would never
				// see memory-backend chunks. size is the chunk's byte
				// length, feeding the unsynced-bytes backpressure counter.
				bs.syncer.addPendingHash(hash, int64(size))
			})
		}
	}
	// wire the Store back-reference onto the Syncer so
	// the file-level dedup short-circuit can reach Store.cache for
	// surgical invalidation of orphaned speculative chunks. Reading
	// through the back-reference (instead of caching a cacheInterface
	// field on the Syncer at construction time) lets test code swap
	// `bs.cache = rec` post-construction and still observe the
	// invalidation — mirrors the TestClose_ClosesCache pattern.
	cfg.Syncer.bs = bs
	return bs, nil
}

// rollupPersistMaxAttempts bounds the converging retry loop in
// persistRollupBlocksConverging. Each attempt re-resolves the canonical
// object_id owner and, on a recognized conflict, converges toward the
// zero-objectID write. The cap guarantees termination so a genuinely
// pathological metadata-store fault surfaces a clear error instead of looping
// forever (#1245-B).
const rollupPersistMaxAttempts = 8

// isRollupPersistConflict reports whether err is a conflict the rollup
// persister can converge past by falling back to the zero-objectID write. It
// recognizes BOTH:
//
//   - isObjectIDConflict(err) — the deterministic first-committer-wins
//     object_id conflict the coordinator maps to engine.ErrObjectIDConflict
//     (Postgres 23505 on files_object_id_idx, the in-closure PutFile conflict
//     on Memory/Badger); and
//
//   - mderrors.IsConflictError(err) — a raw store-level ErrConflict that
//     bypasses the coordinator's in-closure mapObjectIDConflict because it is
//     raised at COMMIT time, not by the PutFile call. Badger's SSI
//     optimistic-concurrency abort under bulk same-content rollup is the
//     canonical case (#1245-B): WithTransaction now returns a wrapped
//     mderrors.StoreError{Code: ErrConflict} when its retry budget is
//     exhausted, which surfaces here through the WithTransaction return value
//     rather than through PutFile.
//
// Recognizing both keeps the converging fallback uniform across the
// deterministic object_id conflict and the SSI hot-key conflict without
// altering isObjectIDConflict (whose dedup-retry callers must keep their
// narrower semantics).
func isRollupPersistConflict(err error) bool {
	return isObjectIDConflict(err) || mderrors.IsConflictError(err)
}

// persistRollupBlocksConverging persists a rollup pass's block list + ObjectID
// for payloadID, guaranteeing convergence under the bulk same-content
// (file-level dedup) race (#1245-B).
//
// Two cooperating mechanisms remove the livelock the naive single-fallback had:
//
//  1. Read-only pre-check. BEFORE attempting to claim the object_id, ask the
//     coordinator whether a canonical owner already exists for this Merkle root
//     (FindByObjectID). If so, this file is a duplicate-but-not-the-first; it
//     persists its blocks with a ZERO object_id immediately and never enters
//     the obj-key write/probe race at all. This eliminates the write-write
//     contention for every identical file after the first, which is what
//     turned a transient SSI abort into a sustained hot-key livelock.
//
//  2. Bounded converging backoff. The genuine first-committer race (two files
//     reach PersistFileBlocks before either has committed, so FindByObjectID
//     missed for both) is still possible. On a recognized conflict
//     (isRollupPersistConflict — uniform across the deterministic object_id
//     conflict and Badger's wrapped commit-time SSI abort) the loop backs off,
//     re-resolves FindByObjectID (a peer may have just become canonical), and
//     retries — converging to the zero-objectID write. The attempt count is
//     capped so a real fault terminates with a clear error rather than spinning
//     the rollup ticker on the same payloadID forever.
//
// A duplicate's blocks are content-addressed and already durable in CAS; the
// only thing it forfeits is ownership of the canonical object_id pointer (left
// zero = "not the dedup target"), which the partial unique index skips so the
// per-file block list lands cleanly and the file stays restorable.
func (bs *Store) persistRollupBlocksConverging(ctx context.Context, payloadID string, persistBlocks []block.BlockRef, persistObjID block.ObjectID) error {
	var zeroObjectID block.ObjectID

	// wantObjID is the object_id this file will TRY to claim. The pre-check and
	// the conflict handler both narrow it to zero once a canonical owner is
	// known to exist, after which the file persists its blocks as a benign
	// duplicate.
	wantObjID := persistObjID

	// (1) Read-only pre-check: if a canonical owner already exists for this
	// Merkle root, target the zero object_id from the start so this file never
	// races on the obj-key write. Skip on a zero objectID (nothing to dedup
	// against) and treat a lookup error as non-fatal — fall through to the
	// write loop, which re-surfaces any real fault. The actual write still goes
	// through the bounded backoff below so a concurrent SSI abort on the
	// (still-hot) file row / content-hash rows converges instead of failing.
	if !persistObjID.IsZero() && bs.canonicalOwnerExists(ctx, persistObjID) {
		logger.Debug("rollup persist: canonical object_id owner already exists; persisting duplicate's blocks with zero object_id (pre-check)",
			"payloadID", payloadID, "objectID", persistObjID.String())
		wantObjID = zeroObjectID
	}

	// (2) Bounded converging backoff around the claim. The first successful
	// committer wins the object_id; every subsequent committer recognizes the
	// conflict, re-resolves, and converges to the zero-objectID write. A
	// zero-objectID write that itself aborts (SSI on a still-hot file row /
	// content-hash row under bulk identical content) is retried until it
	// commits.
	backoff := time.Millisecond
	var lastErr error
	for attempt := 0; attempt < rollupPersistMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := bs.coordinator.PersistFileBlocks(ctx, payloadID, persistBlocks, wantObjID)
		if err == nil {
			return nil
		}
		if !isRollupPersistConflict(err) {
			return err
		}
		lastErr = err

		// Conflict recognized. If we were still trying to claim the canonical
		// object_id, re-resolve: a peer may have just committed and become the
		// canonical owner, in which case we converge to the zero-objectID write
		// on the next attempt. If we already targeted zero, the conflict is on
		// a still-hot non-object_id key (SSI abort on the file row / content-
		// hash rows under bulk identical content) — back off and retry the
		// same zero-objectID write until it commits.
		if !wantObjID.IsZero() && bs.canonicalOwnerExists(ctx, wantObjID) {
			logger.Debug("rollup persist: object_id now owned by another file; converging to zero-objectID write",
				"payloadID", payloadID, "objectID", wantObjID.String(), "attempt", attempt)
			wantObjID = zeroObjectID
		}

		time.Sleep(backoff)
		if backoff < 50*time.Millisecond {
			backoff *= 2
		}
	}
	return fmt.Errorf("rollup persist: did not converge after %d attempts (payloadID=%s): %w",
		rollupPersistMaxAttempts, payloadID, lastErr)
}

// canonicalOwnerExists reports whether a file already owns objectID as its
// canonical Merkle-root pointer. A lookup error is treated as "unknown owner"
// (non-fatal): the caller falls through to the bounded write loop, which
// re-surfaces any real fault. Callers must guard against a zero objectID.
func (bs *Store) canonicalOwnerExists(ctx context.Context, objectID block.ObjectID) bool {
	owner, err := bs.coordinator.FindByObjectID(ctx, objectID)
	return err == nil && owner != nil
}

// Start initializes the store and starts background goroutines.
// Recovery runs on the local store first (if supported), then the syncer
// and local store background goroutines are started. Finally, the prefetcher
// is created if both the read buffer and prefetch workers are configured.
func (bs *Store) Start(ctx context.Context) error {
	// Run recovery on local store if it supports it (FSStore has Recover).
	type recoverer interface {
		Recover(ctx context.Context) error
	}
	if r, ok := bs.local.(recoverer); ok {
		if err := r.Recover(ctx); err != nil {
			logger.Warn("Store: local store recovery encountered errors", "error", err)
		}
	}

	// Start local store background goroutines (e.g., periodic FileBlock metadata persistence).
	// Use background context so these outlive the calling request context.
	bs.local.Start(context.Background())

	// Wire the health callback BEFORE starting the syncer. The health monitor
	// captures the callback at Start time (startHealthMonitor reads
	// m.onHealthChanged once); registering it afterwards means an initial
	// "unhealthy" transition fired by the syncer's eager startup probe is
	// delivered to a nil callback and lost — leaving eviction enabled while the
	// remote is down, which can evict local CAS chunks before they are mirrored
	// (silent data loss on read).
	//
	// When remote goes unhealthy, suspend eviction to prevent evicting blocks
	// that cannot be re-downloaded. When healthy again, re-enable eviction.
	bs.syncer.SetHealthCallback(func(healthy bool) {
		bs.local.SetEvictionEnabled(healthy)
		if healthy {
			logger.Info("Remote store healthy: eviction re-enabled")
		} else {
			logger.Warn("Remote store unhealthy: eviction suspended")
		}
	})

	// Start syncer background goroutines (periodic uploader, transfer queue).
	bs.syncer.Start(context.Background())

	// Reconcile eviction with the post-start health state, covering the case
	// where the initial probe settled health without driving a transition
	// callback (e.g. it started unhealthy with no prior state to transition
	// from).
	bs.local.SetEvictionEnabled(bs.syncer.IsRemoteHealthy())

	// Wire the Cache in Start so the loadByHash closure captures bs and
	// NewCache spawns workers immediately. A single Cache type replaces
	// the legacy ReadBuffer + Prefetcher pair. readBufferBytes is read
	// out of cfg via a stash because cfg lives only inside New; we
	// recover it from bs's own state. Engine constructor stashes the
	// budget on the Store so Start can read it; if the budget is
	// 0 we keep the Null Object.
	if bs.readBufferBytes > 0 {
		realCache := NewCache(bs.readBufferBytes, bs.prefetchWorkers, bs.loadByHash)
		if realCache != nil {
			bs.storeCache(realCache)
		}
	}

	return nil
}

// loadByHash is the loadByHashFn the Cache's prefetch workers call to
// pull a block by ContentHash. It performs a single content-addressed
// local read; local.Get is the only primitive (no mmap fast-path, no
// legacy FileBlock → GetBlockData fallback).
//
// Buffer ownership: local.Get returns a freshly allocated []byte; the
// Cache copies those bytes into its LRU slot on miss. The net
// allocation count matches the legacy mmap-then-copy semantics — the
// alloc just moves earlier in the pipeline.
//
// Remote fallback is intentionally NOT wired here — prefetch is
// best-effort and shouldn't block on a remote round-trip; if the
// block isn't local, the next on-path read will pull it via the
// syncer.
func (bs *Store) loadByHash(ctx context.Context, hash block.ContentHash) ([]byte, error) {
	return bs.local.Get(ctx, hash)
}

// enter is the lifecycle-gate entry every public data op calls before
// touching local/remote/syncer state. It pins the store open for the op's
// duration by taking closeMu.RLock(); if the store is already closed it
// releases the lock and returns ErrStoreClosed. On success the caller MUST
// defer bs.closeMu.RUnlock() so Close (which takes the write lock) can drain.
//
// Callers use it as:
//
//	if err := bs.enter(); err != nil {
//	    return …, err
//	}
//	defer bs.closeMu.RUnlock()
//
// The RLock is held for the WHOLE op (not released early) — that is what
// keeps the underlying local/remote/syncer alive while the op runs and what
// Close blocks on. RLock is shared, so concurrent ops still run in parallel.
func (bs *Store) enter() error {
	bs.closeMu.RLock()
	if bs.closed {
		bs.closeMu.RUnlock()
		return ErrStoreClosed
	}
	return nil
}

// Close releases resources held by the store. It first drains all in-flight
// data ops (closeMu.Lock blocks until every enter()'s RLock is released),
// marks the store closed so new ops fail fast with ErrStoreClosed, then
// tears down cache → syncer → local → remote. Close is idempotent: a second
// call is a no-op that returns the first call's result.
//
// area-7 H-A: by draining under closeMu, an admin RemoveShare → Close can no
// longer run concurrently with a client WriteAt/ReadAt on the same *Store —
// the op either completes fully or never starts (ErrStoreClosed). The drain
// makes it safe for RemoveShare to keep calling Close() outside the shares
// service lock.
func (bs *Store) Close() error {
	// Acquire the write lock: this blocks until all in-flight data ops
	// (which hold closeMu.RLock via enter) have finished, giving us the
	// in-flight drain. New ops arriving after closed=true fail in enter().
	bs.closeMu.Lock()
	defer bs.closeMu.Unlock()

	if bs.closed {
		// Idempotent: return the memoized result of the first teardown.
		return bs.closeErr
	}
	bs.closed = true

	// Cache is never nil thanks to the Null Object pattern. Swap in the
	// Null Object under cacheMu so any concurrent OnChunkComplete read sees
	// a live cache, then close the real one we just detached.
	_ = bs.storeCache(nullCache{}).Close()

	var errs []error
	if err := bs.syncer.Close(); err != nil {
		errs = append(errs, fmt.Errorf("syncer close: %w", err))
	}
	if err := bs.local.Close(); err != nil {
		errs = append(errs, fmt.Errorf("local close: %w", err))
	}
	if bs.remote != nil {
		if err := bs.remote.Close(); err != nil {
			errs = append(errs, fmt.Errorf("remote close: %w", err))
		}
	}

	bs.closeErr = errors.Join(errs...)
	return bs.closeErr
}

// SetMetrics forwards the inline metrics recorder to the underlying local
// store when it participates in the [local.MetricsAware] capability surface
// (the *fs.FSStore eviction/backpressure path). Stores that emit no inline
// metrics (e.g. the in-memory store) simply don't implement MetricsAware, so
// this is a no-op for them. The runtime calls this after it learns its metrics
// handle — shares are constructed before the registry exists.
func (bs *Store) SetMetrics(rec local.MetricsRecorder) {
	if aware, ok := bs.local.(local.MetricsAware); ok {
		aware.SetMetrics(rec)
	}
}

// --- Test helpers ---

// LocalForTest returns the engine's underlying local store as the
// local.LocalStore interface. Used by cross-package test fixtures
// (e.g. internal/adapter/common) that need to drive rollup or other
// admin paths against the concrete *fs.FSStore via a type assertion.
// Do not use in production code.
func (bs *Store) LocalForTest() local.LocalStore { return bs.local }

// RemoteStore returns the per-share remote object store, or nil if the
// share is local-only. Used by the snapshot sync-gate verify step
// to drive VerifyRemoteDurability after DrainAllUploads, and by
// cross-package tests for shared-remote identity checks.
func (bs *Store) RemoteStore() remote.RemoteStore { return bs.remote }

// ListFiles returns the payloadIDs of all files tracked in the local store.
func (bs *Store) ListFiles() []string { return bs.local.ListFiles() }

// EvictLocal removes all local per-file state (memory tracking, files
// map, accessTracker, append log) for a file. CAS chunks are NOT
// removed here — they may be shared with other files via file-level
// dedup and are reclaimed via the refcount → GC path (engine.Delete
// decrements per dropped hash and the mark-sweep GC reaps orphans).
func (bs *Store) EvictLocal(ctx context.Context, payloadID string) error {
	if err := bs.enter(); err != nil {
		return err
	}
	defer bs.closeMu.RUnlock()
	if err := bs.local.EvictMemory(ctx, payloadID); err != nil {
		return err
	}
	return bs.local.DeleteAppendLog(ctx, payloadID)
}

// loadCache returns the current cache under cacheMu. Always non-nil (Null
// Object pattern). Callers invoke the returned interface's methods OUTSIDE
// the lock — cacheMu only guards the field read.
func (bs *Store) loadCache() cacheInterface {
	bs.cacheMu.RLock()
	c := bs.cache
	bs.cacheMu.RUnlock()
	return c
}

// storeCache swaps the cache field under cacheMu and returns the previous
// value so the caller can close it after releasing the lock.
func (bs *Store) storeCache(c cacheInterface) cacheInterface {
	bs.cacheMu.Lock()
	prev := bs.cache
	bs.cache = c
	bs.cacheMu.Unlock()
	return prev
}

// DestroyCache closes the in-memory read cache and replaces it with a
// no-op implementation. Intended for shutdown / share-removal teardown
// and for the REST evict path that drops the read buffer wholesale.
// Returns the number of entries that were present before destruction.
func (bs *Store) DestroyCache() int {
	// Pin against Close teardown. No error return to surface ErrStoreClosed,
	// so a closed store reports 0 destroyed entries rather than racing the
	// cache teardown Close performs under closeMu.Lock — Close already closed
	// the cache, so there is nothing to destroy.
	bs.closeMu.RLock()
	defer bs.closeMu.RUnlock()
	if bs.closed {
		return 0
	}

	// Swap in the Null Object first so concurrent readers never observe a
	// closed cache, then close the previous one after the field is updated.
	prev := bs.storeCache(nullCache{})
	entries := prev.Stats().Entries
	_ = prev.Close()
	return entries
}

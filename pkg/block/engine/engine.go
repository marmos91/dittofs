package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/local"
	"github.com/marmos91/dittofs/pkg/block/remote"
	"github.com/marmos91/dittofs/pkg/metadata"
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

	// FileChunkStore provides block metadata for block store statistics
	// AND the engine-internal lookups (GetFileChunk, ListFileChunks) the
	// dual-read resolver and populateBlockCounts still consume —
	// block.EngineFileChunkStore. When set, GetStats() populates
	// BlocksLocal/BlocksRemote/BlocksTotal.
	FileChunkStore block.EngineFileChunkStore

	// Coordinator handles all metadata-store operations the engine
	// needs (RefCount mutations, ChunkRef-list persistence).
	// keeps pkg/metadata out of the engine hot path. May be nil in
	// tests; production wiring (pkg/controlplane/runtime/shares/
	// service.go) MUST inject a real impl. See coordinator.go for the
	// contract.
	Coordinator MetadataCoordinator

	// SyncedHashStore persists per-CAS-hash local→remote sync state.
	// Sourced from the same per-share metadata-store handle the
	// Coordinator wraps. Threaded through to the Syncer so the carver
	// can commit synced markers + block locators atomically
	// (DefaultCommitBlock). Nil is accepted (local-only / no-remote
	// fixtures); carve stays disabled in that mode.
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

	// metrics is the engine-side data-plane metrics sink (carve/upload
	// path). Retained from SetMetrics when the injected recorder also
	// satisfies DataplaneMetrics. Held as an atomic.Pointer because
	// SetMetrics back-fills it on already-serving shares (the registry is
	// built after shares load), racing the concurrent mirror goroutines that
	// read it. Nil pointer (the zero value) until set; call sites guard.
	metrics atomic.Pointer[DataplaneMetrics]

	// widened to EngineFileChunkStore so populateBlockCounts
	// can call ListFileChunks (engine-internal method not on the public
	// FileChunkStore surface).
	fileChunkStore block.EngineFileChunkStore // optional: for block count stats

	// coordinator handles all metadata-store operations the engine
	// needs (RefCount mutations, ChunkRef-list persistence). May be nil
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

	// migrateCancel/migrateDone govern the background cas→blocks migration
	// goroutine spawned by Start. Close cancels the context and waits on the
	// channel so the goroutine's in-flight remote I/O never races the store
	// teardown below it. Both nil/zero until Start runs.
	migrateCancel context.CancelFunc
	migrateDone   chan struct{}

	// requireDurableCommit gates the strict honest-CLOSE/COMMIT rule (#1274).
	// When false (the default), CommitBlockStore acks once engine.Flush
	// succeeds regardless of local/remote durability — the remote mirror
	// stays fully async and observable via the unsynced-bytes metric, so the
	// fast path is preserved and ordinary NFS/POSIX writes never EIO. When
	// true (opt-in per-share via config["require_durable_commit"]), CLOSE/
	// COMMIT only succeed when the data is on a durable store
	// (localDurable || (Finalized && remoteDurable)), trading latency for
	// synchronous durability on non-fs-local stores. Set once at
	// construction; read on the commit path. fs-local is always durable so
	// the flag is a no-op there.
	requireDurableCommit bool
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
		fileChunkStore:  cfg.FileChunkStore,
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
	// Thread the SyncedHashStore into the Syncer so the carver can commit
	// synced markers + block locators atomically. Nil is accepted
	// (local-only / no-remote fixtures); carve stays disabled in that
	// mode.
	if cfg.SyncedHashStore != nil {
		cfg.Syncer.SetSyncedHashStore(cfg.SyncedHashStore)
	}
	// Chunk-lifecycle hooks are gone with the journal switchover: chunking now
	// happens at carve time inside the journal, and the carve BlockSink writes
	// the per-(file,offset) FileChunk manifest rows atomically in its commit
	// transaction (metadata.DefaultCommitBlock). There is no rollup-completion
	// persister, no write-side cache warm hook, and no per-chunk emitter.
	// wire the Store back-reference onto the Syncer so it can reach the
	// owning Store for dataplane metrics (Syncer.dataplaneMetrics) and
	// cache access (InvalidateFile on delete). Reading through the
	// back-reference (instead of caching a cacheInterface field on the
	// Syncer at construction time) lets test code swap `bs.cache = rec`
	// post-construction and still observe the invalidation — mirrors the
	// TestClose_ClosesCache pattern.
	cfg.Syncer.bs = bs
	return bs, nil
}

// Start initializes the store and starts background goroutines. The journal
// local store recovers its own index on Open, so there is no engine-driven
// recovery step; the syncer and (optional) migration/cache goroutines start
// here.
func (bs *Store) Start(ctx context.Context) error {
	// Start local store background goroutines (no-op for the journal store).
	// Use background context so these outlive the calling request context.
	bs.local.Start(context.Background())

	// One-shot cas→blocks migration: import pre-flip local per-chunk files
	// into the log-blob substrate and re-pack standalone remote objects into
	// packed blocks. Runs in the background so a slow/stalled remote or a
	// near-full disk can't wedge startup — the share serves immediately and any
	// not-yet-repacked standalone chunk is read through the legacy fallback
	// (resolveAndReadChunk). Idempotent and resumable: a failed or cancelled
	// pass is a no-op retry on the next start. Uses a detached context so it
	// outlives Start; Close cancels it and waits on migrateDone before tearing
	// down the stores it uses.
	migrateCtx, cancel := context.WithCancel(context.Background())
	bs.migrateCancel = cancel
	bs.migrateDone = make(chan struct{})
	go func() {
		defer close(bs.migrateDone)
		if err := bs.migrateLegacyCAS(migrateCtx); err != nil && migrateCtx.Err() == nil {
			logger.Warn("cas→blocks migration: background pass failed; will retry next start", "error", err)
		}
	}()

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
// legacy FileChunk → GetBlockData fallback).
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
func (bs *Store) loadByHash(_ context.Context, _ block.ContentHash) ([]byte, error) {
	// The journal local store is (payloadID,offset)-keyed, not hash-keyed, so
	// there is no local content-addressed read to prefetch through. Prefetch by
	// hash is therefore a no-op miss; cold reads hydrate covering chunks from
	// the remote on demand (read_internal.go). ponytail: the CAS read cache is
	// now hint-only.
	return nil, block.ErrChunkNotFound
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

	// Stop the background cas→blocks migration before tearing down the local,
	// remote, and syncer stores it uses. Cancel unblocks any in-flight remote
	// PutBlock/GET; the receive waits for the goroutine to fully exit so it
	// never races the closes below. Safe from under closeMu: the migration
	// goroutine does not take closeMu.
	if bs.migrateCancel != nil {
		bs.migrateCancel()
		<-bs.migrateDone
	}

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

// rollupStopper is the narrow, consumer-defined capability a local store
// exposes to stop + drain its rollup worker pool WITHOUT a full Close. Only
// *fs.FSStore implements it (memory stores have no rollup pool), so absence is
// a benign no-op.
type rollupStopper interface {
	GracefulStopRollup(grace time.Duration) error
}

// StopRollup stops and drains the local store's rollup worker pool, if it has
// one, using the given grace deadline (grace <= 0 defers to the store default).
//
// This is the shutdown-ordering fence for #1543. The rollup ticker persists
// FileChunk manifest rows and rollup offsets THROUGH the metadata store, so it
// must be quiesced BEFORE the runtime closes the metadata stores — otherwise an
// in-flight rollup races the DB close and fails with "sql: database is closed",
// which can leave a chunk dropped locally but never mirrored to the remote.
//
// The block store itself stays open (fds intact); the full teardown still
// happens in the engine Close driven by RemoveShare. Idempotent: the underlying
// GracefulStopRollup guards with stopRollupOnce, so the later Close is a no-op.
func (bs *Store) StopRollup(grace time.Duration) error {
	rs, ok := bs.local.(rollupStopper)
	if !ok {
		return nil
	}
	return rs.GracefulStopRollup(grace)
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
	// The injected recorder (*metrics.Metrics in production) also carries the
	// data-plane upload instruments; retain it for the mirror/upload path.
	if dm, ok := rec.(DataplaneMetrics); ok {
		bs.metrics.Store(&dm)
	}
}

// --- Test helpers ---

// LocalForTest returns the engine's underlying local store as the
// local.LocalStore interface. Used by cross-package test fixtures
// (e.g. internal/adapter/common) that need to drive rollup or other
// admin paths against the concrete *fs.FSStore via a type assertion.
// Do not use in production code.
func (bs *Store) LocalForTest() local.LocalStore { return bs.local }

// LocalStore returns nil: the journal-backed local tier is a per-file byte
// cache, not a content-addressed block.Store, so there is no hash-namespace to
// sweep. The journal self-manages local segment reclaim (dead-byte GC +
// pressure eviction) internally, and the remote-tier FileChunk reap/refcount
// GC runs on gc_block.go. Controlplane's ShareLocalStores() skips a nil local
// store, so per-share local GC (CollectGarbageLocal) is a natural no-op.
func (bs *Store) LocalStore() block.Store { return nil }

// LocalDurable reports whether the engine's local store survives a process
// crash / restart (block.DurabilityReporter). It is the localDurable input to
// the honest CLOSE/COMMIT commit rule (#1274). When the local store does not
// implement DurabilityReporter the conservative default (false) is returned so
// the server never over-promises durability — every production local backend
// (fs) implements it, so this only affects bare test fixtures.
func (bs *Store) LocalDurable() bool {
	return block.IsDurable(bs.local)
}

// RemoteDurable reports whether the engine's remote store survives a process
// crash / restart. A nil remote (local-only share) and a remote that does not
// implement DurabilityReporter both fall back to the conservative default
// (false) via block.IsDurable. The report sees through the encryption /
// compression decorators because they delegate Durable() to the store they wrap.
func (bs *Store) RemoteDurable() bool {
	return block.IsDurable(bs.remote)
}

// RequireDurableCommit reports whether this share enforces the strict
// honest-CLOSE/COMMIT durability rule (#1274). When false (the default), the
// commit seam acks once engine.Flush succeeds and the remote mirror stays
// async. When true, CLOSE/COMMIT only succeed once the data reaches a durable
// store. Configured per-share via config["require_durable_commit"].
func (bs *Store) RequireDurableCommit() bool {
	return bs.requireDurableCommit
}

// SetRequireDurableCommit sets the strict honest-CLOSE/COMMIT durability
// policy for this share. Called once at construction by the shares service
// from the per-share config["require_durable_commit"] key (default false).
func (bs *Store) SetRequireDurableCommit(v bool) {
	bs.requireDurableCommit = v
}

// RemoteStore returns the per-share remote object store, or nil if the
// share is local-only. Used by the snapshot sync-gate verify step
// to drive VerifyRemoteDurability after DrainAllUploads, and by
// cross-package tests for shared-remote identity checks.
func (bs *Store) RemoteStore() remote.RemoteStore { return bs.remote }

// ListFiles returns the payloadIDs of all files with live local data in the
// journal. Callers needing every payload (including fully-carved-and-evicted
// ones) should enumerate the authoritative metadata via the fileChunk store's
// EnumeratePayloads instead.
func (bs *Store) ListFiles() []string { return bs.local.ListFiles(context.Background()) }

// EvictLocal drops a file's local cached bytes. In the journal model the local
// tier is segment-oriented and self-evicts under storage pressure; there is no
// per-file "drop-but-keep-rehydratable" primitive (Delete tombstones the file
// so it would not re-hydrate). Callers that need to force a file cold should
// use DrainLocalSynced. ponytail: per-file forced evict is a no-op — the
// journal owns eviction.
func (bs *Store) EvictLocal(ctx context.Context, payloadID string) error {
	if err := bs.enter(); err != nil {
		return err
	}
	defer bs.closeMu.RUnlock()
	_ = payloadID
	return nil
}

// DrainLocalSynced evicts every locally-resident, remote-durable segment,
// returning the bytes freed. It is the on-demand path the shares evict admin
// uses to force reads back onto the remote (cold-read benchmarking). Unsynced
// (remote-missing) data is never dropped — journal.Evict skips any segment
// holding a dirty record.
func (bs *Store) DrainLocalSynced(ctx context.Context) (int64, error) {
	if err := bs.enter(); err != nil {
		return 0, err
	}
	defer bs.closeMu.RUnlock()
	res, err := bs.local.Evict(ctx, 1<<62)
	return res.BytesFreed, err
}

// WarmAll proactively fetches every remote block of every payload in this
// share onto the local CAS tier, delegating to the syncer's WarmAll under the
// store's close-gate so a concurrent Close drains the run instead of racing
// the local/syncer/remote teardown. See (*Syncer).WarmAll for semantics
// (bounded by ParallelDownloads, errors on a missing remote, terminal on
// ErrDiskFull, honors ctx cancellation). progress may be nil.
func (bs *Store) WarmAll(ctx context.Context, progress func(done, total int64)) (WarmResult, error) {
	if err := bs.enter(); err != nil {
		return WarmResult{}, err
	}
	defer bs.closeMu.RUnlock()
	return bs.syncer.WarmAll(ctx, progress)
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

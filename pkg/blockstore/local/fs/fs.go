package fs

import (
	"container/list"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/local"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// retentionConfig holds retention policy settings read/written atomically.
type retentionConfig struct {
	policy blockstore.RetentionPolicy
	ttl    time.Duration
}

// Errors returned by FSStore.
var (
	ErrStoreClosed    = errors.New("local store: closed")
	ErrDiskFull       = errors.New("local store: disk full after eviction")
	ErrFileNotInStore = errors.New("file not in local store")

	// ErrBlockNotFound is an alias for blockstore.ErrBlockNotFound.
	ErrBlockNotFound = blockstore.ErrBlockNotFound

	// errLRUEmpty is returned by lruEvictOne when there are no candidates left.
	errLRUEmpty = errors.New("local store: LRU empty, no eviction candidates")
)

// Compile-time interface satisfaction check.
var _ local.LocalStore = (*FSStore)(nil)

// FSStore is a two-tier (memory + disk) block store for file data.
//
// NFS WRITE operations (typically 4KB) are buffered in 8MB in-memory blocks.
// When a block fills up or NFS COMMIT is called, the block is flushed atomically
// to a .blk file on disk. This design avoids per-4KB disk I/O and prevents OS
// page cache bloat that caused OOM on earlier versions.
//
// Block metadata (local path, upload state, etc.) is tracked via FileBlockStore
// (BadgerDB) with async batching -- writes are queued in pendingFBs and flushed
// every 200ms by the background goroutine started via Start().
//
// Thread safety: memBlocks uses a dedicated RWMutex (blocksMu) separate from
// the files map (filesMu). Operations on different blocks are fully concurrent
// for reads (RLock). Operations on the same block are serialized via memBlock.mu.
//
// Lock ordering: blocksMu -> mb.mu (never the reverse).
// In flushBlock, the map entry is deleted while holding mb.mu to prevent a race
// where a concurrent writer gets a stale memBlock with nil data.
type FSStore struct {
	baseDir    string
	maxDisk    int64
	maxMemory  int64
	blockStore blockstore.FileBlockStore

	// blocksMu guards the memBlocks and fileBlocks maps. Uses RWMutex for
	// concurrent reads (the common case: checking if a block is already buffered).
	// RWMutex outperforms sync.Map for write-heavy workloads with high key
	// churn (random writes that create/flush/recreate blocks frequently).
	blocksMu   sync.RWMutex
	memBlocks  map[blockKey]*memBlock
	fileBlocks map[string]map[uint64]*memBlock // payloadID -> blockIdx -> mb

	// filesMu guards the files map separately from block operations.
	filesMu sync.RWMutex
	files   map[string]*fileInfo

	closedFlag atomic.Bool

	memUsed  atomic.Int64
	diskUsed atomic.Int64

	// flushFsyncCount counts how many times flushBlock has invoked fsync.
	// Test-only observable surfaced via FlushFsyncCountForTest. Incremented
	// only on the COMMIT-driven path (withFsync=true); pressure and
	// block-fill paths flush without fsync and do not bump this counter.
	flushFsyncCount atomic.Int64

	// pendingFBs queues FileBlock metadata updates for async persistence.
	// Keyed by blockID (string) -> *blockstore.FileBlock.
	// Drained every 200ms by SyncFileBlocks, and on Close/Flush.
	pendingFBs sync.Map

	// diskIndex is the authoritative in-process cache of FileBlock metadata
	// for blocks currently stored on the local filesystem. It is populated
	// whenever a block lands on disk (flushBlock, tryDirectDiskWrite eager-
	// create, WriteFromRemote, Recover) and pruned on delete/evict. The
	// write hot path and eviction consult diskIndex INSTEAD of the
	// FileBlockStore, decoupling those paths from the underlying metadata
	// backend (TD-02d / D-19).
	//
	// Entries survive pendingFBs drain so subsequent hot-path operations
	// (e.g., a second pwrite into the same block after BadgerDB persistence)
	// can still observe the block's existing State / BlockStoreKey without
	// a FileBlockStore round-trip.
	//
	// Keyed by blockID (string) -> *blockstore.FileBlock. The stored pointer
	// is SHARED with pendingFBs so queued updates mutate a single instance.
	diskIndex sync.Map

	// fdPool pools open file descriptors for .blk files to avoid
	// open+close syscalls on every 4KB random write in tryDirectDiskWrite.
	fdPool *fdPool

	// readFDPool pools open file descriptors (O_RDONLY) for .blk files
	// to avoid open+close syscalls on every 4KB random read in readFromDisk.
	readFDPool *fdPool

	// evictionEnabled controls whether ensureSpace can evict blocks.
	// When false, ensureSpace returns ErrDiskFull if over limit instead of
	// evicting remote blocks. Used by local-only mode where there is no
	// remote store to re-fetch evicted blocks from.
	evictionEnabled atomic.Bool

	// retention holds the retention policy and TTL, accessed atomically
	// to avoid data races between SetRetentionPolicy and concurrent eviction.
	retention unsafe.Pointer // *retentionConfig

	// accessTracker maintains per-file last-access times for eviction ordering.
	// Updated on read/write paths via Touch(), queried during eviction.
	accessTracker *accessTracker

	// done signals the Start goroutine to exit. Closed exactly once by Close
	// (guarded by closeOnce) so the goroutine can terminate independently of
	// the ctx passed to Start. wg joins the Start goroutine so Close() returns
	// only after the goroutine has exited — no leaked goroutines across
	// repeated Start/Close cycles.
	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup

	// --- Append-log path (Phase 10, LSL-01/03, flag-gated per D-02/D-36). ---
	//
	// When useAppendLog=false (default through Phase 10 per D-03), every
	// field in this block is its zero value and FSStore is byte-for-byte
	// equivalent to the Phase 09 write path: no log file is created on disk,
	// no rollup worker is started, and AppendWrite rejects with
	// ErrAppendLogDisabled.
	//
	// maxLogBytes and stabilizationMS / rollupWorkers are kept populated with
	// their defaults even when the flag is off so NewWithOptions can raise
	// the flag without a second initialization pass.
	useAppendLog    bool
	maxLogBytes     int64
	stabilizationMS int
	rollupWorkers   int

	// logBytesTotal is the current total bytes of un-rolled-up log content
	// across every payloadID in this FSStore. Incremented by AppendWrite
	// (framed-record size) and decremented by the rollup (Phase 10-06).
	// AppendWrite blocks on pressureCh when logBytesTotal > maxLogBytes
	// (D-14, D-15).
	logBytesTotal atomic.Int64

	// pressureCh is a buffered channel (size 1) that the rollup pulses after
	// freeing log budget. AppendWrite selects on it inside the pressure loop
	// along with ctx.Done() and bc.done.
	pressureCh chan struct{}

	// logsMu guards logFDs, logLocks, dirtyIntervals, and tombstones.
	// Double-checked locking idiom matches blocksMu / memBlocks in
	// getOrCreateMemBlock.
	logsMu         sync.RWMutex
	logFDs         map[string]*logFile      // payloadID -> open log fd wrapper (D-34: one fd per file, bypasses fdPool)
	logLocks       map[string]*sync.Mutex   // payloadID -> per-file append mutex (D-32)
	dirtyIntervals map[string]*intervalTree // payloadID -> dirty-region tree (D-16)
	// tombstones marks payloadIDs whose append log has been (or is being)
	// deleted by DeleteAppendLog (plan 09). rollupFile (plan 06) consults
	// this BEFORE and AFTER the per-file mutex hand-off to ensure no
	// rollup_offset gets persisted for a dead payload. Initialized in New
	// alongside the other logs-* maps.
	tombstones map[string]struct{}

	// truncations records per-payload truncation boundaries set by
	// TruncateAppendLog (D-29 / plan 09). rollupFile consults this after
	// reading the record batch and filters / clips records whose
	// file_offset >= boundary so the emitted chunk stream never includes
	// bytes past the truncation point. Entries are cleared when the
	// payload is deleted; they persist otherwise so subsequent rollup
	// passes keep honoring the boundary.
	truncations map[string]uint64

	// --- Phase 10-06 rollup pool (D-13/D-33). ---
	//
	// When useAppendLog=false, these fields remain their zero values and
	// StartRollup rejects with ErrAppendLogDisabled. When the flag is on,
	// StartRollup launches bc.rollupWorkers goroutines that consume
	// payloadIDs from bc.rollupCh (AppendWrite non-blocking send) and also
	// scan bc.dirtyIntervals on a stabilization-tuned ticker.
	rollupStore   metadata.RollupStore
	rollupCh      chan string
	rollupStarted atomic.Bool
	rollupWg      sync.WaitGroup

	// --- Phase 11 LSL-08: in-process LRU for CAS chunks. ---
	//
	// Eviction is driven entirely from on-disk presence under
	// blocks/{hh}/{hh}/{hex}, indexed by ContentHash in lruIndex/lruList.
	// Eviction = unlink the file directly; no FileBlockStore lookup happens
	// on the write hot path (D-27).
	//
	// Population:
	//   - StoreChunk(...)   -> lruTouch on rename success.
	//   - ReadChunk(...)    -> lruTouch on read success (re-promote).
	//   - seedLRUFromDisk() -> alphabetical scan at New() so warm-started
	//                          stores have a deterministic LRU position.
	//
	// Mutations (Touch / EvictOne) are serialized by lruMu. Disk unlinks
	// happen under lruMu; concurrent ReadChunk that races an evict surfaces
	// as ENOENT and falls through to the engine refetch path (T-11-B-08).
	lruMu    sync.Mutex
	lruIndex map[blockstore.ContentHash]*list.Element // *lruEntry
	lruList  *list.List                               // most-recent at front

	// orphanLogMinAgeSeconds gates the append-log orphan sweep during
	// recovery (D-28 / Warning 3). A log is considered orphan only when
	// (a) its rollup_offset in metadata is 0, (b) no FileBlock exists for
	// block-0 of the payloadID, AND (c) the log file's mtime is older
	// than this threshold. The mtime gate guarantees freshly-created logs
	// with no rolled-up metadata yet are never swept at boot.
	//
	// Default 3600 (1h) is set by NewWithOptions when the option is left
	// at zero. Configurable via
	// `blockstore.local.fs.orphan_log_min_age_seconds` (plan 08 wiring).
	orphanLogMinAgeSeconds int
}

// New creates a new FSStore.
//
// Parameters:
//   - baseDir: directory for .blk block files, created if absent.
//   - maxDisk: maximum total size of on-disk .blk files in bytes. 0 = unlimited.
//   - maxMemory: memory budget for dirty write buffers in bytes. 0 defaults to 256MB.
//   - fileBlockStore: persistent store for FileBlock metadata (local path, upload state, etc.)
func New(baseDir string, maxDisk int64, maxMemory int64, fileBlockStore blockstore.FileBlockStore) (*FSStore, error) {
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("local store: create base dir: %w", err)
	}

	if maxMemory <= 0 {
		maxMemory = 256 * 1024 * 1024 // 256MB default
	}

	defaultRetention := &retentionConfig{policy: blockstore.RetentionLRU}
	bc := &FSStore{
		baseDir:       baseDir,
		maxDisk:       maxDisk,
		maxMemory:     maxMemory,
		blockStore:    fileBlockStore,
		memBlocks:     make(map[blockKey]*memBlock),
		fileBlocks:    make(map[string]map[uint64]*memBlock),
		files:         make(map[string]*fileInfo),
		fdPool:        newFDPool(defaultFDPoolSize),
		readFDPool:    newFDPool(defaultFDPoolSize),
		accessTracker: newAccessTracker(),
		retention:     unsafe.Pointer(defaultRetention),
		done:          make(chan struct{}),
	}
	bc.evictionEnabled.Store(true)

	// Phase 11 LSL-08: in-process LRU for CAS chunks (see field comments).
	bc.lruIndex = make(map[blockstore.ContentHash]*list.Element)
	bc.lruList = list.New()

	// Phase 10 append-log plumbing — maps + pressure channel always
	// initialized so NewWithOptions can enable the flag atomically. When
	// useAppendLog=false (the New() default), AppendWrite returns
	// ErrAppendLogDisabled and nothing else in this block touches disk.
	// D-36: zero behavior change when the flag is off.
	bc.pressureCh = make(chan struct{}, 1)
	bc.logFDs = make(map[string]*logFile)
	bc.logLocks = make(map[string]*sync.Mutex)
	bc.dirtyIntervals = make(map[string]*intervalTree)
	bc.tombstones = make(map[string]struct{})
	bc.truncations = make(map[string]uint64)
	bc.maxLogBytes = 1 << 30 // 1 GiB default (D-14)
	bc.stabilizationMS = 250 // D-16 default
	bc.rollupWorkers = 2     // D-13/D-33 default
	// rollupCh buffered so AppendWrite's non-blocking send rarely drops;
	// on drop, the ticker arm in chunkRollupWorker picks up the payload
	// on the next scan.
	bc.rollupCh = make(chan string, bc.rollupWorkers*4)

	// Seed the in-process LRU (LSL-08) from any chunks already present on
	// disk under blocks/{hh}/{hh}/{hex}. Cold-start order is alphabetical
	// for determinism; subsequent reads/writes promote chunks to the front.
	bc.seedLRUFromDisk()

	return bc, nil
}

// lruEntry is a single LRU-tracked CAS chunk on disk.
type lruEntry struct {
	hash blockstore.ContentHash
	size int64
	path string // absolute path under <baseDir>/blocks/<hh>/<hh>/<hex>
}

// lruTouch promotes (or inserts) a chunk to the most-recent end of the LRU.
// Called from StoreChunk on rename success and from ReadChunk on cache hit.
// Safe to call from concurrent goroutines.
func (bc *FSStore) lruTouch(h blockstore.ContentHash, size int64, path string) {
	bc.lruMu.Lock()
	defer bc.lruMu.Unlock()
	if el, ok := bc.lruIndex[h]; ok {
		bc.lruList.MoveToFront(el)
		// Update size/path in case the chunk was re-stored (idempotent CAS).
		entry := el.Value.(*lruEntry)
		entry.size = size
		entry.path = path
		return
	}
	el := bc.lruList.PushFront(&lruEntry{hash: h, size: size, path: path})
	bc.lruIndex[h] = el
}

// lruEvictOne removes the least-recently-used chunk from the LRU and
// unlinks its on-disk file. Returns the freed byte count, or 0 + sentinel
// when the LRU is empty. Concurrent ReadChunk that races an evict surfaces
// as blockstore.ErrChunkNotFound (T-11-B-08, accept/refetch posture).
func (bc *FSStore) lruEvictOne() (int64, error) {
	bc.lruMu.Lock()
	el := bc.lruList.Back()
	if el == nil {
		bc.lruMu.Unlock()
		return 0, errLRUEmpty
	}
	entry := el.Value.(*lruEntry)
	bc.lruList.Remove(el)
	delete(bc.lruIndex, entry.hash)
	bc.lruMu.Unlock()

	if err := os.Remove(entry.path); err != nil && !os.IsNotExist(err) {
		// File system error: re-insert to avoid losing the bookkeeping
		// (the chunk is still on disk, it just couldn't be unlinked).
		bc.lruMu.Lock()
		bc.lruIndex[entry.hash] = bc.lruList.PushBack(entry)
		bc.lruMu.Unlock()
		return 0, fmt.Errorf("evict %s: %w", entry.path, err)
	}
	return entry.size, nil
}

// seedLRUFromDisk walks <baseDir>/blocks/ at startup and registers every
// chunk file in the LRU. Order is deterministic (alphabetical hash hex)
// so repeated startups produce the same cold-start eviction order.
//
// Best-effort: any per-file error is silently skipped (the next StoreChunk
// or ReadChunk will register the chunk on demand).
func (bc *FSStore) seedLRUFromDisk() {
	blocksDir := filepath.Join(bc.baseDir, "blocks")
	if _, err := os.Stat(blocksDir); err != nil {
		return
	}
	type seed struct {
		hash blockstore.ContentHash
		size int64
		path string
	}
	var seeds []seed
	_ = filepath.WalkDir(blocksDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		// Path layout: <baseDir>/blocks/<hh>/<hh>/<hex>.
		name := d.Name()
		if len(name) != blockstore.HashSize*2 {
			return nil
		}
		raw, err := hex.DecodeString(name)
		if err != nil || len(raw) != blockstore.HashSize {
			return nil
		}
		var h blockstore.ContentHash
		copy(h[:], raw)
		info, err := d.Info()
		if err != nil {
			return nil
		}
		seeds = append(seeds, seed{hash: h, size: info.Size(), path: path})
		return nil
	})
	// Sort alphabetically by hash hex for deterministic cold-start order.
	// Files visited by WalkDir are already lexicographically ordered within
	// directories, but cross-directory order depends on filesystem
	// traversal — sort explicitly to be safe.
	sort.Slice(seeds, func(i, j int) bool {
		return seeds[i].hash.String() < seeds[j].hash.String()
	})
	for _, s := range seeds {
		bc.lruTouch(s.hash, s.size, s.path)
	}
}

// FSStoreOptions configures the Phase 10 append-log path. Zero value means
// the append log is disabled (D-03 default through Phase 10); setting
// UseAppendLog=true opts into the new write path wired by
// `AppendWrite`. MaxLogBytes, RollupWorkers, and StabilizationMS default
// via New() when left zero here.
type FSStoreOptions struct {
	UseAppendLog    bool
	MaxLogBytes     int64
	RollupWorkers   int
	StabilizationMS int
	// RollupStore persists per-file rollup_offset (LSL-05). Required when
	// UseAppendLog=true AND StartRollup will be called. Nil is accepted when
	// UseAppendLog is false (the flag path is fully bypassed) or when the
	// caller will not start the rollup pool.
	RollupStore metadata.RollupStore
	// OrphanLogMinAgeSeconds is the minimum age (seconds) a log file must
	// have before recovery will classify it as orphan and sweep it.
	// Defaults to 3600 (1h) when zero. See FSStore.orphanLogMinAgeSeconds
	// and D-28 / Warning 3 in 10-CONTEXT.md for the rationale.
	OrphanLogMinAgeSeconds int
}

// NewWithOptions constructs an FSStore with the append-log path optionally
// enabled. When opts.UseAppendLog is false (the default through Phase 10
// per D-03), the returned store is byte-for-byte identical to one from
// New() — no log file on disk, no rollup worker. Phase 11 (A2) flips the
// default.
//
// Non-zero values in opts override the defaults set by New(); zero values
// keep the New() defaults (1 GiB log, 250ms stabilization, 2 rollup
// workers).
func NewWithOptions(baseDir string, maxDisk, maxMemory int64, fileBlockStore blockstore.FileBlockStore, opts FSStoreOptions) (*FSStore, error) {
	bc, err := New(baseDir, maxDisk, maxMemory, fileBlockStore)
	if err != nil {
		return nil, err
	}
	bc.useAppendLog = opts.UseAppendLog
	if opts.MaxLogBytes > 0 {
		bc.maxLogBytes = opts.MaxLogBytes
	}
	if opts.RollupWorkers > 0 {
		bc.rollupWorkers = opts.RollupWorkers
		// Re-size rollupCh to match the caller-specified pool size so the
		// non-blocking send in AppendWrite has proportional headroom.
		bc.rollupCh = make(chan string, bc.rollupWorkers*4)
	}
	if opts.StabilizationMS > 0 {
		bc.stabilizationMS = opts.StabilizationMS
	}
	bc.rollupStore = opts.RollupStore
	if opts.OrphanLogMinAgeSeconds > 0 {
		bc.orphanLogMinAgeSeconds = opts.OrphanLogMinAgeSeconds
	} else {
		bc.orphanLogMinAgeSeconds = 3600
	}
	return bc, nil
}

// SetRetentionPolicy updates the retention policy and TTL for eviction decisions.
// Called during share creation and on runtime policy updates.
// Safe for concurrent use with ensureSpace/eviction (uses atomic pointer swap).
//   - pin: never evict local blocks (ensureSpace returns ErrDiskFull)
//   - ttl: evict only after file last-access exceeds ttl duration
//   - lru: evict least-recently-accessed blocks first (default)
func (bc *FSStore) SetRetentionPolicy(policy blockstore.RetentionPolicy, ttl time.Duration) {
	cfg := &retentionConfig{policy: policy, ttl: ttl}
	atomic.StorePointer(&bc.retention, unsafe.Pointer(cfg))
}

// getRetention returns the current retention config atomically.
func (bc *FSStore) getRetention() retentionConfig {
	return *(*retentionConfig)(atomic.LoadPointer(&bc.retention))
}

// Close flushes pending FileBlock metadata and marks the store as closed.
// After Close, all read/write operations return ErrStoreClosed.
//
// Close is idempotent: multiple calls signal the background goroutine launched
// by Start() exactly once (via closeOnce), then deterministically join the
// goroutine via wg.Wait() before returning. This prevents the goroutine leak
// previously possible when Start's ctx outlived Close (TD-02a).
func (bc *FSStore) Close() error {
	bc.closedFlag.Store(true)
	bc.closeOnce.Do(func() {
		close(bc.done)
	})
	// Join the Start goroutine (if any) before proceeding with teardown.
	// Safe to call even when Start was never invoked — Wait() on a zero
	// counter returns immediately.
	bc.wg.Wait()
	// Join rollup workers (if any were started by StartRollup). Zero-counter
	// Wait() is a no-op when the flag is off. Must run before we close log
	// fds below so no rollup goroutine touches an already-closed fd.
	bc.rollupWg.Wait()
	bc.SyncFileBlocks(context.Background())
	bc.fdPool.CloseAll()
	bc.readFDPool.CloseAll()

	// Close append-log fds (Phase 10). Safe after closedFlag.Store(true) +
	// wg.Wait above: no new AppendWrite can acquire an fd (isClosed guard),
	// and any rollup worker (Phase 10-06) has already joined via wg.
	bc.logsMu.Lock()
	for pid, lf := range bc.logFDs {
		if lf != nil && lf.f != nil {
			_ = lf.f.Close()
		}
		delete(bc.logFDs, pid)
	}
	bc.logsMu.Unlock()
	return nil
}

func (bc *FSStore) isClosed() bool {
	return bc.closedFlag.Load()
}

// Start launches the background goroutine that periodically persists queued
// FileBlock metadata updates to BadgerDB. This batches many small PutFileBlock
// calls (one per 4KB NFS write) into fewer store writes (every 200ms).
//
// Must be called after New and before any writes.
// The goroutine stops on either ctx cancellation or Close() (whichever fires
// first), with a final drain on exit. Close() waits for the goroutine to
// terminate so teardown is deterministic — see TD-02a.
func (bc *FSStore) Start(ctx context.Context) {
	bc.wg.Add(1)
	go func() {
		defer bc.wg.Done()
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				bc.SyncFileBlocks(context.Background())
				return
			case <-bc.done:
				bc.SyncFileBlocks(context.Background())
				return
			case <-ticker.C:
				bc.SyncFileBlocks(ctx)
			}
		}
	}()
}

// SyncFileBlocks persists all queued FileBlock metadata updates to the store.
// Called periodically by Start(), on Close(), and before GetDirtyBlocks()
// to ensure the FileBlockStore is up-to-date for ListLocalBlocks queries.
func (bc *FSStore) SyncFileBlocks(ctx context.Context) {
	bc.pendingFBs.Range(func(key, value any) bool {
		fb := value.(*blockstore.FileBlock)
		if err := bc.blockStore.PutFileBlock(ctx, fb); err == nil {
			bc.pendingFBs.Delete(key)
		}
		return true
	})
}

// SyncFileBlocksForFile persists queued FileBlock metadata only for blocks
// belonging to the given payloadID. Much cheaper than SyncFileBlocks during
// random writes to many files, since it skips unrelated blocks.
func (bc *FSStore) SyncFileBlocksForFile(ctx context.Context, payloadID string) {
	bc.pendingFBs.Range(func(key, value any) bool {
		blockID := key.(string)
		if !belongsToFile(blockID, payloadID) {
			return true
		}
		fb := value.(*blockstore.FileBlock)
		if err := bc.blockStore.PutFileBlock(ctx, fb); err == nil {
			bc.pendingFBs.Delete(key)
		}
		return true
	})
}

// queueFileBlockUpdate queues a FileBlock metadata update for async persistence
// AND registers the block in the in-process diskIndex so the write hot path and
// eviction can see it without a FileBlockStore round-trip (TD-02d / D-19).
//
// pendingFBs and diskIndex share the same *FileBlock pointer, so mutations
// applied by the hot path (e.g., fb.LastAccess = now) are visible to both the
// drain goroutine and subsequent hot-path lookups.
func (bc *FSStore) queueFileBlockUpdate(fb *blockstore.FileBlock) {
	bc.pendingFBs.Store(fb.ID, fb)
	bc.diskIndex.Store(fb.ID, fb)
}

// diskIndexStore registers a FileBlock in the in-process diskIndex without
// queuing an async persistence. Used by Recover to seed the index from disk
// state without triggering redundant PutFileBlock writes.
func (bc *FSStore) diskIndexStore(fb *blockstore.FileBlock) {
	bc.diskIndex.Store(fb.ID, fb)
}

// diskIndexLookup returns the FileBlock metadata for a block currently on
// disk, or (nil, false) if no entry exists. This is the hot-path replacement
// for the FileBlockStore.GetFileBlock + pendingFBs fallback: all callers on
// the write hot path and eviction must use this instead of lookupFileBlock.
func (bc *FSStore) diskIndexLookup(blockID string) (*blockstore.FileBlock, bool) {
	v, ok := bc.diskIndex.Load(blockID)
	if !ok {
		return nil, false
	}
	return v.(*blockstore.FileBlock), true
}

// diskIndexDelete removes a block from the in-process diskIndex. Called when
// a block is deleted (DeleteBlockFile) or evicted.
func (bc *FSStore) diskIndexDelete(blockID string) {
	bc.diskIndex.Delete(blockID)
}

// lookupFileBlock retrieves a FileBlock, checking the pending queue first
// (for recently-written metadata not yet persisted) then falling back to the
// store. NOTE: this helper hits the FileBlockStore and MUST NOT be called
// from the local write hot path or from eviction (TD-02d / D-19). Non-hot-
// path callers (recovery, manage, state transitions) may use it.
func (bc *FSStore) lookupFileBlock(ctx context.Context, blockID string) (*blockstore.FileBlock, error) {
	if v, ok := bc.pendingFBs.Load(blockID); ok {
		return v.(*blockstore.FileBlock), nil
	}
	return bc.blockStore.GetFileBlock(ctx, blockID)
}

// Stats returns a snapshot of current local store statistics.
func (bc *FSStore) Stats() local.Stats {
	bc.filesMu.RLock()
	fileCount := len(bc.files)
	bc.filesMu.RUnlock()

	var memBlockCount int
	bc.blocksMu.RLock()
	for _, mb := range bc.memBlocks {
		mb.mu.RLock()
		if mb.data != nil {
			memBlockCount++
		}
		mb.mu.RUnlock()
	}
	bc.blocksMu.RUnlock()

	return local.Stats{
		DiskUsed:      bc.diskUsed.Load(),
		MaxDisk:       bc.maxDisk,
		MemUsed:       bc.memUsed.Load(),
		MaxMemory:     bc.maxMemory,
		FileCount:     fileCount,
		MemBlockCount: memBlockCount,
	}
}

// getOrCreateMemBlock returns the memBlock for the given key, creating one with
// a pre-allocated 8MB buffer if it doesn't exist. The pre-allocation avoids
// allocation jitter on the write hot path.
//
// Uses double-checked locking: RLock fast path for existing blocks, Lock for creation.
func (bc *FSStore) getOrCreateMemBlock(key blockKey) *memBlock {
	bc.blocksMu.RLock()
	mb, exists := bc.memBlocks[key]
	bc.blocksMu.RUnlock()
	if exists {
		return mb
	}

	bc.blocksMu.Lock()
	mb, exists = bc.memBlocks[key]
	if !exists {
		mb = &memBlock{
			data: getBlockBuf(),
		}
		bc.memBlocks[key] = mb
		// Maintain per-file secondary index
		fm := bc.fileBlocks[key.payloadID]
		if fm == nil {
			fm = make(map[uint64]*memBlock)
			bc.fileBlocks[key.payloadID] = fm
		}
		fm[key.blockIdx] = mb
		bc.memUsed.Add(int64(blockstore.BlockSize))
	}
	bc.blocksMu.Unlock()
	return mb
}

// getMemBlock returns the memBlock for the given key, or nil if not in memory.
func (bc *FSStore) getMemBlock(key blockKey) *memBlock {
	bc.blocksMu.RLock()
	mb := bc.memBlocks[key]
	bc.blocksMu.RUnlock()
	return mb
}

// updateFileSize updates the tracked file size if the new end offset is larger.
// Uses double-checked locking: RLock fast path for existing files, Lock for creation.
func (bc *FSStore) updateFileSize(payloadID string, end uint64) {
	bc.filesMu.RLock()
	fi, exists := bc.files[payloadID]
	bc.filesMu.RUnlock()

	if !exists {
		bc.filesMu.Lock()
		fi, exists = bc.files[payloadID]
		if !exists {
			fi = &fileInfo{}
			bc.files[payloadID] = fi
		}
		bc.filesMu.Unlock()
	}

	fi.mu.Lock()
	if end > fi.fileSize {
		fi.fileSize = end
	}
	fi.mu.Unlock()
}

// GetFileSize returns the tracked file size and whether the file is tracked.
// This is a fast in-memory lookup -- no disk or store access.
func (bc *FSStore) GetFileSize(_ context.Context, payloadID string) (uint64, bool) {
	bc.filesMu.RLock()
	fi, exists := bc.files[payloadID]
	bc.filesMu.RUnlock()

	if !exists {
		return 0, false
	}

	fi.mu.RLock()
	size := fi.fileSize
	fi.mu.RUnlock()

	return size, true
}

// makeBlockID creates a deterministic block ID string from a blockKey.
// Format: "{payloadID}/{blockIdx}" -- used as the primary key in FileBlockStore.
func makeBlockID(key blockKey) string {
	return fmt.Sprintf("%s/%d", key.payloadID, key.blockIdx)
}

// purgeMemBlocks removes all in-memory blocks for payloadID where shouldRemove returns true.
// Releases the 8MB buffer and decrements memUsed for each removed block.
func (bc *FSStore) purgeMemBlocks(payloadID string, shouldRemove func(blockIdx uint64) bool) {
	bc.blocksMu.Lock()
	fm := bc.fileBlocks[payloadID]
	if fm != nil {
		for blockIdx, mb := range fm {
			if !shouldRemove(blockIdx) {
				continue
			}
			mb.mu.Lock()
			if mb.data != nil {
				bc.memUsed.Add(-int64(blockstore.BlockSize))
				putBlockBuf(mb.data)
				mb.data = nil
			}
			mb.mu.Unlock()
			key := blockKey{payloadID: payloadID, blockIdx: blockIdx}
			delete(bc.memBlocks, key)
			delete(fm, blockIdx)
		}
		if len(fm) == 0 {
			delete(bc.fileBlocks, payloadID)
		}
	}
	bc.blocksMu.Unlock()
}

// EvictMemory removes all in-memory data and disk tracking for a file.
// Does not delete .blk files from disk -- that is handled by eviction or
// explicit deletion via DeleteAllBlockFiles.
func (bc *FSStore) EvictMemory(_ context.Context, payloadID string) error {
	bc.purgeMemBlocks(payloadID, func(uint64) bool { return true })

	bc.filesMu.Lock()
	delete(bc.files, payloadID)
	bc.filesMu.Unlock()

	bc.accessTracker.Remove(payloadID)

	return nil
}

// Truncate discards local blocks beyond newSize and updates the tracked file size.
// Blocks whose start offset (blockIdx * BlockSize) >= newSize are purged from memory.
func (bc *FSStore) Truncate(_ context.Context, payloadID string, newSize uint64) error {
	bc.filesMu.RLock()
	fi, ok := bc.files[payloadID]
	bc.filesMu.RUnlock()

	if ok {
		fi.mu.Lock()
		fi.fileSize = newSize
		fi.mu.Unlock()
	}

	bc.purgeMemBlocks(payloadID, func(blockIdx uint64) bool {
		return blockIdx*blockstore.BlockSize >= newSize
	})
	return nil
}

// ListFiles returns the payloadIDs of all files currently tracked in the local store.
func (bc *FSStore) ListFiles() []string {
	bc.filesMu.RLock()
	defer bc.filesMu.RUnlock()
	result := make([]string, 0, len(bc.files))
	for payloadID := range bc.files {
		result = append(result, payloadID)
	}
	return result
}

// WriteFromRemote stores data that was fetched from the remote block store locally.
// Unlike WriteAt (which creates Dirty blocks), the block is marked Remote
// since it already exists remotely -- making it immediately evictable by the
// disk space manager without needing a re-sync.
//
// Metadata lookup uses the in-process diskIndex first (TD-02d / D-19) so
// repopulation after an evict-then-refetch cycle does not round-trip through
// the FileBlockStore. On a diskIndex miss (the steady-state case after a
// server restart, or when this node never produced the block locally), the
// canonical FileBlock row is fetched from the FileBlockStore so the existing
// CAS Hash + BlockStoreKey are preserved. Without this fallback the local
// store would clobber the CAS row with a zero-hash legacy-key row, after
// which subsequent reads would silently return zero data and GC would reap
// the still-live CAS object as an orphan (Pass-2 CR-2-01).
func (bc *FSStore) WriteFromRemote(ctx context.Context, payloadID string, data []byte, offset uint64) error {
	blockIdx := offset / blockstore.BlockSize
	key := blockKey{payloadID: payloadID, blockIdx: blockIdx}
	blockID := makeBlockID(key)

	fb, ok := bc.diskIndexLookup(blockID)
	if !ok {
		// diskIndex miss: prefer the FileBlockStore row (which carries the
		// CAS Hash + BlockStoreKey populated by the syncer at upload time).
		// Only fall back to a brand-new FileBlock when the row is absent —
		// this is the genuine first-registration path for a never-seen
		// block (e.g. local-only stores with no remote upload trail).
		if existing, err := bc.lookupFileBlock(ctx, blockID); err == nil && existing != nil {
			fb = existing
		} else {
			fb = blockstore.NewFileBlock(blockID, "")
		}
	}
	// Treat the block as evictable now that bytes are landing in the local
	// cache. DO NOT touch fb.Hash or fb.BlockStoreKey here — the canonical
	// CAS metadata is owned by the syncer/uploader and re-stamping would
	// clobber it (CR-2-01).
	fb.State = blockstore.BlockStateRemote

	path := bc.blockPath(blockID)
	if err := bc.ensureSpace(ctx, int64(len(data))); err != nil {
		return err
	}

	if err := writeFile(path, data); err != nil {
		return err
	}

	bc.diskUsed.Add(int64(len(data)))

	fb.LocalPath = path
	fb.DataSize = uint32(len(data))
	fb.LastAccess = time.Now()
	bc.queueFileBlockUpdate(fb)

	end := offset + uint64(len(data))
	bc.updateFileSize(payloadID, end)

	bc.accessTracker.Touch(payloadID)

	return nil
}

// GetBlockData returns the raw data for a specific block, checking memory first
// (for unflushed writes) then disk. Returns ErrBlockNotFound if the block is
// not in either tier.
func (bc *FSStore) GetBlockData(ctx context.Context, payloadID string, blockIdx uint64) ([]byte, uint32, error) {
	key := blockKey{payloadID: payloadID, blockIdx: blockIdx}
	blockID := makeBlockID(key)

	if mb := bc.getMemBlock(key); mb != nil {
		mb.mu.RLock()
		if mb.data != nil && mb.dataSize > 0 {
			data := make([]byte, mb.dataSize)
			copy(data, mb.data[:mb.dataSize])
			dataSize := mb.dataSize
			mb.mu.RUnlock()
			return data, dataSize, nil
		}
		mb.mu.RUnlock()
	}

	fb, err := bc.lookupFileBlock(ctx, blockID)
	if err != nil || fb.LocalPath == "" || fb.DataSize == 0 {
		return nil, 0, ErrBlockNotFound
	}

	data, err := readFile(fb.LocalPath, fb.DataSize)
	if err != nil {
		return nil, 0, err
	}

	return data, fb.DataSize, nil
}

// IsBlockLocal checks if a specific block is available locally (memory or disk).
// Used by the syncer to decide whether to download a block before reading.
func (bc *FSStore) IsBlockLocal(ctx context.Context, payloadID string, blockIdx uint64) bool {
	key := blockKey{payloadID: payloadID, blockIdx: blockIdx}
	// Check memory first (dirty/unflushed blocks)
	if mb := bc.getMemBlock(key); mb != nil {
		mb.mu.RLock()
		hasData := mb.data != nil
		mb.mu.RUnlock()
		if hasData {
			return true
		}
	}
	// Check disk via FileBlockStore metadata
	blockID := makeBlockID(key)
	fb, err := bc.lookupFileBlock(ctx, blockID)
	return err == nil && fb.LocalPath != ""
}

// belongsToFile checks if a blockID (format: "payloadID/blockIdx") belongs to
// the given payloadID by checking the prefix.
func belongsToFile(blockID, payloadID string) bool {
	if len(blockID) <= len(payloadID)+1 {
		return false
	}
	return blockID[:len(payloadID)] == payloadID && blockID[len(payloadID)] == '/'
}

// writeFile atomically writes data to path, creating parent directories as needed.
// Calls FADV_DONTNEED after writing to avoid polluting the OS page cache.
func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create parent dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("create block file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write block file: %w", err)
	}
	dropPageCache(f)
	return f.Close()
}

// readFile reads exactly size bytes from path.
// Calls FADV_DONTNEED after reading to avoid polluting the OS page cache.
func readFile(path string, size uint32) ([]byte, error) {
	data := make([]byte, size)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if _, err := io.ReadFull(f, data); err != nil {
		return nil, err
	}
	dropPageCache(f)
	return data, nil
}

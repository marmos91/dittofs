package fs

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/local"
)

// Compile-time interface satisfaction check.
var _ local.LocalStore = (*FSStore)(nil)

// FSStore is a two-tier (memory + disk) block cache for file data.
//
// NFS WRITE operations (typically 4KB) are buffered in 8MB in-memory blocks.
// When a block fills up or NFS COMMIT is called, the block is flushed atomically
// to a .blk file on disk. This design avoids per-4KB disk I/O and prevents OS
// page cache bloat that caused OOM on earlier versions.
//
// Block metadata (cache path, upload state, etc.) is tracked via FileBlockStore
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

	// pendingFBs queues FileBlock metadata updates for async persistence.
	// Keyed by blockID (string) -> *blockstore.FileBlock.
	// Drained every 200ms by SyncFileBlocks, and on Close/Flush.
	pendingFBs sync.Map

	// fdCache caches open file descriptors for .blk files to avoid
	// open+close syscalls on every 4KB random write in tryDirectDiskWrite.
	fdCache *fdCache

	// readFDCache caches open file descriptors (O_RDONLY) for .blk files
	// to avoid open+close syscalls on every 4KB random read in readFromDisk.
	readFDCache *fdCache

	// skipFsync skips fsync in Flush() for S3 backends where data durability
	// comes from S3 upload, not local disk. The cache .blk files are just
	// staging buffers -- losing them on power failure means re-downloading
	// from S3, not data loss. Saves ~0.5-2ms per COMMIT.
	skipFsync bool

	// evictionEnabled controls whether ensureSpace can evict blocks.
	// When false, ensureSpace returns ErrDiskFull if over limit instead of
	// evicting remote blocks. Used by local-only mode where there is no
	// remote store to re-fetch evicted blocks from.
	evictionEnabled atomic.Bool
}

// New creates a new FSStore.
//
// Parameters:
//   - baseDir: directory for .blk cache files, created if absent.
//   - maxDisk: maximum total size of on-disk .blk files in bytes. 0 = unlimited.
//   - maxMemory: memory budget for dirty write buffers in bytes. 0 defaults to 256MB.
//   - fileBlockStore: persistent store for FileBlock metadata (cache path, upload state, etc.)
func New(baseDir string, maxDisk int64, maxMemory int64, fileBlockStore blockstore.FileBlockStore) (*FSStore, error) {
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("cache: create base dir: %w", err)
	}

	if maxMemory <= 0 {
		maxMemory = 256 * 1024 * 1024 // 256MB default
	}

	bc := &FSStore{
		baseDir:     baseDir,
		maxDisk:     maxDisk,
		maxMemory:   maxMemory,
		blockStore:  fileBlockStore,
		memBlocks:   make(map[blockKey]*memBlock),
		fileBlocks:  make(map[string]map[uint64]*memBlock),
		files:       make(map[string]*fileInfo),
		fdCache:     newFDCache(defaultFDCacheSize),
		readFDCache: newFDCache(defaultFDCacheSize),
	}
	bc.evictionEnabled.Store(true)
	return bc, nil
}

// SetSkipFsync disables fsync in Flush() for S3 backends.
// Data durability comes from S3 upload, not local disk -- the cache .blk files
// are staging buffers, not the final store. Saves ~0.5-2ms per COMMIT.
func (bc *FSStore) SetSkipFsync(skip bool) {
	bc.skipFsync = skip
}

// Close flushes pending FileBlock metadata and marks the cache as closed.
// After Close, all read/write operations return ErrCacheClosed.
func (bc *FSStore) Close() error {
	bc.closedFlag.Store(true)
	bc.SyncFileBlocks(context.Background())
	bc.fdCache.CloseAll()
	bc.readFDCache.CloseAll()
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
// The goroutine stops when ctx is cancelled, with a final drain on exit.
func (bc *FSStore) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
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

// queueFileBlockUpdate queues a FileBlock metadata update for async persistence.
// The update will be written to the store by the next SyncFileBlocks call.
func (bc *FSStore) queueFileBlockUpdate(fb *blockstore.FileBlock) {
	bc.pendingFBs.Store(fb.ID, fb)
}

// lookupFileBlock retrieves a FileBlock, checking the pending queue first
// (for recently-written metadata not yet persisted) then falling back to the store.
func (bc *FSStore) lookupFileBlock(ctx context.Context, blockID string) (*blockstore.FileBlock, error) {
	if v, ok := bc.pendingFBs.Load(blockID); ok {
		return v.(*blockstore.FileBlock), nil
	}
	return bc.blockStore.GetFileBlock(ctx, blockID)
}

// Stats returns a snapshot of current cache statistics.
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

// GetFileSize returns the cached file size and whether the file is tracked.
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

// EvictMemory removes all cached data (memory and disk tracking) for a file.
// Does not delete .blk files from disk -- that is handled by eviction or
// explicit deletion via DeleteAllBlockFiles.
func (bc *FSStore) EvictMemory(_ context.Context, payloadID string) error {
	bc.purgeMemBlocks(payloadID, func(uint64) bool { return true })

	bc.filesMu.Lock()
	delete(bc.files, payloadID)
	bc.filesMu.Unlock()

	return nil
}

// Truncate discards cached blocks beyond newSize and updates the tracked file size.
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

// ListFiles returns the payloadIDs of all files currently tracked in the cache.
func (bc *FSStore) ListFiles() []string {
	bc.filesMu.RLock()
	defer bc.filesMu.RUnlock()
	result := make([]string, 0, len(bc.files))
	for payloadID := range bc.files {
		result = append(result, payloadID)
	}
	return result
}

// WriteFromRemote caches data that was fetched from the remote block store.
// Unlike WriteAt (which creates Dirty blocks), the block is marked Remote
// since it already exists remotely -- making it immediately evictable by the
// disk space manager without needing a re-sync.
func (bc *FSStore) WriteFromRemote(ctx context.Context, payloadID string, data []byte, offset uint64) error {
	blockIdx := offset / blockstore.BlockSize
	key := blockKey{payloadID: payloadID, blockIdx: blockIdx}
	blockID := makeBlockID(key)

	fb, err := bc.lookupFileBlock(ctx, blockID)
	if err != nil {
		fb = blockstore.NewFileBlock(blockID, "")
	}
	fb.BlockStoreKey = blockstore.FormatStoreKey(payloadID, blockIdx)
	fb.State = blockstore.BlockStateRemote

	path := bc.blockPath(blockID)
	if err := bc.ensureSpace(ctx, int64(len(data))); err != nil {
		return err
	}

	if err := writeFile(path, data); err != nil {
		return err
	}

	bc.diskUsed.Add(int64(len(data)))

	fb.CachePath = path
	fb.DataSize = uint32(len(data))
	fb.LastAccess = time.Now()
	bc.queueFileBlockUpdate(fb)

	end := offset + uint64(len(data))
	bc.updateFileSize(payloadID, end)

	return nil
}

// GetDirtyBlocks flushes all in-memory blocks for a file to disk, then returns
// all blocks in Local state (written to disk but not yet synced to remote).
// Used by the syncer to find blocks that need syncing.
//
// Optimization: uses the flushed block list from Flush() directly to read data,
// avoiding the expensive SyncFileBlocksForFile + ListLocalBlocks round-trip
// (write to BadgerDB then immediately read back). The pending FileBlock metadata
// is persisted asynchronously by the background ticker -- the syncer doesn't
// need it to be in BadgerDB since it gets data from cache files.
func (bc *FSStore) GetDirtyBlocks(ctx context.Context, payloadID string) ([]local.PendingBlock, error) {
	flushed, err := bc.Flush(ctx, payloadID)
	if err != nil {
		return nil, err
	}

	if len(flushed) == 0 {
		return nil, nil
	}

	// Read data from cache files for each freshly-flushed block.
	// No BadgerDB round-trip needed -- Flush() already told us which blocks
	// were written and where. The FileBlock metadata (with state=Local)
	// is queued in pendingFBs and will be persisted by the background ticker.
	var result []local.PendingBlock
	for _, fb := range flushed {
		data, err := readFile(fb.CachePath, fb.DataSize)
		if err != nil {
			continue
		}

		result = append(result, local.PendingBlock{
			BlockIndex: fb.BlockIndex,
			Data:       data,
			DataSize:   fb.DataSize,
		})
	}

	return result, nil
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
	if err != nil || fb.CachePath == "" || fb.DataSize == 0 {
		return nil, 0, ErrBlockNotFound
	}

	data, err := readFile(fb.CachePath, fb.DataSize)
	if err != nil {
		return nil, 0, err
	}

	return data, fb.DataSize, nil
}

// transitionBlockState atomically transitions a block's state in the FileBlockStore.
// If requireState > 0, the transition only succeeds when the block is in that state
// (CAS semantics for upload claim). Pass requireState = 0 for unconditional transition.
func (bc *FSStore) transitionBlockState(ctx context.Context, payloadID string, blockIdx uint64, requireState, targetState blockstore.BlockState) bool {
	key := blockKey{payloadID: payloadID, blockIdx: blockIdx}
	blockID := makeBlockID(key)
	fb, err := bc.lookupFileBlock(ctx, blockID)
	if err != nil {
		return false
	}
	if requireState != 0 && fb.State != requireState {
		return false
	}
	fb.State = targetState
	bc.queueFileBlockUpdate(fb)
	return true
}

// MarkBlockRemote marks a block as confirmed in the remote block store.
// Remote blocks are eligible for disk eviction since they can be re-fetched.
func (bc *FSStore) MarkBlockRemote(ctx context.Context, payloadID string, blockIdx uint64) bool {
	return bc.transitionBlockState(ctx, payloadID, blockIdx, 0, blockstore.BlockStateRemote)
}

// MarkBlockSyncing claims a block for sync to remote (Local -> Syncing).
// Only succeeds if the block is currently Local, preventing duplicate syncs.
func (bc *FSStore) MarkBlockSyncing(ctx context.Context, payloadID string, blockIdx uint64) bool {
	return bc.transitionBlockState(ctx, payloadID, blockIdx, blockstore.BlockStateLocal, blockstore.BlockStateSyncing)
}

// MarkBlockLocal reverts a block to Local state after a failed sync attempt,
// so the syncer will retry it on the next sync cycle.
func (bc *FSStore) MarkBlockLocal(ctx context.Context, payloadID string, blockIdx uint64) bool {
	return bc.transitionBlockState(ctx, payloadID, blockIdx, 0, blockstore.BlockStateLocal)
}

// IsBlockCached checks if a specific block is available in cache (memory or disk).
// Used by the syncer to decide whether to download a block before reading.
func (bc *FSStore) IsBlockCached(ctx context.Context, payloadID string, blockIdx uint64) bool {
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
	return err == nil && fb.CachePath != ""
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
		return fmt.Errorf("create cache file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write cache file: %w", err)
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

package cache

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// BlockCache is a two-tier (memory + disk) block cache for file data.
//
// NFS WRITE operations (typically 4KB) are buffered in 8MB in-memory blocks.
// When a block fills up or NFS COMMIT is called, the block is flushed atomically
// to a .blk file on disk. This design avoids per-4KB disk I/O and prevents OS
// page cache bloat that caused OOM on earlier versions.
//
// Block metadata (cache path, upload state, etc.) is tracked via FileBlockStore
// (BadgerDB) with async batching — writes are queued in pendingFBs and flushed
// every 200ms by the background goroutine started via Start().
//
// Thread safety: memBlocks uses sync.Map for lock-free concurrent access.
// Operations on different blocks are fully concurrent. Operations on the same
// block are serialized via memBlock.mu. The files map uses a separate filesMu
// to avoid contention with block operations.
type BlockCache struct {
	baseDir    string
	maxDisk    int64
	maxMemory  int64
	blockStore metadata.FileBlockStore

	// memBlocks stores blockKey → *memBlock using sync.Map for lock-free reads.
	// This eliminates the global RWMutex that previously serialized all block
	// lookups and flushes across all payloadIDs.
	memBlocks sync.Map

	// filesMu guards the files map separately from block operations.
	filesMu sync.RWMutex
	files   map[string]*fileInfo

	closedFlag atomic.Bool

	memUsed  atomic.Int64
	diskUsed atomic.Int64

	// pendingFBs queues FileBlock metadata updates for async persistence.
	// Keyed by blockID (string) → *metadata.FileBlock.
	// Drained every 200ms by SyncFileBlocks, and on Close/Flush.
	pendingFBs sync.Map
}

// New creates a new BlockCache.
//
// Parameters:
//   - baseDir: directory for .blk cache files, created if absent.
//   - maxDisk: maximum total size of on-disk .blk files in bytes. 0 = unlimited.
//   - maxMemory: memory budget for dirty write buffers in bytes. 0 defaults to 256MB.
//   - blockStore: persistent store for FileBlock metadata (cache path, upload state, etc.)
func New(baseDir string, maxDisk int64, maxMemory int64, blockStore metadata.FileBlockStore) (*BlockCache, error) {
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("cache: create base dir: %w", err)
	}

	if maxMemory <= 0 {
		maxMemory = 256 * 1024 * 1024 // 256MB default
	}

	return &BlockCache{
		baseDir:    baseDir,
		maxDisk:    maxDisk,
		maxMemory:  maxMemory,
		blockStore: blockStore,
		files:      make(map[string]*fileInfo),
	}, nil
}

// Close flushes pending FileBlock metadata and marks the cache as closed.
// After Close, all read/write operations return ErrCacheClosed.
func (bc *BlockCache) Close() error {
	bc.closedFlag.Store(true)
	bc.SyncFileBlocks(context.Background())
	return nil
}

func (bc *BlockCache) isClosed() bool {
	return bc.closedFlag.Load()
}

// Start launches the background goroutine that periodically persists queued
// FileBlock metadata updates to BadgerDB. This batches many small PutFileBlock
// calls (one per 4KB NFS write) into fewer store writes (every 200ms).
//
// Must be called after New and before any writes.
// The goroutine stops when ctx is cancelled, with a final drain on exit.
func (bc *BlockCache) Start(ctx context.Context) {
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
// to ensure the FileBlockStore is up-to-date for ListPendingUpload queries.
func (bc *BlockCache) SyncFileBlocks(ctx context.Context) {
	bc.pendingFBs.Range(func(key, value any) bool {
		fb := value.(*metadata.FileBlock)
		if err := bc.blockStore.PutFileBlock(ctx, fb); err == nil {
			bc.pendingFBs.Delete(key)
		}
		return true
	})
}

// queueFileBlockUpdate queues a FileBlock metadata update for async persistence.
// The update will be written to the store by the next SyncFileBlocks call.
func (bc *BlockCache) queueFileBlockUpdate(fb *metadata.FileBlock) {
	bc.pendingFBs.Store(fb.ID, fb)
}

// lookupFileBlock retrieves a FileBlock, checking the pending queue first
// (for recently-written metadata not yet persisted) then falling back to the store.
func (bc *BlockCache) lookupFileBlock(ctx context.Context, blockID string) (*metadata.FileBlock, error) {
	if v, ok := bc.pendingFBs.Load(blockID); ok {
		return v.(*metadata.FileBlock), nil
	}
	return bc.blockStore.GetFileBlock(ctx, blockID)
}

// Stats returns a snapshot of current cache statistics.
func (bc *BlockCache) Stats() Stats {
	bc.filesMu.RLock()
	fileCount := len(bc.files)
	bc.filesMu.RUnlock()

	var memBlockCount int
	bc.memBlocks.Range(func(_, _ any) bool {
		memBlockCount++
		return true
	})

	return Stats{
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
// Uses sync.Map.LoadOrStore for atomic load-or-create without external locking.
func (bc *BlockCache) getOrCreateMemBlock(key blockKey) *memBlock {
	if v, ok := bc.memBlocks.Load(key); ok {
		return v.(*memBlock)
	}

	mb := &memBlock{
		data: getBlockBuf(),
	}
	if actual, loaded := bc.memBlocks.LoadOrStore(key, mb); loaded {
		// Another goroutine created it first — return our buffer to the pool.
		putBlockBuf(mb.data)
		return actual.(*memBlock)
	}
	// We won the race — account for the new allocation.
	bc.memUsed.Add(int64(BlockSize))
	return mb
}

// getMemBlock returns the memBlock for the given key, or nil if not in memory.
func (bc *BlockCache) getMemBlock(key blockKey) *memBlock {
	if v, ok := bc.memBlocks.Load(key); ok {
		return v.(*memBlock)
	}
	return nil
}

// updateFileSize updates the tracked file size if the new end offset is larger.
// Uses double-checked locking: RLock fast path for existing files, Lock for creation.
func (bc *BlockCache) updateFileSize(payloadID string, end uint64) {
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
// This is a fast in-memory lookup — no disk or store access.
func (bc *BlockCache) GetFileSize(_ context.Context, payloadID string) (uint64, bool) {
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
// Format: "{payloadID}/{blockIdx}" — used as the primary key in FileBlockStore.
func makeBlockID(key blockKey) string {
	return fmt.Sprintf("%s/%d", key.payloadID, key.blockIdx)
}

// purgeMemBlocks removes all in-memory blocks for payloadID where shouldRemove returns true.
// Releases the 8MB buffer and decrements memUsed for each removed block.
func (bc *BlockCache) purgeMemBlocks(payloadID string, shouldRemove func(blockIdx uint64) bool) {
	bc.memBlocks.Range(func(k, v any) bool {
		key := k.(blockKey)
		if key.payloadID != payloadID || !shouldRemove(key.blockIdx) {
			return true
		}
		mb := v.(*memBlock)
		mb.mu.Lock()
		if mb.data != nil {
			bc.memUsed.Add(-int64(BlockSize))
			putBlockBuf(mb.data)
			mb.data = nil
		}
		mb.mu.Unlock()
		bc.memBlocks.Delete(key)
		return true
	})
}

// Remove removes all cached data (memory and disk tracking) for a file.
// Does not delete .blk files from disk — that is handled by eviction.
func (bc *BlockCache) Remove(_ context.Context, payloadID string) error {
	bc.purgeMemBlocks(payloadID, func(uint64) bool { return true })

	bc.filesMu.Lock()
	delete(bc.files, payloadID)
	bc.filesMu.Unlock()

	return nil
}

// Truncate discards cached blocks beyond newSize and updates the tracked file size.
// Blocks whose start offset (blockIdx * BlockSize) >= newSize are purged from memory.
func (bc *BlockCache) Truncate(_ context.Context, payloadID string, newSize uint64) error {
	bc.filesMu.RLock()
	fi, ok := bc.files[payloadID]
	bc.filesMu.RUnlock()

	if ok {
		fi.mu.Lock()
		fi.fileSize = newSize
		fi.mu.Unlock()
	}

	bc.purgeMemBlocks(payloadID, func(blockIdx uint64) bool {
		return blockIdx*BlockSize >= newSize
	})
	return nil
}

// ListFiles returns the payloadIDs of all files currently tracked in the cache.
func (bc *BlockCache) ListFiles() []string {
	bc.filesMu.RLock()
	defer bc.filesMu.RUnlock()
	result := make([]string, 0, len(bc.files))
	for payloadID := range bc.files {
		result = append(result, payloadID)
	}
	return result
}

// WriteDownloaded caches data that was downloaded from the block store (S3).
// Unlike WriteAt (which creates Dirty blocks), the block is marked Uploaded
// since it already exists remotely — making it immediately evictable by the
// disk space manager without needing a re-upload.
func (bc *BlockCache) WriteDownloaded(ctx context.Context, payloadID string, data []byte, offset uint64) error {
	blockIdx := offset / BlockSize
	key := blockKey{payloadID: payloadID, blockIdx: blockIdx}
	blockID := makeBlockID(key)

	fb, err := bc.lookupFileBlock(ctx, blockID)
	if err != nil {
		fb = metadata.NewFileBlock(blockID, "")
	}
	fb.BlockStoreKey = FormatStoreKey(payloadID, blockIdx)
	fb.State = metadata.BlockStateUploaded

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
// all blocks in Sealed state (written to disk but not yet uploaded to S3).
// Used by the offloader to find blocks that need uploading.
func (bc *BlockCache) GetDirtyBlocks(ctx context.Context, payloadID string) ([]PendingBlock, error) {
	if err := bc.Flush(ctx, payloadID); err != nil {
		return nil, err
	}

	// Persist queued metadata so ListPendingUpload sees all sealed blocks.
	bc.SyncFileBlocks(ctx)

	pending, err := bc.blockStore.ListPendingUpload(ctx, 0, 0)
	if err != nil {
		return nil, err
	}

	var result []PendingBlock
	for _, fb := range pending {
		if fb.State != metadata.BlockStateSealed || fb.CachePath == "" {
			continue
		}
		if !belongsToFile(fb.ID, payloadID) {
			continue
		}

		data, err := readFile(fb.CachePath, fb.DataSize)
		if err != nil {
			continue
		}

		var blockIdx uint64
		fmt.Sscanf(fb.ID[len(payloadID)+1:], "%d", &blockIdx)

		result = append(result, PendingBlock{
			BlockIndex: blockIdx,
			Data:       data,
			DataSize:   fb.DataSize,
			Hash:       fb.Hash,
		})
	}

	return result, nil
}

// GetBlockData returns the raw data for a specific block, checking memory first
// (for unflushed writes) then disk. Returns ErrBlockNotFound if the block is
// not in either tier.
func (bc *BlockCache) GetBlockData(ctx context.Context, payloadID string, blockIdx uint64) ([]byte, uint32, error) {
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
func (bc *BlockCache) transitionBlockState(ctx context.Context, payloadID string, blockIdx uint64, requireState, targetState metadata.BlockState) bool {
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

// MarkBlockUploaded marks a block as successfully uploaded to the block store (S3).
// Uploaded blocks are eligible for disk eviction since they can be re-downloaded.
func (bc *BlockCache) MarkBlockUploaded(ctx context.Context, payloadID string, blockIdx uint64) bool {
	return bc.transitionBlockState(ctx, payloadID, blockIdx, 0, metadata.BlockStateUploaded)
}

// MarkBlockUploading claims a block for upload (Sealed → Uploading).
// Only succeeds if the block is currently Sealed, preventing duplicate uploads.
func (bc *BlockCache) MarkBlockUploading(ctx context.Context, payloadID string, blockIdx uint64) bool {
	return bc.transitionBlockState(ctx, payloadID, blockIdx, metadata.BlockStateSealed, metadata.BlockStateUploading)
}

// MarkBlockPending reverts a block to Sealed state after a failed upload attempt,
// so the offloader will retry it on the next upload cycle.
func (bc *BlockCache) MarkBlockPending(ctx context.Context, payloadID string, blockIdx uint64) bool {
	return bc.transitionBlockState(ctx, payloadID, blockIdx, 0, metadata.BlockStateSealed)
}

// FormatStoreKey returns the block store key (S3 object key) for a block.
// Format: "{payloadID}/block-{blockIdx}".
func FormatStoreKey(payloadID string, blockIdx uint64) string {
	return fmt.Sprintf("%s/block-%d", payloadID, blockIdx)
}

// IsBlockCached checks if a specific block is available in cache (memory or disk).
// Used by the offloader to decide whether to download a block before reading.
func (bc *BlockCache) IsBlockCached(ctx context.Context, payloadID string, blockIdx uint64) bool {
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
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("create cache file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("write cache file: %w", err)
	}
	dropPageCache(f)
	f.Close()
	return nil
}

// readFile reads exactly size bytes from path.
// Calls FADV_DONTNEED after reading to avoid polluting the OS page cache.
func readFile(path string, size uint32) ([]byte, error) {
	data := make([]byte, size)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Read(data); err != nil {
		return nil, err
	}
	dropPageCache(f)
	return data, nil
}

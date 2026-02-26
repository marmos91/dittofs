package offloader

import (
	"context"
	"encoding/base64"
	"fmt"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/payload/block"
	"github.com/marmos91/dittofs/pkg/payload/chunk"
	"github.com/marmos91/dittofs/pkg/payload/store"
)

// hashB64 returns a base64-encoded representation of a hash for readable logging.
func hashB64(h [32]byte) string {
	return base64.StdEncoding.EncodeToString(h[:])
}

// defaultShutdownTimeout is the maximum time to wait for the transfer queue
// to finish processing during graceful shutdown.
const defaultShutdownTimeout = 30 * time.Second

// blockPool reuses 4MB buffers for block uploads to reduce GC pressure.
// Uses *[]byte to satisfy staticcheck SA6002 (sync.Pool prefers pointer types).
var blockPool = sync.Pool{
	New: func() any {
		buf := make([]byte, BlockSize)
		return &buf
	},
}

// fileUploadState tracks in-flight uploads for a single file.
type fileUploadState struct {
	inFlight sync.WaitGroup    // Tracks in-flight eager uploads
	flush    sync.WaitGroup    // Tracks in-flight flush operations
	errors   []error           // Accumulated errors
	errorsMu sync.Mutex        // Protects errors
	blocksMu sync.Mutex        // Protects uploadedBlocks and blockHashes
	uploaded map[blockKey]bool // Tracks which blocks have been uploaded

	// Block hashes for finalization (sorted by chunk/block index)
	blockHashes map[blockKey][32]byte
}

// blockKey uniquely identifies a block within a file.
type blockKey struct {
	chunkIdx uint32
	blockIdx uint32
}

// downloadResult is a broadcast-capable result for in-flight download deduplication.
// When the download completes, err is set and done is closed. Multiple waiters can
// safely read the result because closing a channel notifies ALL receivers.
type downloadResult struct {
	done chan struct{} // Closed when download completes
	err  error         // Result of the download (set before closing done)
	mu   sync.Mutex    // Protects err during write
}

// FinalizationCallback is called when all blocks for a file have been uploaded.
// It receives the payloadID and a list of block hashes for computing the final object hash.
type FinalizationCallback func(ctx context.Context, payloadID string, blockHashes [][32]byte)

// Offloader handles eager upload and parallel download for cache-to-block-store integration.
//
// Key features:
//   - Eager upload: Uploads complete 4MB blocks immediately in background goroutines
//   - Download priority: Downloads pause uploads to minimize read latency
//   - Prefetch: Speculatively fetches upcoming blocks for sequential reads
//   - Configurable parallelism: Set max concurrent uploads via config
//   - In-flight deduplication: Avoids duplicate downloads for the same block
//   - Content-addressed deduplication: Skip upload if block with same hash exists (optional)
//   - Non-blocking: All operations return immediately, I/O happens in background
//   - Finalization callback: Notifies when all blocks are uploaded for a file
type Offloader struct {
	cache       *cache.Cache
	blockStore  store.BlockStore
	objectStore metadata.ObjectStore // Required: enables content-addressed deduplication
	config      Config

	// Finalization callback - called when all blocks for a file are uploaded
	onFinalized FinalizationCallback

	// Per-file upload tracking
	uploads   map[string]*fileUploadState // payloadID -> state
	uploadsMu sync.Mutex

	// Global upload semaphore - limits total concurrent uploads
	uploadSem chan struct{}

	// Transfer queue for non-blocking operations
	queue *TransferQueue

	// Download priority: uploads pause when downloads are active
	ioCond           *sync.Cond // Condition variable for upload/download coordination
	downloadsPending int        // Count of active downloads (protected by ioCond.L)

	// In-flight download tracking: prevents duplicate downloads
	// Uses downloadResult with broadcast pattern - closing done channel notifies ALL waiters
	inFlight   map[string]*downloadResult // blockKey -> broadcast result
	inFlightMu sync.Mutex

	// Shutdown
	closed bool
	mu     sync.RWMutex
}

// New creates a new Offloader.
//
// Parameters:
//   - c: The cache to transfer from/to
//   - blockStore: The block store to transfer to
//   - objectStore: Required ObjectStore for content-addressed deduplication
//   - config: Offloader configuration
func New(c *cache.Cache, blockStore store.BlockStore, objectStore metadata.ObjectStore, config Config) *Offloader {
	if objectStore == nil {
		panic("objectStore is required for Offloader")
	}
	if config.ParallelUploads <= 0 {
		config.ParallelUploads = DefaultParallelUploads
	}
	if config.ParallelDownloads <= 0 {
		config.ParallelDownloads = DefaultParallelDownloads
	}

	// Calculate semaphore size - use MaxParallelUploads if set, otherwise ParallelUploads
	semSize := config.ParallelUploads
	if config.MaxParallelUploads > 0 {
		semSize = config.MaxParallelUploads
	}
	if semSize < 1 {
		semSize = DefaultParallelUploads
	}

	m := &Offloader{
		cache:       c,
		blockStore:  blockStore,
		objectStore: objectStore,
		config:      config,
		uploads:     make(map[string]*fileUploadState),
		ioCond:      sync.NewCond(&sync.Mutex{}),
		inFlight:    make(map[string]*downloadResult),
		uploadSem:   make(chan struct{}, semSize),
	}

	// Initialize transfer queue
	queueConfig := DefaultTransferQueueConfig()
	queueConfig.Workers = config.ParallelUploads
	m.queue = NewTransferQueue(m, queueConfig)

	return m
}

// SetFinalizationCallback sets the callback function that is invoked when
// all blocks for a file have been uploaded. The callback receives the payloadID
// and an ordered list of block hashes for computing the final object hash.
//
// This is used by the metadata layer to compute the Object/Chunk/Block hierarchy
// and update FileAttr.ObjectID after all uploads complete.
func (m *Offloader) SetFinalizationCallback(fn FinalizationCallback) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onFinalized = fn
}

// canProcess returns false if the offloader is closed or context is cancelled.
func (m *Offloader) canProcess(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return !m.closed
}

// blockRange returns the range of block indices that overlap with [offset, offset+length).
func blockRange(offset, length uint32) (start, end uint32) {
	start = offset / BlockSize
	end = (offset + length - 1) / BlockSize
	return
}

// ============================================================================
// Flush API (Returns FlushResult)
// ============================================================================

// Flush enqueues remaining dirty data for background upload and returns immediately.
//
// This method does NOT wait for S3 uploads to complete because:
// 1. Data is already safe in WAL-backed mmap cache (crash-safe via OS page cache)
// 2. Eager upload handles complete 4MB blocks asynchronously
// 3. Remaining partial blocks are enqueued for background upload
//
// Both NFS COMMIT and SMB CLOSE use this method. NFS/SMB semantics only require
// data to be durable on stable storage - the mmap WAL provides this guarantee.
//
// Deduplication: Blocks already uploaded by eager upload are tracked in state.uploaded
// and skipped by uploadRemainingBlocks. No need to wait for eager uploads to complete.
//
// Small file optimization: If SmallFileThreshold > 0 and the file is smaller than
// the threshold, the upload is done SYNCHRONOUSLY. This immediately frees the 4MB
// block buffer, preventing pendingSize buildup when creating many small files.
func (m *Offloader) Flush(ctx context.Context, payloadID string) (*FlushResult, error) {
	if !m.canProcess(ctx) {
		return nil, fmt.Errorf("offloader is closed")
	}

	// Get or create upload state for tracking
	state := m.getOrCreateUploadState(payloadID)

	// Check if this is a small file that should be flushed synchronously.
	// This prevents pendingSize buildup when creating many small files.
	fileSize := m.cache.GetFileSize(ctx, payloadID)
	isSmallFile := m.config.SmallFileThreshold > 0 &&
		int64(fileSize) <= m.config.SmallFileThreshold

	if isSmallFile {
		return m.flushSmallFileSync(ctx, payloadID, state)
	}

	// Large file: async flush (existing behavior)
	state.flush.Add(1)

	// Upload remaining dirty blocks (partial blocks not covered by eager upload)
	// in background. No blocking - data is safe in mmap cache.
	//
	// IMPORTANT: We use context.Background() here because the request context is
	// cancelled when COMMIT returns. The background upload should continue regardless.
	//
	// Server shutdown is handled separately by Offloader.Close() which:
	// 1. Stops accepting new work via canProcess() check
	// 2. Drains the transfer queue with a timeout
	// 3. uploadRemainingBlocks checks canProcess() before each block upload
	//
	// Data durability is guaranteed by the mmap WAL cache - uploads are best-effort
	// for performance, not required for durability.
	go func() {
		defer state.flush.Done()
		bgCtx := context.Background()

		if err := m.uploadRemainingBlocks(bgCtx, payloadID); err != nil {
			logger.Warn("Failed to upload remaining blocks",
				"payloadID", payloadID,
				"error", err)
		}

		// Wait for any in-flight eager uploads to complete
		state.inFlight.Wait()

		// Invoke finalization callback
		m.invokeFinalizationCallback(bgCtx, payloadID)
	}()

	return &FlushResult{Finalized: true}, nil
}

// flushSmallFileSync uploads a small file synchronously during Flush().
// This ensures the 4MB block buffer is freed immediately, preventing
// pendingSize buildup when creating many small files.
func (m *Offloader) flushSmallFileSync(ctx context.Context, payloadID string, state *fileUploadState) (*FlushResult, error) {
	logger.Debug("Small file sync flush",
		"payloadID", payloadID,
		"threshold", m.config.SmallFileThreshold)

	// Upload remaining blocks synchronously (blocks until complete)
	if err := m.uploadRemainingBlocks(ctx, payloadID); err != nil {
		return nil, fmt.Errorf("sync flush failed: %w", err)
	}

	// Wait for any in-flight eager uploads to complete
	state.inFlight.Wait()

	// Invoke finalization callback
	m.invokeFinalizationCallback(ctx, payloadID)

	return &FlushResult{Finalized: true}, nil
}

// WaitForEagerUploads waits for in-flight eager uploads to complete.
// This is useful in tests to ensure uploads complete before checking results.
func (m *Offloader) WaitForEagerUploads(ctx context.Context, payloadID string) error {
	state := m.getUploadState(payloadID)
	if state == nil {
		return nil
	}

	done := make(chan struct{})
	go func() {
		state.inFlight.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WaitForAllUploads waits for both eager uploads AND flush operations to complete.
// FOR TESTING ONLY - this method is used in integration tests to verify data was uploaded
// before checking block store contents. Production code should NOT call this method;
// production uses non-blocking Flush() which returns immediately (data safety is
// guaranteed by the WAL-backed mmap cache).
func (m *Offloader) WaitForAllUploads(ctx context.Context, payloadID string) error {
	state := m.getUploadState(payloadID)
	if state == nil {
		return nil
	}

	done := make(chan struct{})
	go func() {
		state.inFlight.Wait() // Wait for eager uploads
		state.flush.Wait()    // Wait for flush operations
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ============================================================================
// Block Store Queries
// ============================================================================

// GetFileSize returns the total size of a file from the block store.
// This is used as a fallback when the cache doesn't have the file.
func (m *Offloader) GetFileSize(ctx context.Context, payloadID string) (uint64, error) {
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return 0, fmt.Errorf("offloader is closed")
	}
	blockStore := m.blockStore
	m.mu.RUnlock()

	if blockStore == nil {
		return 0, fmt.Errorf("no block store configured")
	}

	// List all blocks to find the highest chunk/block indices
	prefix := payloadID + "/"
	blocks, err := blockStore.ListByPrefix(ctx, prefix)
	if err != nil {
		return 0, fmt.Errorf("list blocks: %w", err)
	}

	if len(blocks) == 0 {
		return 0, nil
	}

	// Find the last block (highest chunk/block indices)
	var maxChunkIdx, maxBlockIdx uint32
	for _, bk := range blocks {
		var chunkIdx, blockIdx uint32
		if _, err := fmt.Sscanf(bk, payloadID+"/chunk-%d/block-%d", &chunkIdx, &blockIdx); err != nil {
			continue
		}
		if chunkIdx > maxChunkIdx || (chunkIdx == maxChunkIdx && blockIdx > maxBlockIdx) {
			maxChunkIdx = chunkIdx
			maxBlockIdx = blockIdx
		}
	}

	// Only read the last block to get its size (may be partial)
	lastBlockKey := FormatBlockKey(payloadID, maxChunkIdx, maxBlockIdx)
	lastBlockData, err := blockStore.ReadBlock(ctx, lastBlockKey)
	lastBlockSize := uint64(BlockSize)
	if err == nil {
		lastBlockSize = uint64(len(lastBlockData))
	}

	// Total = full chunks + full blocks in last chunk + last block size
	totalSize := uint64(maxChunkIdx)*uint64(chunk.Size) +
		uint64(maxBlockIdx)*uint64(BlockSize) +
		lastBlockSize

	return totalSize, nil
}

// Exists checks if any blocks exist for a file in the block store.
func (m *Offloader) Exists(ctx context.Context, payloadID string) (bool, error) {
	if !m.canProcess(ctx) {
		return false, fmt.Errorf("offloader is closed")
	}

	if m.blockStore == nil {
		return false, fmt.Errorf("no block store configured")
	}

	// Check if the first block exists (fast path)
	firstBlockKey := FormatBlockKey(payloadID, 0, 0)
	_, err := m.blockStore.ReadBlock(ctx, firstBlockKey)
	if err == nil {
		return true, nil
	}
	if err == store.ErrBlockNotFound {
		return false, nil
	}
	return false, fmt.Errorf("check block: %w", err)
}

// Truncate removes blocks beyond the new size from the block store.
// Note: This deletes whole blocks only. Partial block truncation (e.g., truncating
// to middle of a block) is not supported - the last block retains its original size.
// Future optimization: Add TruncateBlock to BlockStore interface using S3 CopyObjectWithRange.
func (m *Offloader) Truncate(ctx context.Context, payloadID string, newSize uint64) error {
	if !m.canProcess(ctx) {
		return fmt.Errorf("offloader is closed")
	}

	if m.blockStore == nil {
		return fmt.Errorf("no block store configured")
	}

	// Calculate which chunk/block the new size falls into
	newChunkIdx := chunk.IndexForOffset(newSize)
	offsetInChunk := chunk.OffsetInChunk(newSize)
	newBlockIdx := block.IndexForOffset(offsetInChunk)

	// List and delete blocks beyond the new size
	prefix := payloadID + "/"
	blocks, err := m.blockStore.ListByPrefix(ctx, prefix)
	if err != nil {
		return fmt.Errorf("list blocks: %w", err)
	}

	for _, bk := range blocks {
		var chunkIdx, blockIdx uint32
		if _, err := fmt.Sscanf(bk, payloadID+"/chunk-%d/block-%d", &chunkIdx, &blockIdx); err != nil {
			continue
		}
		if chunkIdx > newChunkIdx || (chunkIdx == newChunkIdx && blockIdx > newBlockIdx) {
			if err := m.blockStore.DeleteBlock(ctx, bk); err != nil {
				return fmt.Errorf("delete block %s: %w", bk, err)
			}
		}
	}

	return nil
}

// Delete removes all blocks for a file from the block store.
// Use this for unfinalized files (no ObjectID).
func (m *Offloader) Delete(ctx context.Context, payloadID string) error {
	if !m.canProcess(ctx) {
		return fmt.Errorf("offloader is closed")
	}

	if m.blockStore == nil {
		return fmt.Errorf("no block store configured")
	}

	// Clean up upload state for this file
	m.uploadsMu.Lock()
	delete(m.uploads, payloadID)
	m.uploadsMu.Unlock()

	return m.blockStore.DeleteByPrefix(ctx, payloadID+"/")
}

// ============================================================================
// Lifecycle
// ============================================================================

// Start begins background upload processing.
// Must be called after New() to enable async uploads.
func (m *Offloader) Start(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.queue != nil {
		m.queue.Start(ctx)
	}
}

// Close shuts down the offloader and waits for pending uploads.
func (m *Offloader) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.mu.Unlock()

	// Stop transfer queue with graceful shutdown timeout
	if m.queue != nil {
		m.queue.Stop(defaultShutdownTimeout)
	}

	return nil
}

// HealthCheck verifies the block store is accessible.
func (m *Offloader) HealthCheck(ctx context.Context) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.closed {
		return fmt.Errorf("offloader is closed")
	}

	if m.blockStore == nil {
		return fmt.Errorf("no block store configured")
	}

	return m.blockStore.HealthCheck(ctx)
}


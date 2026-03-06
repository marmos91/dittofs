package offloader

import (
	"context"
	"encoding/base64"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/payload/store"
)

// hashB64 returns a base64-encoded representation of a hash for readable logging.
func hashB64(h [32]byte) string {
	return base64.StdEncoding.EncodeToString(h[:])
}

// defaultShutdownTimeout is the maximum time to wait for the transfer queue
// to finish processing during graceful shutdown.
const defaultShutdownTimeout = 30 * time.Second

// blockKey uniquely identifies a block within a file (flat index).
type blockKey = uint64

// fileUploadState tracks in-flight uploads for a single file.
type fileUploadState struct {
	inFlight sync.WaitGroup    // Tracks in-flight eager uploads
	flush    sync.WaitGroup    // Tracks in-flight flush operations
	errors   []error           // Accumulated errors
	errorsMu sync.Mutex        // Protects errors
	blocksMu sync.Mutex        // Protects uploadedBlocks and blockHashes
	uploaded map[blockKey]bool  // Tracks which blocks have been uploaded

	// Block hashes for finalization (sorted by block index)
	blockHashes map[blockKey][32]byte
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

// Offloader handles async cache-to-block-store transfers with eager upload,
// parallel download, prefetch, in-flight dedup, and content-addressed dedup.
type Offloader struct {
	cache       *cache.BlockCache
	blockStore  store.BlockStore
	fileBlockStore metadata.FileBlockStore // Required: enables content-addressed deduplication
	config      Config

	// Finalization callback - called when all blocks for a file are uploaded
	onFinalized FinalizationCallback

	uploads   map[string]*fileUploadState // payloadID -> per-file upload tracking
	uploadsMu sync.Mutex

	uploadSem chan struct{} // Limits total concurrent uploads

	queue *TransferQueue // Transfer queue for non-blocking operations

	ioCond           *sync.Cond // Upload/download coordination (uploads yield to downloads)
	downloadsPending int        // Active downloads (protected by ioCond.L)

	inFlight   map[string]*downloadResult // In-flight download dedup (blockKey -> broadcast)
	inFlightMu sync.Mutex

	stopCh chan struct{} // Signals periodic uploader to stop
	closed bool
	mu     sync.RWMutex

	uploading atomic.Bool // Guards against overlapping periodic upload ticks
}

// New creates a new Offloader. The fileBlockStore is required for content-addressed dedup.
func New(c *cache.BlockCache, blockStore store.BlockStore, fileBlockStore metadata.FileBlockStore, config Config) *Offloader {
	if fileBlockStore == nil {
		panic("fileBlockStore is required for Offloader")
	}
	if config.ParallelUploads <= 0 {
		config.ParallelUploads = DefaultParallelUploads
	}
	if config.ParallelDownloads <= 0 {
		config.ParallelDownloads = DefaultParallelDownloads
	}

	semSize := config.ParallelUploads
	if config.MaxParallelUploads > 0 {
		semSize = config.MaxParallelUploads
	}

	m := &Offloader{
		cache:       c,
		blockStore:  blockStore,
		fileBlockStore: fileBlockStore,
		config:      config,
		uploads:     make(map[string]*fileUploadState),
		ioCond:      sync.NewCond(&sync.Mutex{}),
		inFlight:    make(map[string]*downloadResult),
		uploadSem:   make(chan struct{}, semSize),
		stopCh:      make(chan struct{}),
	}

	queueConfig := DefaultTransferQueueConfig()
	queueConfig.Workers = config.ParallelUploads
	queueConfig.DownloadWorkers = config.ParallelDownloads
	m.queue = NewTransferQueue(m, queueConfig)

	return m
}

// SetFinalizationCallback sets the callback invoked when all blocks for a file are uploaded.
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

// flushAndFinalize uploads remaining blocks, waits for in-flight uploads,
// and invokes the finalization callback. Shared by sync and async flush paths.
func (m *Offloader) flushAndFinalize(ctx context.Context, payloadID string, state *fileUploadState) error {
	if err := m.uploadRemainingBlocks(ctx, payloadID); err != nil {
		return err
	}
	state.inFlight.Wait()
	m.invokeFinalizationCallback(ctx, payloadID)
	return nil
}

// Flush enqueues remaining dirty data for background upload and returns immediately.
// Data is safe in the on-disk cache; S3 uploads happen asynchronously.
// Small files (below SmallFileThreshold) are uploaded synchronously to free buffers.
func (m *Offloader) Flush(ctx context.Context, payloadID string) (*FlushResult, error) {
	if !m.canProcess(ctx) {
		return nil, fmt.Errorf("offloader is closed")
	}

	state := m.getOrCreateUploadState(payloadID)

	fileSize, _ := m.cache.GetFileSize(ctx, payloadID)
	if m.config.SmallFileThreshold > 0 && int64(fileSize) <= m.config.SmallFileThreshold {
		logger.Debug("Small file sync flush",
			"payloadID", payloadID, "threshold", m.config.SmallFileThreshold)
		if err := m.flushAndFinalize(ctx, payloadID, state); err != nil {
			return nil, fmt.Errorf("sync flush failed: %w", err)
		}
		return &FlushResult{Finalized: true}, nil
	}

	// Background upload: use context.Background() because the request context
	// is cancelled when COMMIT returns, but uploads should continue.
	state.flush.Go(func() {
		bgCtx := context.Background()
		if err := m.flushAndFinalize(bgCtx, payloadID, state); err != nil {
			logger.Warn("Failed to upload remaining blocks",
				"payloadID", payloadID, "error", err)
		}
	})

	return &FlushResult{Finalized: true}, nil
}

// WaitForEagerUploads waits for in-flight eager uploads to complete (for testing).
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

// WaitForAllUploads waits for both eager uploads and flush operations to complete.
// FOR TESTING ONLY -- production code should use non-blocking Flush().
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

// GetFileSize returns the total size of a file from the block store.
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

	prefix := payloadID + "/"
	blocks, err := blockStore.ListByPrefix(ctx, prefix)
	if err != nil {
		return 0, fmt.Errorf("list blocks: %w", err)
	}
	if len(blocks) == 0 {
		return 0, nil
	}

	var maxBlockIdx uint64
	for _, bk := range blocks {
		var blockIdx uint64
		if _, err := fmt.Sscanf(bk, payloadID+"/block-%d", &blockIdx); err != nil {
			continue
		}
		if blockIdx > maxBlockIdx {
			maxBlockIdx = blockIdx
		}
	}

	lastBlockKey := cache.FormatStoreKey(payloadID, maxBlockIdx)
	lastBlockData, err := blockStore.ReadBlock(ctx, lastBlockKey)
	if err != nil {
		return 0, fmt.Errorf("read last block %s: %w", lastBlockKey, err)
	}

	return maxBlockIdx*uint64(BlockSize) + uint64(len(lastBlockData)), nil
}

// Exists checks if any blocks exist for a file in the block store.
func (m *Offloader) Exists(ctx context.Context, payloadID string) (bool, error) {
	if !m.canProcess(ctx) {
		return false, fmt.Errorf("offloader is closed")
	}
	if m.blockStore == nil {
		return false, fmt.Errorf("no block store configured")
	}

	firstBlockKey := cache.FormatStoreKey(payloadID, 0)
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
func (m *Offloader) Truncate(ctx context.Context, payloadID string, newSize uint64) error {
	if !m.canProcess(ctx) {
		return fmt.Errorf("offloader is closed")
	}
	if m.blockStore == nil {
		return fmt.Errorf("no block store configured")
	}

	prefix := payloadID + "/"
	if newSize == 0 {
		return m.blockStore.DeleteByPrefix(ctx, prefix)
	}

	keepBlockIdx := (newSize - 1) / uint64(BlockSize)

	blocks, err := m.blockStore.ListByPrefix(ctx, prefix)
	if err != nil {
		return fmt.Errorf("list blocks: %w", err)
	}

	for _, bk := range blocks {
		var blockIdx uint64
		if _, err := fmt.Sscanf(bk, payloadID+"/block-%d", &blockIdx); err != nil {
			continue
		}
		if blockIdx > keepBlockIdx {
			if err := m.blockStore.DeleteBlock(ctx, bk); err != nil {
				return fmt.Errorf("delete block %s: %w", bk, err)
			}
		}
	}

	return nil
}

// Delete removes all blocks for a file from the block store.
func (m *Offloader) Delete(ctx context.Context, payloadID string) error {
	if !m.canProcess(ctx) {
		return fmt.Errorf("offloader is closed")
	}
	if m.blockStore == nil {
		return fmt.Errorf("no block store configured")
	}

	m.uploadsMu.Lock()
	delete(m.uploads, payloadID)
	m.uploadsMu.Unlock()

	return m.blockStore.DeleteByPrefix(ctx, payloadID+"/")
}

// Start begins background upload processing and periodic uploader.
// Must be called after New() to enable async uploads.
func (m *Offloader) Start(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.queue != nil {
		m.queue.Start(ctx)
	}

	interval := m.config.UploadInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	go m.periodicUploader(ctx, interval)
}

// periodicUploader runs every interval, scanning for blocks to upload.
// Uses an atomic guard to prevent overlapping ticks: if the previous upload
// batch is still running when the ticker fires, the tick is skipped. This
// prevents unbounded memory growth when uploads take longer than the interval
// (e.g., 8 blocks x 2-3s S3 upload = 16-24s, but interval is only 2s).
func (m *Offloader) periodicUploader(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	logger.Info("Periodic uploader started", "interval", interval, "upload_delay", m.config.UploadDelay)

	for {
		select {
		case <-ticker.C:
			if !m.canProcess(ctx) {
				logger.Info("Periodic uploader: canProcess=false, exiting")
				return
			}
			// Skip this tick if the previous upload batch is still running.
			// This prevents overlapping ticks from multiplying memory usage.
			if !m.uploading.CompareAndSwap(false, true) {
				logger.Debug("Periodic uploader: previous tick still running, skipping")
				continue
			}
			m.uploadPendingBlocks(ctx)
			m.uploading.Store(false)
		case <-m.stopCh:
			logger.Info("Periodic uploader: stopCh received, exiting")
			return
		case <-ctx.Done():
			logger.Info("Periodic uploader: context cancelled, exiting")
			return
		}
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

	close(m.stopCh)
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

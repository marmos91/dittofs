package sync

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
)

// parseStoreKeyBlockIdx extracts the block index from a store key for a known payloadID.
// Delegates to blockstore.ParseStoreKey, verifying the key belongs to the expected file.
func parseStoreKeyBlockIdx(storeKey, payloadID string) (uint64, bool) {
	pid, blockIdx, ok := blockstore.ParseStoreKey(storeKey)
	if !ok || pid != payloadID {
		return 0, false
	}
	return blockIdx, true
}

// waitWithContext runs fn in a goroutine and waits for it to finish or the
// context to be cancelled. Returns nil on completion, or ctx.Err() on timeout.
func waitWithContext(ctx context.Context, fn func()) error {
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// defaultShutdownTimeout is the maximum time to wait for the transfer queue
// to finish processing during graceful shutdown.
const defaultShutdownTimeout = 30 * time.Second

// fileSyncState tracks in-flight uploads for a single file.
type fileSyncState struct {
	inFlight gosync.WaitGroup // Tracks in-flight eager uploads
	flush    gosync.WaitGroup // Tracks in-flight flush operations
}

// fetchResult is a broadcast-capable result for in-flight download deduplication.
// When the download completes, err is set and done is closed. Multiple waiters can
// safely read the result because closing a channel notifies ALL receivers.
type fetchResult struct {
	done chan struct{} // Closed when download completes
	err  error         // Result of the download (set before closing done)
	mu   gosync.Mutex  // Protects err during write
}

// FinalizationCallback is called when all blocks for a file have been uploaded.
// It receives the payloadID and a list of block hashes for computing the final object hash.
type FinalizationCallback func(ctx context.Context, payloadID string, blockHashes [][32]byte)

// Syncer handles async cache-to-block-store transfers with eager upload,
// parallel download, prefetch, in-flight dedup, and content-addressed dedup.
type Syncer struct {
	cache          local.LocalStore
	remoteStore    remote.RemoteStore
	fileBlockStore blockstore.FileBlockStore // Required: enables content-addressed deduplication
	config         Config

	// Finalization callback - called when all blocks for a file are uploaded
	onFinalized FinalizationCallback

	uploads   map[string]*fileSyncState // payloadID -> per-file upload tracking
	uploadsMu gosync.Mutex

	queue *SyncQueue // Transfer queue for non-blocking operations

	inFlight   map[string]*fetchResult // In-flight download dedup (store key -> broadcast)
	inFlightMu gosync.Mutex

	stopCh chan struct{} // Signals periodic uploader to stop
	closed bool
	mu     gosync.RWMutex

	periodicStarted bool        // true once periodicUploader goroutine is launched
	uploading       atomic.Bool // Guards against overlapping periodic upload ticks
}

// New creates a new Syncer. The fileBlockStore is required for content-addressed dedup.
func New(c local.LocalStore, remoteStore remote.RemoteStore, fileBlockStore blockstore.FileBlockStore, config Config) *Syncer {
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

	m := &Syncer{
		cache:          c,
		remoteStore:    remoteStore,
		fileBlockStore: fileBlockStore,
		config:         config,
		uploads:        make(map[string]*fileSyncState),
		inFlight:       make(map[string]*fetchResult),
		stopCh:         make(chan struct{}),
	}

	queueConfig := DefaultSyncQueueConfig()
	queueConfig.Workers = config.ParallelUploads
	queueConfig.DownloadWorkers = config.ParallelDownloads
	m.queue = NewSyncQueue(m, queueConfig)

	return m
}

// SetFinalizationCallback sets the callback invoked when all blocks for a file are uploaded.
func (m *Syncer) SetFinalizationCallback(fn FinalizationCallback) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onFinalized = fn
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

// Flush writes dirty in-memory blocks to disk cache (.blk files).
// Remote uploads happen asynchronously via the periodic uploader, so this
// returns without waiting for remote sync. This decouples NFS COMMIT latency
// from remote upload latency -- with a sufficiently large cache, remote write
// performance equals local performance. Remote latency only matters when
// backpressure kicks in (cache full) or on cold reads.
func (m *Syncer) Flush(ctx context.Context, payloadID string) (*blockstore.FlushResult, error) {
	if err := m.checkReady(ctx); err != nil {
		return nil, err
	}

	// Flush memBlocks to disk cache only.
	// The data is safe on disk after this call. Remote uploads happen asynchronously
	// via the periodic syncer -- Local blocks are picked up after UploadDelay
	// (resettable on new writes), allowing sparse blocks to accumulate more data
	// before sync.
	if _, err := m.cache.Flush(ctx, payloadID); err != nil {
		return nil, fmt.Errorf("cache flush failed: %w", err)
	}

	return &blockstore.FlushResult{Finalized: false}, nil
}

// DrainAllUploads waits for all in-flight uploads across all files to complete.
// This includes both eager uploads (inFlight) and flush operations (flush) for
// every tracked file.
//
// Useful for benchmarking and testing to ensure clean boundaries between workloads,
// and exposed via the REST API for the benchmark runner to call between test phases.
//
// Returns nil when all uploads complete, or ctx.Err() if the context is cancelled.
func (m *Syncer) DrainAllUploads(ctx context.Context) error {
	m.uploadsMu.Lock()
	states := make([]*fileSyncState, 0, len(m.uploads))
	for _, state := range m.uploads {
		states = append(states, state)
	}
	m.uploadsMu.Unlock()

	return waitWithContext(ctx, func() {
		for _, state := range states {
			state.inFlight.Wait()
			state.flush.Wait()
		}
	})
}

// WaitForEagerUploads waits for in-flight eager uploads to complete (for testing).
func (m *Syncer) WaitForEagerUploads(ctx context.Context, payloadID string) error {
	state := m.getSyncState(payloadID)
	if state == nil {
		return nil
	}
	return waitWithContext(ctx, state.inFlight.Wait)
}

// WaitForAllUploads waits for both eager uploads and flush operations to complete.
// FOR TESTING ONLY -- production code should use non-blocking Flush().
func (m *Syncer) WaitForAllUploads(ctx context.Context, payloadID string) error {
	state := m.getSyncState(payloadID)
	if state == nil {
		return nil
	}
	return waitWithContext(ctx, func() {
		state.inFlight.Wait()
		state.flush.Wait()
	})
}

// GetFileSize returns the total size of a file from the remote store.
func (m *Syncer) GetFileSize(ctx context.Context, payloadID string) (uint64, error) {
	if err := m.checkReady(ctx); err != nil {
		return 0, err
	}

	if m.remoteStore == nil {
		logger.Debug("syncer: skipping GetFileSize, no remote store")
		return 0, nil
	}

	prefix := payloadID + "/"
	blocks, err := m.remoteStore.ListByPrefix(ctx, prefix)
	if err != nil {
		return 0, fmt.Errorf("list blocks: %w", err)
	}
	if len(blocks) == 0 {
		return 0, nil
	}

	var maxBlockIdx uint64
	for _, bk := range blocks {
		blockIdx, ok := parseStoreKeyBlockIdx(bk, payloadID)
		if !ok {
			continue
		}
		if blockIdx > maxBlockIdx {
			maxBlockIdx = blockIdx
		}
	}

	lastBlockKey := blockstore.FormatStoreKey(payloadID, maxBlockIdx)
	lastBlockData, err := m.remoteStore.ReadBlock(ctx, lastBlockKey)
	if err != nil {
		return 0, fmt.Errorf("read last block %s: %w", lastBlockKey, err)
	}

	return maxBlockIdx*uint64(BlockSize) + uint64(len(lastBlockData)), nil
}

// Exists checks if any blocks exist for a file in the remote store.
func (m *Syncer) Exists(ctx context.Context, payloadID string) (bool, error) {
	if err := m.checkReady(ctx); err != nil {
		return false, err
	}
	if m.remoteStore == nil {
		logger.Debug("syncer: skipping Exists, no remote store")
		return false, nil
	}

	firstBlockKey := blockstore.FormatStoreKey(payloadID, 0)
	_, err := m.remoteStore.ReadBlock(ctx, firstBlockKey)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, blockstore.ErrBlockNotFound) {
		return false, nil
	}
	return false, fmt.Errorf("check block: %w", err)
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
		blockIdx, ok := parseStoreKeyBlockIdx(bk, payloadID)
		if !ok {
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

	// Always clean up upload tracking even with nil remoteStore.
	m.uploadsMu.Lock()
	delete(m.uploads, payloadID)
	m.uploadsMu.Unlock()

	if m.remoteStore == nil {
		logger.Debug("syncer: skipping Delete, no remote store")
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

	if m.periodicStarted {
		return // Already started (e.g., via SetRemoteStore before Start)
	}
	m.periodicStarted = true

	interval := m.config.UploadInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	go m.periodicUploader(ctx, interval)
}

// SyncNow triggers an immediate upload cycle for all local blocks,
// bypassing the UploadDelay. Blocks until all eligible blocks are uploaded.
// Intended for testing -- production code uses the periodic uploader.
func (m *Syncer) SyncNow(ctx context.Context) {
	// Flush queued FileBlock metadata to the store so ListLocalBlocks can find them.
	m.cache.SyncFileBlocks(ctx)
	pending, err := m.fileBlockStore.ListLocalBlocks(ctx, 0, 0)
	if err != nil {
		return
	}
	for _, fb := range pending {
		if fb.CachePath == "" {
			continue
		}
		m.syncFileBlock(ctx, fb)
	}
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
			m.syncLocalBlocks(ctx)
			m.uploading.Store(false)
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
// It sets the remoteStore, enables cache eviction, and starts the periodic syncer.
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
	m.cache.SetEvictionEnabled(true)

	if !m.periodicStarted {
		m.periodicStarted = true
		interval := m.config.UploadInterval
		if interval <= 0 {
			interval = 2 * time.Second
		}
		go m.periodicUploader(ctx, interval)
	}

	logger.Info("Remote store attached, periodic syncer started")
	return nil
}

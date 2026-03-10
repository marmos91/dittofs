package readcache

import (
	"context"
	"sync"
	"sync/atomic"
)

const (
	// seqThreshold is the number of consecutive sequential reads before prefetch triggers.
	seqThreshold = 2

	// maxPrefetchDepth is the maximum number of blocks to prefetch ahead (8 * 8MB = 64MB).
	maxPrefetchDepth = 8

	// defaultPrefetchWorkers is the default worker goroutine count for the prefetch pool.
	defaultPrefetchWorkers = 4
)

// LoadBlockFn loads a block from the underlying store (local disk or remote).
// It is dependency-injected to avoid import cycles with the engine package.
type LoadBlockFn func(ctx context.Context, payloadID string, blockIdx uint64) ([]byte, uint32, error)

// LocalChecker checks if a block is available on local disk, allowing
// the prefetcher to skip blocks that don't need to be fetched.
type LocalChecker interface {
	IsBlockCached(ctx context.Context, payloadID string, blockIdx uint64) bool
}

// seqTracker tracks sequential read patterns for a single payloadID.
type seqTracker struct {
	lastBlockIdx uint64 // last block index read
	seqCount     int    // consecutive sequential reads
	depth        int    // current prefetch depth (doubles on each trigger)
}

// prefetchReq is a work item for the prefetch worker pool.
type prefetchReq struct {
	payloadID string
	blockIdx  uint64
}

// Prefetcher detects sequential access patterns per file and pre-loads
// upcoming blocks into the ReadCache using a bounded worker pool.
//
// Sequential detection: After seqThreshold (2) consecutive sequential block
// reads, the prefetcher submits prefetch requests for upcoming blocks.
//
// Adaptive depth: Starts at 1 block ahead, doubles on each sequential trigger
// up to maxPrefetchDepth (8). Resets to 1 on non-sequential read.
//
// Bounded pool: Fixed goroutine count consuming from a buffered channel.
// Non-blocking submit drops requests when the channel is full (natural backpressure).
type Prefetcher struct {
	mu       sync.Mutex
	trackers map[string]*seqTracker // payloadID -> sequential tracker

	reqCh  chan prefetchReq // bounded channel for worker pool
	cache  *ReadCache       // L1 cache to fill with prefetched data
	loadFn LoadBlockFn      // injected block loader
	local  LocalChecker     // check local disk cache (can be nil)
	cancel context.CancelFunc
	wg     sync.WaitGroup
	closed atomic.Bool
}

// NewPrefetcher creates a prefetcher with the specified worker count.
// It starts worker goroutines that consume from a bounded channel.
// Returns nil if cache is nil or loadFn is nil (can't prefetch without these).
// If workers <= 0, defaults to defaultPrefetchWorkers.
func NewPrefetcher(workers int, cache *ReadCache, loadFn LoadBlockFn, local LocalChecker) *Prefetcher {
	if cache == nil || loadFn == nil {
		return nil
	}
	if workers <= 0 {
		workers = defaultPrefetchWorkers
	}

	ctx, cancel := context.WithCancel(context.Background())

	p := &Prefetcher{
		trackers: make(map[string]*seqTracker),
		reqCh:    make(chan prefetchReq, workers*4),
		cache:    cache,
		loadFn:   loadFn,
		local:    local,
		cancel:   cancel,
	}

	// Start worker goroutines.
	p.wg.Add(workers)
	for i := 0; i < workers; i++ {
		go p.worker(ctx)
	}

	return p
}

// OnRead tracks a read access and triggers prefetch if sequential pattern detected.
//
// Sequential detection logic:
//  1. No tracker for payloadID: create new tracker, no prefetch.
//  2. blockIdx == lastBlockIdx+1: increment seqCount.
//     If seqCount >= seqThreshold: submit prefetch for depth blocks ahead,
//     then double depth (capped at maxPrefetchDepth).
//  3. Otherwise (non-sequential): reset tracker.
func (p *Prefetcher) OnRead(payloadID string, blockIdx uint64) {
	if p == nil || p.closed.Load() {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	tr, exists := p.trackers[payloadID]
	if !exists {
		// First read for this file -- create tracker, no prefetch.
		p.trackers[payloadID] = &seqTracker{
			lastBlockIdx: blockIdx,
			seqCount:     0,
			depth:        1,
		}
		return
	}

	if blockIdx == tr.lastBlockIdx+1 {
		// Sequential read.
		tr.seqCount++
		tr.lastBlockIdx = blockIdx

		if tr.seqCount >= seqThreshold {
			// Submit prefetch requests for depth blocks ahead.
			for d := 1; d <= tr.depth; d++ {
				target := blockIdx + uint64(d)
				// Skip blocks already in L1 cache.
				if p.cache.Contains(payloadID, target) {
					continue
				}
				p.submit(payloadID, target)
			}
			// Double depth for next trigger (capped at maxPrefetchDepth).
			tr.depth *= 2
			if tr.depth > maxPrefetchDepth {
				tr.depth = maxPrefetchDepth
			}
		}
	} else {
		// Non-sequential read -- reset tracker.
		tr.lastBlockIdx = blockIdx
		tr.seqCount = 0
		tr.depth = 1
	}
}

// Reset removes the sequential tracker for a payloadID.
// Called on write, truncate, or delete to reset prefetch state.
func (p *Prefetcher) Reset(payloadID string) {
	if p == nil {
		return
	}

	p.mu.Lock()
	delete(p.trackers, payloadID)
	p.mu.Unlock()
}

// Close stops all workers and waits for them to finish. Idempotent.
// Workers are stopped via context cancellation (not channel close) to avoid
// a race where submit() sends on a closed channel.
func (p *Prefetcher) Close() {
	if p == nil {
		return
	}

	if !p.closed.CompareAndSwap(false, true) {
		return // already closed
	}

	p.cancel()
	p.wg.Wait()
}

// submit sends a prefetch request to the worker pool.
// Non-blocking: if the channel is full, the request is silently dropped.
func (p *Prefetcher) submit(payloadID string, blockIdx uint64) {
	if p.closed.Load() {
		return
	}
	req := prefetchReq{payloadID: payloadID, blockIdx: blockIdx}
	select {
	case p.reqCh <- req:
	default:
		// Channel full -- drop request (natural backpressure).
	}
}

// worker processes prefetch requests from the channel.
// Uses select on ctx.Done() to stop (not channel close) to avoid races with submit().
func (p *Prefetcher) worker(ctx context.Context) {
	defer p.wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		case req := <-p.reqCh:
			// Check cancellation immediately after dequeue to avoid
			// slow I/O when Close() has already been called.
			if ctx.Err() != nil {
				return
			}

			// Skip if already in L1 cache.
			if p.cache.Contains(req.payloadID, req.blockIdx) {
				continue
			}

			// Skip if block is on local disk.
			if p.local != nil && p.local.IsBlockCached(ctx, req.payloadID, req.blockIdx) {
				continue
			}

			// Load block from underlying store.
			data, dataSize, err := p.loadFn(ctx, req.payloadID, req.blockIdx)
			if err != nil {
				// Prefetch failures are non-fatal -- just skip.
				continue
			}

			// Fill L1 cache with the prefetched data.
			p.cache.Put(req.payloadID, req.blockIdx, data, dataSize)
		}
	}
}

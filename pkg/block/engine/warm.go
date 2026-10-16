package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/journal"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
)

// WarmResult summarizes a WarmAll run: how many chunks came back from the
// remote tier and how many bytes they moved. Every enumerated chunk is
// attempted, but a chunk that is sparse or not yet synced returns no data and
// is not counted, so BlocksFetched is what landed, not the size of the work
// list — callers that need the enumerated total read it from the progress
// callback. See WarmAll for why there is no already-local count.
type WarmResult struct {
	BlocksFetched int64 `json:"blocks_fetched"`
	BytesFetched  int64 `json:"bytes_fetched"`
}

// warmTarget identifies one FileChunk row that must be fetched. It carries the
// resolved *block.FileChunk from enumeration so the worker fetches by the row
// in hand rather than round-tripping through a blockIdx lookup — FastCDC chunks
// start at arbitrary, non-BlockSize-aligned offsets, so a blockIdx lookup would
// miss every non-aligned chunk and silently skip it (#1374).
type warmTarget struct {
	payloadID string
	fb        *block.FileChunk
	span      hydrateSpan
}

// WarmAll proactively materializes every block of every payload in this share
// onto the local CAS tier by reusing the per-block fetch primitive
// (fetchResolvedBlock). It enumerates payloads from the authoritative metadata
// (fileChunkStore.EnumeratePayloads) and the per-payload FileChunk rows, and
// fetches every one of them with bounded concurrency
// (RemoteSyncConfig.ParallelDownloads). Enumerating the metadata rather than the
// local store's ListFiles is what lets warm materialize payloads whose append
// log was discarded after rollup — their FileChunk rows survive, but
// local.ListFiles no longer reports them, so the old surface made warm a silent
// no-op on rolled-up shares (#1374).
//
// Already-local chunks are re-fetched rather than skipped. The local tier is
// keyed by (payloadID, offset) and exposes no per-range residency probe: its
// DataExtents reports evicted ranges as data, so using it to decide "already
// local" would skip exactly the cold chunks warm exists to fetch. Re-hydrating a
// warm chunk is idempotent, so the cost of not knowing is a redundant GET, while
// the cost of guessing wrong is a warm run that silently does nothing.
//
// progress (may be nil) is invoked with the running (done, total) counts as
// blocks are processed, so callers can drive a poll/UI. total is the number of
// enumerated chunks, which is what this run will fetch. Callbacks are
// serialized independently of downloads, and payload admission is released
// before waiting for callbacks to finish. Blocks processed while a callback
// runs are reported together by the next call, so done may advance by more
// than one; the last call is delivered before WarmAll returns. Callbacks may
// inspect or mutate files; they must not close the engine whose lifecycle
// pins this operation.
//
// A nil remote tier is an error: there is nothing to warm from. A fetch that
// fails with fs.ErrDiskFull is terminal — the bounded local tier cannot hold
// the working set — so the whole run is cancelled and the error surfaced.
// Context cancellation stops the run promptly.
func (m *RemoteSync) WarmAll(ctx context.Context, progress func(done, total int64)) (WarmResult, error) {
	if err := m.checkReady(ctx); err != nil {
		return WarmResult{}, err
	}
	if m.remoteStore == nil {
		return WarmResult{}, errors.New("warm: share has no remote tier to warm from")
	}

	// Enumerate all FileChunk rows into the work list. The enumeration walks the
	// same surface as populateBlockCounts: fileChunkStore.EnumeratePayloads ->
	// per-payload ListFileChunks (the authoritative metadata, which survives
	// rollup). Each target carries the resolved row so the worker fetches by the
	// row in hand (fetchResolvedBlock) instead of round-tripping through a
	// BlockSize-aligned blockIdx lookup, which would miss every non-aligned
	// FastCDC chunk (#1374).
	var payloadIDs []string
	if err := m.fileChunkStore.EnumeratePayloads(ctx, func(payloadID string) error {
		payloadIDs = append(payloadIDs, payloadID)
		return nil
	}); err != nil {
		return WarmResult{}, fmt.Errorf("warm: enumerate payloads: %w", err)
	}

	// Payloads are admitted one at a time, in sorted order, and each stays
	// admitted from before its rows are read until the last fetch of those rows
	// has written its bytes back. Guarding already-resolved targets would still
	// let a replacement finish before a worker writes the old manifest's bytes
	// back; releasing each payload once it is warmed means a clone of one file
	// waits for that file's warm, not the whole share's. Sorting and
	// deduplicating exactly as clone does keeps reverse source/destination
	// operations from forming an admission cycle with a warm that holds several.
	slices.Sort(payloadIDs)
	payloadIDs = slices.Compact(payloadIDs)

	// Count the work first, so progress reports a total from its first call.
	// Each payload is admitted only while its rows are read. They are read again
	// when the payload is warmed, and the total follows any change in between.
	counted := make([]int64, len(payloadIDs))
	var total int64
	for i, payloadID := range payloadIDs {
		n, err := m.countWarmTargets(ctx, payloadID)
		if err != nil {
			return WarmResult{}, err
		}
		counted[i] = n
		total += n
	}

	prog := startWarmProgress(progress, total)
	if total == 0 {
		prog.finish()
		return WarmResult{}, nil
	}

	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		blocksFetched atomic.Int64
		bytesFetched  atomic.Int64
		errMu         sync.Mutex
		firstErr      error
	)
	fail := func(err error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		errMu.Unlock()
		cancel()
	}

	// Remote downloads are bounded by the same ParallelDownloads limit the
	// cold-read demand loop uses. A payload is admitted only once a download slot
	// is free, and that slot goes to its first fetch, so a payload is never held
	// while its first fetch waits for another payload's to finish. The first
	// error cancels the rest.
	g, gctx := errgroup.WithContext(ctx)
	slots := semaphore.NewWeighted(int64(m.downloadParallelism()))
	for i, payloadID := range payloadIDs {
		if gctx.Err() != nil {
			break // first error/cancel: stop scheduling the remaining payloads
		}
		if err := slots.Acquire(gctx, 1); err != nil {
			fail(err)
			break
		}
		release, err := m.admission.enter(gctx, payloadID)
		if err != nil {
			slots.Release(1)
			fail(err)
			break
		}
		// Sampled before this payload's rows are read, so a warm fetch cannot
		// write back over bytes written while it was running (see hydrateSpan).
		targets, err := m.warmTargets(gctx, payloadID, m.local.WriteVersion())
		if err != nil {
			release()
			slots.Release(1)
			fail(err)
			break
		}
		if d := int64(len(targets)) - counted[i]; d != 0 {
			prog.add(0, d)
		}
		// One reference per scheduled fetch plus this loop's own: the payload
		// is released by whichever finishes last. A fetch drops its reference
		// before it reports progress, so a callback waiting for exclusive
		// admission on this payload is not waiting on itself.
		var refs atomic.Int64
		refs.Store(1)
		unref := func() {
			if refs.Add(-1) == 0 {
				release()
			}
		}
		handed := false // whether a fetch has taken the slot acquired above
		for _, t := range targets {
			if gctx.Err() != nil {
				break
			}
			if handed {
				if err := slots.Acquire(gctx, 1); err != nil {
					break
				}
			}
			handed = true
			refs.Add(1)
			g.Go(func() error {
				defer slots.Release(1)
				data, err := m.fetchResolvedBlock(gctx, t.fb, t.span)
				unref()
				if err != nil {
					if errors.Is(err, journal.ErrLocalStoreFull) {
						err = fmt.Errorf("warm: local tier full while fetching %s (raise journal_size or evict): %w",
							t.fb.ID, err)
					} else {
						err = fmt.Errorf("warm: fetch %s: %w", t.fb.ID, err)
					}
					fail(err)
					return err
				}
				if data != nil {
					blocksFetched.Add(1)
					bytesFetched.Add(int64(len(data)))
				}
				prog.add(1, 0)
				return nil
			})
		}
		if !handed {
			slots.Release(1)
		}
		unref()
	}

	// Every payload is released once Wait returns: each fetch dropped its
	// reference, and the loop dropped its own. Counts are read after Wait so
	// both the success and failure returns report everything that landed.
	_ = g.Wait()
	prog.finish()
	errMu.Lock()
	err := firstErr
	errMu.Unlock()
	if err == nil {
		err = parent.Err()
	}
	return WarmResult{
		BlocksFetched: blocksFetched.Load(),
		BytesFetched:  bytesFetched.Load(),
	}, err
}

// countWarmTargets reports how many rows of payloadID a warm would fetch,
// holding the payload's admission only while they are read.
func (m *RemoteSync) countWarmTargets(ctx context.Context, payloadID string) (int64, error) {
	release, err := m.admission.enter(ctx, payloadID)
	if err != nil {
		return 0, err
	}
	defer release()
	targets, err := m.warmTargets(ctx, payloadID, m.local.WriteVersion())
	return int64(len(targets)), err
}

// warmTargets resolves payloadID's rows into the ranges a warm fetches. The
// caller holds the payload's admission and sampled at before calling.
func (m *RemoteSync) warmTargets(ctx context.Context, payloadID string, at uint64) ([]warmTarget, error) {
	rows, err := m.listFileChunksSnapshot(ctx, payloadID)
	if err != nil {
		return nil, fmt.Errorf("warm: list blocks for %s: %w", payloadID, err)
	}
	// Every placeable row start, ascending, so each row's claim end is a
	// binary search rather than another walk of the manifest.
	starts := make([]uint64, 0, len(rows))
	for _, fb := range rows {
		if fb == nil {
			continue
		}
		if absOff, ok := block.ParseChunkOffset(fb.ID); ok {
			starts = append(starts, absOff)
		}
	}
	slices.Sort(starts)

	var targets []warmTarget
	for _, fb := range rows {
		if fb == nil {
			continue
		}
		absOff, ok := block.ParseChunkOffset(fb.ID)
		if !ok {
			continue
		}
		// A row is warmed only over the bytes it still holds: a row
		// straddling a later row's start hands them over at that start, and
		// hydrating its full extent would leave its older bytes over the
		// newer row's head (see hydrateSpan). A row overlapped in its middle
		// therefore warms only up to that later row, and the remainder past
		// it is left for the demand read to fetch — one row here means one
		// range, and filling the gap with the older bytes is the outcome
		// this clamp exists to prevent.
		span := hydrateSpan{From: absOff, To: absOff + uint64(fb.DataSize), At: at}
		if i := sort.Search(len(starts), func(i int) bool { return starts[i] > absOff }); i < len(starts) && starts[i] < span.To {
			span.To = starts[i]
		}
		targets = append(targets, warmTarget{payloadID: payloadID, fb: fb, span: span})
	}
	return targets, nil
}

// warmProgress calls a warm's progress callback on one goroutine. Producers
// only add to two counters, so a fetch never waits for a callback: a callback
// may wait for exclusive admission on a payload that is released only after
// one of those fetches finishes. Whatever arrives while a callback runs is
// folded into the next call, so a stalled callback costs two counters rather
// than an entry per finished target, and every call still reports the counts
// after some prefix of the events.
type warmProgress struct {
	fn       func(done, total int64)
	mu       sync.Mutex
	done     int64 // targets finished since the last call
	total    int64 // corrections to the total since the last call
	closed   bool
	wake     chan struct{}
	finished chan struct{}
}

func startWarmProgress(fn func(done, total int64), total int64) *warmProgress {
	p := &warmProgress{fn: fn, wake: make(chan struct{}, 1), finished: make(chan struct{})}
	if fn == nil {
		close(p.finished)
		return p
	}
	go func() {
		defer close(p.finished)
		var done int64
		fn(0, total)
		for {
			p.mu.Lock()
			d, t, closed := p.done, p.total, p.closed
			p.done, p.total = 0, 0
			p.mu.Unlock()
			if d != 0 || t != 0 {
				done += d
				total += t
				fn(done, total)
				continue
			}
			if closed {
				return
			}
			<-p.wake
		}
	}()
	return p
}

// add records done more finished targets and a correction of total.
func (p *warmProgress) add(done, total int64) {
	if p.fn == nil {
		return
	}
	p.mu.Lock()
	p.done += done
	p.total += total
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// finish waits until every recorded event has been delivered.
func (p *warmProgress) finish() {
	if p.fn != nil {
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
		select {
		case p.wake <- struct{}{}:
		default:
		}
	}
	<-p.finished
}

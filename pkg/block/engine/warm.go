package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/journal"
)

// WarmResult summarizes a WarmAll run: how many blocks were fetched from the
// remote tier, how many bytes that fetch moved, and how many blocks were
// already physically present locally (skipped without a remote round-trip).
type WarmResult struct {
	BlocksFetched      int64 `json:"blocks_fetched"`
	BytesFetched       int64 `json:"bytes_fetched"`
	BlocksAlreadyLocal int64 `json:"blocks_already_local"`
}

// warmTarget identifies one FileChunk row that must be fetched. It carries the
// resolved *block.FileChunk from enumeration so the worker fetches by the row
// in hand rather than round-tripping through a blockIdx lookup — FastCDC chunks
// start at arbitrary, non-BlockSize-aligned offsets, so a blockIdx lookup would
// miss every non-aligned chunk and silently skip it (#1374).
type warmTarget struct {
	payloadID string
	fb        *block.FileChunk
}

// WarmAll proactively materializes every block of every payload in this share
// onto the local CAS tier by reusing the per-block fetch primitive
// (fetchResolvedBlock). It enumerates payloads from the authoritative metadata
// (fileChunkStore.EnumeratePayloads) and the per-payload FileChunk rows, skips
// any block already present locally, and fetches the rest with bounded
// concurrency (SyncerConfig.ParallelDownloads). Enumerating the metadata rather
// than the local store's ListFiles is what lets warm materialize payloads whose
// append log was discarded after rollup — their FileChunk rows survive, but
// local.ListFiles no longer reports them, so the old surface made warm a silent
// no-op on rolled-up shares (#1374).
//
// progress (may be nil) is invoked after each block is processed with the
// running (done, total) counts so callers can drive a poll/UI. total is the
// number of NOT-already-local blocks (the blocks this run will actually fetch).
//
// A nil remote tier is an error: there is nothing to warm from. A fetch that
// fails with fs.ErrDiskFull is terminal — the bounded local tier cannot hold
// the working set — so the whole run is cancelled and the error surfaced.
// Context cancellation stops the run promptly.
func (m *Syncer) WarmAll(ctx context.Context, progress func(done, total int64)) (WarmResult, error) {
	if err := m.checkReady(ctx); err != nil {
		return WarmResult{}, err
	}
	if m.remoteStore == nil {
		return WarmResult{}, errors.New("warm: share has no remote tier to warm from")
	}

	// Enumerate all FileChunk rows and split into already-local (counted,
	// skipped) and to-fetch (the work list). The enumeration walks the same
	// surface as populateBlockCounts: fileChunkStore.EnumeratePayloads ->
	// per-payload ListFileChunks (the authoritative metadata, which survives
	// rollup). Each to-fetch target carries the resolved row so the worker
	// fetches by the row in hand (fetchResolvedBlock) instead of round-tripping
	// through a BlockSize-aligned blockIdx lookup, which would miss every
	// non-aligned FastCDC chunk (#1374).
	var payloadIDs []string
	if err := m.fileChunkStore.EnumeratePayloads(ctx, func(payloadID string) error {
		payloadIDs = append(payloadIDs, payloadID)
		return nil
	}); err != nil {
		return WarmResult{}, fmt.Errorf("warm: enumerate payloads: %w", err)
	}

	var (
		targets      []warmTarget
		alreadyLocal int64
	)
	for _, payloadID := range payloadIDs {
		if err := ctx.Err(); err != nil {
			return WarmResult{}, err
		}
		rows, err := m.listFileChunksSnapshot(ctx, payloadID)
		if err != nil {
			return WarmResult{}, fmt.Errorf("warm: list blocks for %s: %w", payloadID, err)
		}
		for _, fb := range rows {
			if fb == nil {
				continue
			}
			if _, ok := block.ParseChunkOffset(fb.ID); !ok {
				continue
			}
			// ponytail: no per-hash local-presence probe (journal is not
			// hash-keyed), so warm every covering chunk. Re-hydrating an
			// already-warm chunk is idempotent — a redundant GET at worst.
			targets = append(targets, warmTarget{payloadID: payloadID, fb: fb})
		}
	}

	total := int64(len(targets))
	if progress != nil {
		progress(0, total)
	}
	if total == 0 {
		return WarmResult{BlocksAlreadyLocal: alreadyLocal}, nil
	}

	var (
		blocksFetched atomic.Int64
		bytesFetched  atomic.Int64
	)

	// progress reporting is serialized: the counter increment and the callback
	// emission happen under one lock so callbacks fire in monotonic `done`
	// order. Snapshotting an atomic counter and emitting outside the lock lets a
	// goroutine that incremented to N-1 call back after the one that reached N,
	// leaving the final observed progress below total even though every fetch
	// finished.
	var (
		progressMu sync.Mutex
		done       int64
	)
	emitProgress := func() {
		if progress == nil {
			return
		}
		progressMu.Lock()
		defer progressMu.Unlock()
		done++
		// Hold the lock across the user callback so emissions stay ordered;
		// defer the unlock so a panicking callback can't leave it held and
		// deadlock every later emit.
		progress(done, total)
	}

	// Bound remote-download concurrency via the shared fetchGroup helper (same
	// ParallelDownloads limit the cold-read demand loop uses). g.Go blocks once
	// the limit is reached and the first error cancels the rest via gctx.
	g, gctx := m.fetchGroup(ctx)

	for _, t := range targets {
		if gctx.Err() != nil {
			break // first error/cancel: stop scheduling the remaining fetches
		}
		t := t
		g.Go(func() error {
			data, err := m.fetchResolvedBlock(gctx, t.fb)
			if err != nil {
				if errors.Is(err, journal.ErrLocalStoreFull) {
					return fmt.Errorf("warm: local tier full while fetching %s (raise local_store_size or evict): %w",
						t.fb.ID, err)
				}
				return fmt.Errorf("warm: fetch %s: %w", t.fb.ID, err)
			}
			if data != nil {
				blocksFetched.Add(1)
				bytesFetched.Add(int64(len(data)))
			}
			emitProgress()
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return WarmResult{
			BlocksFetched:      blocksFetched.Load(),
			BytesFetched:       bytesFetched.Load(),
			BlocksAlreadyLocal: alreadyLocal,
		}, err
	}

	return WarmResult{
		BlocksFetched:      blocksFetched.Load(),
		BytesFetched:       bytesFetched.Load(),
		BlocksAlreadyLocal: alreadyLocal,
	}, nil
}

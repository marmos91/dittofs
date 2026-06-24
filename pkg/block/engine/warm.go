package engine

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"golang.org/x/sync/errgroup"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
)

// WarmResult summarizes a WarmAll run: how many blocks were fetched from the
// remote tier, how many bytes that fetch moved, and how many blocks were
// already physically present locally (skipped without a remote round-trip).
type WarmResult struct {
	BlocksFetched      int64 `json:"blocks_fetched"`
	BytesFetched       int64 `json:"bytes_fetched"`
	BlocksAlreadyLocal int64 `json:"blocks_already_local"`
}

// warmTarget identifies one (payloadID, blockIdx) pair that must be fetched.
type warmTarget struct {
	payloadID string
	blockIdx  uint64
}

// WarmAll proactively materializes every block of every payload in this share
// onto the local CAS tier by reusing the per-block fetch primitive
// (fetchBlock). It enumerates payloads from the authoritative metadata
// (fileBlockStore.EnumeratePayloads) and the per-payload FileBlock rows, skips
// any block already present locally, and fetches the rest with bounded
// concurrency (SyncerConfig.ParallelDownloads). Enumerating the metadata rather
// than the local store's ListFiles is what lets warm materialize payloads whose
// append log was discarded after rollup — their FileBlock rows survive, but
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

	// Enumerate all (payloadID, blockIdx) pairs and split into already-local
	// (counted, skipped) and to-fetch (the work list). The enumeration walks
	// the same surface as populateBlockCounts: fileBlockStore.EnumeratePayloads
	// -> per-payload ListFileBlocks (the authoritative metadata, which survives
	// rollup). blockIdx is derived from the chunk's absolute offset the same way
	// the read path does (resolveFileBlockFromRows / blockRange).
	var payloadIDs []string
	if err := m.fileBlockStore.EnumeratePayloads(ctx, func(payloadID string) error {
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
		rows, err := m.listFileBlocksSnapshot(ctx, payloadID)
		if err != nil {
			return WarmResult{}, fmt.Errorf("warm: list blocks for %s: %w", payloadID, err)
		}
		for _, fb := range rows {
			if fb == nil {
				continue
			}
			abs, ok := block.ParseChunkOffset(fb.ID)
			if !ok {
				continue
			}
			blockIdx := abs / uint64(BlockSize)
			if m.blockIsLocalFromRow(ctx, fb) {
				alreadyLocal++
				continue
			}
			targets = append(targets, warmTarget{payloadID: payloadID, blockIdx: blockIdx})
		}
	}

	total := int64(len(targets))
	if progress != nil {
		progress(0, total)
	}
	if total == 0 {
		return WarmResult{BlocksAlreadyLocal: alreadyLocal}, nil
	}

	parallel := m.config.ParallelDownloads
	if parallel < 1 {
		parallel = 1
	}

	var (
		blocksFetched atomic.Int64
		bytesFetched  atomic.Int64
		done          atomic.Int64
	)

	g, gctx := errgroup.WithContext(ctx)
	sem := make(chan struct{}, parallel)

	for _, t := range targets {
		t := t
		// Block for a slot, but stop dispatching the moment the group is
		// failing/cancelled (a fetch returned an error or ctx was cancelled).
		select {
		case <-gctx.Done():
		case sem <- struct{}{}:
		}
		if gctx.Err() != nil {
			break
		}
		g.Go(func() error {
			defer func() { <-sem }()
			data, err := m.fetchBlock(gctx, t.payloadID, t.blockIdx)
			if err != nil {
				if errors.Is(err, fs.ErrDiskFull) {
					return fmt.Errorf("warm: local tier full while fetching %s/%d (raise local_store_size or evict): %w",
						t.payloadID, t.blockIdx, err)
				}
				return fmt.Errorf("warm: fetch %s/%d: %w", t.payloadID, t.blockIdx, err)
			}
			if data != nil {
				blocksFetched.Add(1)
				bytesFetched.Add(int64(len(data)))
			}
			cur := done.Add(1)
			if progress != nil {
				progress(cur, total)
			}
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

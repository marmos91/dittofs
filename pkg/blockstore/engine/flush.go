package engine

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/chunker"
)

// Flush ensures all dirty data for a payload is persisted.
//
// Pre-rollup file-level dedup hook: when a coordinator is wired and the
// file's speculative BlockRef manifest is non-empty, the engine asks the
// coordinator whether a previously-quiesced file with the same Merkle
// root already exists. On hit the upload pump is bypassed entirely —
// FileAttr.Blocks is swapped to the target's BlockRef list, refcounts
// are reconciled, and Flush returns Finalized=true without delegating
// to the syncer. On miss / nil-coordinator the syncer's mirror loop
// runs as usual.
//
// Auto-promote into the read buffer is intentionally NOT done here
// the Cache is CAS-keyed and Flush has no BlockRef snapshot at this
// layer to translate flushed bytes into hash-keyed cache entries.
func (bs *Store) Flush(ctx context.Context, payloadID string) (*blockstore.FlushResult, error) {
	// Both pre-rollup dedup hooks require a coordinator; gate them
	// jointly so the nil-check isn't repeated.
	if bs.coordinator != nil {
		// Opt 4: eager small-file dedup BEFORE
		// the speculative path. Files at or below chunker.MinChunkSize
		// (1 MiB) emit a single chunk under FastCDC anyway; hashing the
		// whole content in RAM and consulting metadata.FindByObjectID
		// skips chunker + log + CAS write entirely on hit. Sibling fast-
		// path; shares applyFileLevelDedupHit's finalize machinery so
		// + cache invalidation + appendlog cleanup
		// invariants remain identical to the speculative path.
		//
		// Source-of-truth for the in-RAM bytes: bs.local.ReadPayloadAt
		// consults the per-payload appendlog (pre-rollup bytes) before
		// the FileBlock manifest, which is the right surface — eager runs
		// BEFORE the rollup commits anything to CAS. For local stores that
		// have already rolled up (the in-memory backend's synchronous
		// rollup, FSStore steady state), ReadPayloadAt walks the manifest
		// and serves the same bytes from the now-stored chunks; the eager
		// path's hash + lookup are identical either way.
		//
		// Outer size gate at the call site is intentionally defensive
		// (tryEagerSmallFileDedup re-checks internally — that gate is the
		// real authority) but lets us skip the ReadPayloadAt alloc + I/O
		// entirely for large files.
		if size, found := bs.local.GetFileSize(ctx, payloadID); found && size > 0 && size <= chunker.MinChunkSize {
			// Outer gate already bounds size to chunker.MinChunkSize (1 MiB)
			// well below math.MaxInt on every supported platform. The cast
			// here is therefore safe; the explicit form documents the
			// bounded-uint64->int conversion for readers and linters.
			isize := int(size)
			data := make([]byte, isize)
			n, err := bs.local.ReadPayloadAt(ctx, payloadID, data, 0)
			// On a clean read we have the full payload in RAM; consult
			// eager dedup. A short / errored read is treated as "skip
			// eager and fall through to speculative" — the eager
			// optimisation is opportunistic and never blocks Flush.
			if err == nil && n == isize {
				hit, derr := bs.syncer.tryEagerSmallFileDedup(ctx, payloadID, data)
				if derr != nil {
					return nil, fmt.Errorf("eager small-file dedup: %w", derr)
				}
				if hit {
					return &blockstore.FlushResult{Finalized: true}, nil
				}
			}
		}

		// File-level dedup pre-hook: if a fully-quiesced manifest matches
		// an already-stored ObjectID, skip the upload pump entirely.
		specBlocks, blockStates, err := bs.syncer.snapshotPendingBlockRefs(ctx, payloadID)
		if err != nil {
			return nil, fmt.Errorf("snapshot pending blockrefs: %w", err)
		}
		if len(specBlocks) > 0 {
			fileObjectID, err := bs.coordinator.GetFileObjectID(ctx, payloadID)
			if err != nil {
				return nil, fmt.Errorf("get file objectID: %w", err)
			}
			hit, err := bs.syncer.trySpeculativeFileLevelDedup(ctx, payloadID, specBlocks, fileObjectID, blockStates)
			if err != nil {
				return nil, fmt.Errorf("file-level dedup: %w", err)
			}
			if hit {
				return &blockstore.FlushResult{Finalized: true}, nil
			}
		}
	}
	// Delegate to syncer's mirror loop.
	return bs.syncer.Flush(ctx, payloadID)
}

// DrainAllUploads waits for all pending uploads to complete.
func (bs *Store) DrainAllUploads(ctx context.Context) error {
	return bs.syncer.DrainAllUploads(ctx)
}

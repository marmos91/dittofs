package engine

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/journal"
)

// blockRefHashes extracts the ContentHash slice from a ChunkRef list
// for OnRead's hint API.
func blockRefHashes(refs []block.ChunkRef) []block.ContentHash {
	out := make([]block.ContentHash, len(refs))
	for i, r := range refs {
		out[i] = r.Hash
	}
	return out
}

// readAtInternal reads from the primary payloadID via the journal-backed local
// tier. journal.ReadAt fills dst with the file's local bytes and zero-fills
// never-written holes; if any requested byte was written-but-evicted it reports
// cold=true. On a cold read the engine hydrates the covering chunks from the
// remote store (verified) back into the journal and re-reads, so a warm re-read
// serves the fetched bytes. A genuinely sparse hole (no remote chunk) stays
// zero-filled — RFC-safe.
func (bs *Store) readAtInternal(ctx context.Context, payloadID string, data []byte, offset uint64) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}

	n, cold, err := bs.local.ReadAt(ctx, payloadID, int64(offset), data)
	if err != nil {
		var corrupt *journal.CorruptRangeError
		if errors.As(err, &corrupt) {
			// A durable-tier warm read detected on-disk corruption. The local bytes
			// are untrustworthy, so never return them: heal from the remote or fail
			// closed.
			return bs.healCorruptWarmRead(ctx, payloadID, data, offset)
		}
		return 0, fmt.Errorf("local read failed: %w", err)
	}
	if !cold {
		return n, nil
	}

	// Cold: some requested bytes are written-but-evicted. Fetch + hydrate the
	// covering remote chunks, then re-read the now-warm window.
	if err := bs.ensureAndReadFromLocal(ctx, payloadID, data, offset); err != nil {
		return 0, err
	}
	return len(data), nil
}

// healCorruptWarmRead recovers from on-disk corruption that a durable-tier warm
// read detected (journal returned *CorruptRangeError). With a remote store it
// re-fetches the covering chunks through the standard hydrate path — the fetch
// is BLAKE3-verified and the fresh Hydrate supersedes the corrupt local interval
// by version — then re-reads the now-healed bytes. Without a remote there is no
// good copy to heal from, so it fails closed with ErrIntegrityCheckFailed (maps
// to NFS3ERR_IO) rather than returning corrupt or zero-filled bytes.
func (bs *Store) healCorruptWarmRead(ctx context.Context, payloadID string, data []byte, offset uint64) (int, error) {
	if !bs.HasRemoteStore() {
		return 0, fmt.Errorf("warm read integrity failure for %s at offset %d (local-only, no remote to heal from): %w",
			payloadID, offset, block.ErrIntegrityCheckFailed)
	}
	if err := bs.ensureAndReadFromLocal(ctx, payloadID, data, offset); err != nil {
		return 0, fmt.Errorf("heal corrupt warm read for %s at offset %d: %w", payloadID, offset, err)
	}
	return len(data), nil
}

// ensureAndReadFromLocal hydrates every remote-resident chunk covering the
// window into the local journal, then re-reads. EnsureAvailableAndRead resolves
// the covering FileChunk rows, does one BLAKE3-verified ranged read per chunk,
// and Hydrates the plaintext at the chunk's file offset; a range with no remote
// chunk (genuine sparse hole) is left for ReadAt's zero-fill below.
func (bs *Store) ensureAndReadFromLocal(ctx context.Context, payloadID string, dest []byte, offset uint64) error {
	if _, err := bs.syncer.EnsureAvailableAndRead(ctx, payloadID, offset, uint32(len(dest)), dest); err != nil {
		return fmt.Errorf("cold read hydrate failed: %w", err)
	}
	if _, _, err := bs.local.ReadAt(ctx, payloadID, int64(offset), dest); err != nil {
		return fmt.Errorf("read after hydrate failed: %w", err)
	}
	return nil
}

// rowWithOffset bundles a FileChunk row with the absolute payload
// offset of its first byte. The carve BlockSink encodes the chunk's
// absolute offset directly as the numeric component of the row ID
// ("<payloadID>/<chunkOffset>"), so absOffset is the parsed
// component verbatim.
type rowWithOffset struct {
	fb        *block.FileChunk
	absOffset uint64
}

// findRowCoveringOffset returns the row whose absolute byte range
// [absOffset, absOffset+DataSize) contains target, or nil if no row
// in rows covers it. The walk is O(N) over the per-payload row
// list — acceptable for the FastCDC steady-state (chunks average ~4 MiB
// so even a 4 GiB file produces ~1000 rows).
//
// A row whose ID does not parse cannot be placed, so its range is unknown. That
// is only fatal to reads it might have covered: if some other row covers target,
// that answer is unaffected and is returned. Only when nothing covers target does
// the unplaceable row matter, because then the choice is between reporting a hole
// — which the caller zero-fills, inventing data the file may never have had — and
// admitting the manifest is inconsistent. It admits.
//
// Scoping it this way keeps one bad row from making a whole payload unreadable.
// The alternative, refusing the moment such a row is seen at any offset, would
// take a file that reads correctly apart from one damaged range and make all of
// it unavailable.
//
// When two rows cover target the greatest start wins, which is what the indexed
// badger lookup returns. Returning whichever row the walk reached first made the
// answer depend on ListFileChunks ordering, so the same read could serve
// different bytes on different backends. Overlap is not hypothetical: a truncate
// narrows a straddling row to the new size, and a later write re-carves from an
// earlier chunk boundary, leaving the narrowed row still claiming bytes the new
// row also covers. The greater start is the newer row, so it is also the one
// holding the bytes the last write put there.
func findRowCoveringOffset(rows []*block.FileChunk, target uint64) (*rowWithOffset, error) {
	unplaceable := ""
	var hit *rowWithOffset
	for _, fb := range rows {
		if fb == nil {
			continue
		}
		abs, ok := block.ParseChunkOffset(fb.ID)
		if !ok {
			if unplaceable == "" {
				unplaceable = fb.ID
			}
			continue
		}
		if target >= abs && target < abs+uint64(fb.DataSize) {
			if hit == nil || abs > hit.absOffset {
				hit = &rowWithOffset{fb: fb, absOffset: abs}
			}
		}
	}
	if hit == nil && unplaceable != "" {
		return nil, fmt.Errorf("%w: nothing covers offset %d and manifest holds unplaceable row %q",
			block.ErrManifestInconsistent, target, unplaceable)
	}
	return hit, nil
}

// chunkAtOffsetResolver is the indexed covering-chunk lookup, implemented only
// by the badger metadata backend. resolveCovering type-asserts for it and falls
// back to a ListFileChunks walk otherwise.
type chunkAtOffsetResolver interface {
	GetFileChunkAtOffset(ctx context.Context, payloadID string, off uint64) (*block.FileChunk, error)
}

// resolveCovering returns the FileChunk covering absolute byte offset off and
// its parsed absolute start offset, or (nil, 0, nil) for a hole. When the store
// implements chunkAtOffsetResolver (badger) it uses the indexed single-chunk
// lookup that avoids enumerating the whole per-payload manifest; otherwise it
// falls back to ListFileChunks + findRowCoveringOffset (memory/sqlite/postgres —
// not the profiled hot path).
func resolveCovering(ctx context.Context, store block.EngineFileChunkStore, payloadID string, off uint64) (*block.FileChunk, uint64, error) {
	if store == nil {
		return nil, 0, nil
	}
	if r, ok := store.(chunkAtOffsetResolver); ok {
		fb, err := r.GetFileChunkAtOffset(ctx, payloadID, off)
		if err != nil || fb == nil {
			return nil, 0, err
		}
		abs, ok := block.ParseChunkOffset(fb.ID)
		if !ok {
			return nil, 0, fmt.Errorf("%w: malformed FileChunk ID %q", block.ErrManifestInconsistent, fb.ID)
		}
		return fb, abs, nil
	}
	rows, err := store.ListFileChunks(ctx, payloadID)
	if err != nil {
		if errors.Is(err, block.ErrFileChunkNotFound) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	rw, err := findRowCoveringOffset(rows, off)
	if err != nil {
		return nil, 0, err
	}
	if rw == nil {
		return nil, 0, nil
	}
	return rw.fb, rw.absOffset, nil
}

// chunkAtOrAfterOffsetResolver is the indexed successor lookup, implemented only
// by the badger metadata backend. resolveNextChunkStart type-asserts for it and
// falls back to a ListFileChunks walk otherwise.
type chunkAtOrAfterOffsetResolver interface {
	GetFileChunkAtOrAfterOffset(ctx context.Context, payloadID string, off uint64) (*block.FileChunk, error)
}

// resolveNextChunkStart returns the absolute start offset of the first chunk of
// payloadID that begins strictly after off, or ok=false when none does — every
// remaining byte is hole. It is the step a reader takes to cross a hole: off is
// known to be uncovered, so the next data can only begin later.
//
// Asking for off+1 rather than off keeps a zero-length row parked exactly at off
// from answering with off itself, which would stall a caller that loops on the
// result.
//
// When the store implements chunkAtOrAfterOffsetResolver (badger) the successor
// comes from the chunk-offset index; other backends fall back to a ListFileChunks
// walk, which mirrors resolveCovering and is likewise not the profiled hot path.
//
// Either way a row whose ID cannot be placed is reported rather than stepped
// over. Unlike coverage, the successor answer is not scoped to a single row: an
// unplaceable row sits at an unknown offset, so it may be the true successor,
// and returning a later one would silently reclassify the bytes it holds as hole
// for the caller to zero-fill. The two lookups here and in resolveCovering are
// independent — a backend may index coverage without indexing succession, and
// each walk is its own ListFileChunks snapshot — so the guard cannot be borrowed
// from findRowCoveringOffset having already run.
func resolveNextChunkStart(ctx context.Context, store block.EngineFileChunkStore, payloadID string, off uint64) (uint64, bool, error) {
	if store == nil {
		return 0, false, nil
	}
	if off == math.MaxUint64 {
		return 0, false, nil // nothing can start after the last representable offset
	}
	if r, ok := store.(chunkAtOrAfterOffsetResolver); ok {
		fb, err := r.GetFileChunkAtOrAfterOffset(ctx, payloadID, off+1)
		if err != nil || fb == nil {
			return 0, false, err
		}
		abs, ok := block.ParseChunkOffset(fb.ID)
		if !ok {
			return 0, false, fmt.Errorf("%w: nothing covers offset %d and the next chunk is unplaceable row %q",
				block.ErrManifestInconsistent, off, fb.ID)
		}
		return abs, true, nil
	}
	rows, err := store.ListFileChunks(ctx, payloadID)
	if err != nil {
		if errors.Is(err, block.ErrFileChunkNotFound) {
			return 0, false, nil
		}
		return 0, false, err
	}
	var (
		best        uint64
		found       bool
		unplaceable string
	)
	for _, fb := range rows {
		if fb == nil {
			continue
		}
		abs, parsed := block.ParseChunkOffset(fb.ID)
		if !parsed {
			if unplaceable == "" {
				unplaceable = fb.ID
			}
			continue
		}
		if abs <= off {
			continue
		}
		if !found || abs < best {
			best, found = abs, true
		}
	}
	if unplaceable != "" {
		return 0, false, fmt.Errorf("%w: nothing covers offset %d and the manifest holds unplaceable row %q",
			block.ErrManifestInconsistent, off, unplaceable)
	}
	return best, found, nil
}

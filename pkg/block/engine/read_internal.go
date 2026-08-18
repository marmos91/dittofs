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
// tier. journal.ReadAt fills dst with the file's local bytes, zero-fills the
// rest, and reports whether the window held evicted (cold) or uncovered (hole)
// ranges. Both are reconciled against the CAS manifest: the covering chunks are
// hydrated from the remote store (verified) back into the journal and re-read. A
// range no manifest row covers is a genuinely sparse hole and stays zero-filled
// — RFC-safe.
//
// A hole is reconciled because the local tier cannot tell a never-written range
// from one whose cold interval it lost: cold seeding cannot place a manifest row
// whose ID carries no offset, so that range arrives here as a plain hole and
// only the manifest can say its zeros would be invented. DataExtents widens its
// map to the whole file for such a row, so SEEK reports data where READ refuses
// — never a hole where READ would have refused.
//
// A fully warm read never consults the manifest, and neither does a hole on a
// local-only share, where there is no remote to hydrate from.
func (bs *Store) readAtInternal(ctx context.Context, payloadID string, data []byte, offset uint64) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}

	n, st, err := bs.local.ReadAt(ctx, payloadID, int64(offset), data)
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
	reconcile := st.Cold || (st.Hole && bs.HasRemoteStore())
	if !reconcile {
		return n, nil
	}

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
// the covering FileChunk rows — refusing with ErrManifestInconsistent when an
// uncovered offset sits on a payload holding an unplaceable row — does one
// BLAKE3-verified ranged read per chunk, and Hydrates the plaintext at the
// chunk's file offset; a range with no remote chunk (genuine sparse hole) is
// left for ReadAt's zero-fill below.
//
// The re-read's cold flag is the post-condition, not a leftover: hydration is
// supposed to have made every written-but-evicted byte in the window local
// again, so a window still reporting cold means some byte the manifest claims
// was written could not be brought back. ReadAt zero-fills what it cannot serve,
// so accepting a cold re-read would hand the caller zeros for real data with no
// error anywhere. Fail closed instead. A never-written hole does not report
// cold, so a genuinely sparse file still reads as zeros; a zero-length window
// never reaches here (readAtInternal returns early), and would report cold=false
// regardless, so the guard has nothing to fail open on.
func (bs *Store) ensureAndReadFromLocal(ctx context.Context, payloadID string, dest []byte, offset uint64) error {
	if _, err := bs.syncer.EnsureAvailableAndRead(ctx, payloadID, offset, uint32(len(dest)), dest); err != nil {
		return fmt.Errorf("manifest reconcile for %s at offset %d failed: %w", payloadID, offset, err)
	}
	_, st, err := bs.local.ReadAt(ctx, payloadID, int64(offset), dest)
	if err != nil {
		return fmt.Errorf("read after hydrate failed: %w", err)
	}
	if st.Cold {
		return fmt.Errorf("window for %s at offset %d (%d bytes) is still cold after hydrate: %w",
			payloadID, offset, len(dest), block.ErrChunkNotFound)
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
// When two rows cover target, the greatest start wins, which is what the indexed
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
		// target-abs is overflow-free because target >= abs is checked first;
		// abs+DataSize would wrap on an absurd offset.
		if target >= abs && target-abs < uint64(fb.DataSize) {
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

// chunkWindowResolver answers covering and successor lookups for one payload
// across a whole window walk. A backend that indexes the lookup is asked per
// offset, exactly as a single-shot resolve would. A backend without the index
// falls back to a ListFileChunks scan, and a walk crossing K chunks would then
// pay K full per-payload manifest fetches; the resolver takes that scan once and
// answers every offset of the walk from the same rows, so the walk costs one
// fetch regardless of how many chunks it spans.
//
// The snapshot is deliberately scoped to a single walk. A manifest mutating
// mid-walk yields a torn view either way, and one consistent view is the safer
// of the two.
type chunkWindowResolver struct {
	store     block.EngineFileChunkStore
	payloadID string
	rows      []*block.FileChunk
	listed    bool
}

func newChunkWindowResolver(store block.EngineFileChunkStore, payloadID string) *chunkWindowResolver {
	return &chunkWindowResolver{store: store, payloadID: payloadID}
}

// list returns the payload's manifest rows, scanning at most once per resolver.
// A payload with no rows caches the empty snapshot too, so a sparse payload is
// not re-scanned on every offset of the walk.
func (r *chunkWindowResolver) list(ctx context.Context) ([]*block.FileChunk, error) {
	if r.listed {
		return r.rows, nil
	}
	rows, err := r.store.ListFileChunks(ctx, r.payloadID)
	if err != nil {
		if errors.Is(err, block.ErrFileChunkNotFound) {
			r.listed = true
			return nil, nil
		}
		return nil, err
	}
	r.rows, r.listed = rows, true
	return rows, nil
}

// covering returns the FileChunk covering absolute byte offset off, its parsed
// absolute start offset, and the offset at which its claim on the bytes from
// off onwards ends, or (nil, 0, 0, nil) for a hole.
//
// The claim ends at the row's own end, or at the start of the next row that
// begins inside it, whichever comes first. Coverage resolves an overlap to the
// greatest start, so from the first byte of a later-starting row it is that row
// that holds the bytes, and the straddling row's remaining extent describes
// bytes it no longer owns. A caller that consumed the straddler's full extent
// would step over the later row entirely and, on the fetch path, write the
// older bytes over the newer row's head.
//
// A successor lookup that fails is treated as "no later row known" rather than
// propagated: a row whose ID cannot be placed must not make a covered read
// fail, which is the same scoping findRowCoveringOffset applies.
func (r *chunkWindowResolver) covering(ctx context.Context, off uint64) (*block.FileChunk, uint64, uint64, error) {
	fb, abs, err := r.coveringRow(ctx, off)
	if err != nil || fb == nil {
		return nil, 0, 0, err
	}
	claimEnd := abs + uint64(fb.DataSize)
	if next, ok, nextErr := r.nextStart(ctx, off); nextErr == nil && ok && next < claimEnd {
		claimEnd = next
	}
	return fb, abs, claimEnd, nil
}

// coveringRow resolves the row covering off and its absolute start offset,
// through the backend's chunk-offset index when it has one.
func (r *chunkWindowResolver) coveringRow(ctx context.Context, off uint64) (*block.FileChunk, uint64, error) {
	if idx, ok := r.store.(chunkAtOffsetResolver); ok {
		fb, err := idx.GetFileChunkAtOffset(ctx, r.payloadID, off)
		if err != nil || fb == nil {
			return nil, 0, err
		}
		abs, parsed := block.ParseChunkOffset(fb.ID)
		if !parsed {
			return nil, 0, fmt.Errorf("%w: malformed FileChunk ID %q", block.ErrManifestInconsistent, fb.ID)
		}
		return fb, abs, nil
	}
	rows, err := r.list(ctx)
	if err != nil {
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

// resolveCovering returns the FileChunk covering absolute byte offset off and
// its parsed absolute start offset, or (nil, 0, nil) for a hole. When the store
// implements chunkAtOffsetResolver (badger) it uses the indexed single-chunk
// lookup that avoids enumerating the whole per-payload manifest; otherwise it
// falls back to ListFileChunks + findRowCoveringOffset (memory/sqlite/postgres —
// not the profiled hot path). Callers walking a whole window should hold a
// chunkWindowResolver instead so the fallback scan is paid once.
func resolveCovering(ctx context.Context, store block.EngineFileChunkStore, payloadID string, off uint64) (*block.FileChunk, uint64, error) {
	if store == nil {
		return nil, 0, nil
	}
	return newChunkWindowResolver(store, payloadID).coveringRow(ctx, off)
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
// for the caller to zero-fill. The coverage and succession lookups are
// independent — a backend may index coverage without indexing succession — so
// the guard cannot be borrowed from findRowCoveringOffset having already run,
// even when both fall back to the same snapshot.
func resolveNextChunkStart(ctx context.Context, store block.EngineFileChunkStore, payloadID string, off uint64) (uint64, bool, error) {
	if store == nil {
		return 0, false, nil
	}
	return newChunkWindowResolver(store, payloadID).nextStart(ctx, off)
}

// nextStart is resolveNextChunkStart over this resolver's snapshot.
func (r *chunkWindowResolver) nextStart(ctx context.Context, off uint64) (uint64, bool, error) {
	if off == math.MaxUint64 {
		return 0, false, nil // nothing can start after the last representable offset
	}
	if idx, ok := r.store.(chunkAtOrAfterOffsetResolver); ok {
		fb, err := idx.GetFileChunkAtOrAfterOffset(ctx, r.payloadID, off+1)
		if err != nil || fb == nil {
			return 0, false, err
		}
		abs, parsed := block.ParseChunkOffset(fb.ID)
		if !parsed {
			return 0, false, fmt.Errorf("%w: nothing covers offset %d and the next chunk is unplaceable row %q",
				block.ErrManifestInconsistent, off, fb.ID)
		}
		return abs, true, nil
	}
	rows, err := r.list(ctx)
	if err != nil {
		return 0, false, err
	}
	if unplaceable := firstUnplaceable(rows); unplaceable != "" {
		return 0, false, fmt.Errorf("%w: nothing covers offset %d and the manifest holds unplaceable row %q",
			block.ErrManifestInconsistent, off, unplaceable)
	}
	best, found := minStartAfter(rows, off)
	return best, found, nil
}

// minStartAfter returns the smallest start offset among rows that begins
// strictly after off. A row whose ID carries no offset is skipped: it sits at an
// unknown place, so it can neither confirm nor deny a successor. A caller for
// which that ambiguity is fatal checks firstUnplaceable itself.
func minStartAfter(rows []*block.FileChunk, off uint64) (uint64, bool) {
	var (
		best  uint64
		found bool
	)
	for _, fb := range rows {
		if fb == nil {
			continue
		}
		abs, parsed := block.ParseChunkOffset(fb.ID)
		if !parsed || abs <= off {
			continue
		}
		if !found || abs < best {
			best, found = abs, true
		}
	}
	return best, found
}

// firstUnplaceable returns the ID of the first row whose ID carries no offset,
// or "" when every row can be placed.
func firstUnplaceable(rows []*block.FileChunk) string {
	for _, fb := range rows {
		if fb == nil {
			continue
		}
		if _, ok := block.ParseChunkOffset(fb.ID); !ok {
			return fb.ID
		}
	}
	return ""
}

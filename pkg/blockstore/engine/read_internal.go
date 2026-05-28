package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore"
)

// blockRefHashes extracts the ContentHash slice from a BlockRef list
// for OnRead's hint API.
func blockRefHashes(refs []blockstore.BlockRef) []blockstore.ContentHash {
	out := make([]blockstore.ContentHash, len(refs))
	for i, r := range refs {
		out[i] = r.Hash
	}
	return out
}

// computeFileSize returns the maximum (Offset + Size) across the
// BlockRef list — a conservative upper bound on file size used as
// the OnRead fileSize hint.
func computeFileSize(refs []blockstore.BlockRef) uint64 {
	var maxEnd uint64
	for _, r := range refs {
		end := r.Offset + uint64(r.Size)
		if end > maxEnd {
			maxEnd = end
		}
	}
	return maxEnd
}

// readAtInternal reads from the primary payloadID. Always goes through
// the local store (with remote-fallback on miss); the cache is hint-only
// and does not serve bytes here.
//
// The primary entry is LocalStore.ReadPayloadAt — a payload-keyed read
// that consults BOTH the in-flight append log (pre-rollup bytes) AND
// the rolled-up CAS chunks via the FileBlock manifest. This closes the
// pre-rollup read-after-write window where freshly-appended bytes would
// otherwise return zeros until the async rollup commits FileBlock rows.
//
// On a local miss (ErrFileBlockNotFound), fall back to the CAS-hash
// walk (readLocalByHash, used for chunks that the manifest knows about
// but the LocalStore did not surface — e.g., post-eviction reads where
// only the metadata row survived) and finally to remote-fetch via the
// syncer.
func (bs *BlockStore) readAtInternal(ctx context.Context, payloadID string, data []byte, offset uint64) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}

	// Primary: payload-keyed local read. Covers both the pre-rollup
	// append-log window and the post-rollup CAS path.
	n, err := bs.local.ReadPayloadAt(ctx, payloadID, data, offset)
	if err == nil {
		return n, nil
	}
	if !errors.Is(err, blockstore.ErrFileBlockNotFound) {
		return 0, fmt.Errorf("local read failed: %w", err)
	}

	// Local miss — try the CAS-hash walk (handles edge cases where the
	// FileBlockStore manifest is reachable via the engine's
	// fileBlockStore field but not the LocalStore-internal one).
	found, err := bs.readLocalByHash(ctx, payloadID, data, offset)
	if err != nil {
		return 0, fmt.Errorf("local read failed: %w", err)
	}
	if found {
		return len(data), nil
	}

	if err := bs.ensureAndReadFromLocal(ctx, payloadID, data, offset); err != nil {
		return 0, err
	}

	return len(data), nil
}

// ensureAndReadFromLocal downloads blocks from remote if needed and reads from local store.
func (bs *BlockStore) ensureAndReadFromLocal(ctx context.Context, payloadID string, dest []byte, offset uint64) error {
	length := uint32(len(dest))

	// Fast path: direct-serve copies S3 data directly to dest, skipping a second read.
	filled, err := bs.syncer.EnsureAvailableAndRead(ctx, payloadID, offset, length, dest)
	if err != nil {
		return fmt.Errorf("direct download failed: %w", err)
	}
	if filled {
		return nil
	}

	found, err := bs.readLocalByHash(ctx, payloadID, dest, offset)
	if err != nil {
		return fmt.Errorf("read after download failed: %w", err)
	}
	if !found {
		clear(dest)
		logger.Debug("Sparse block: miss after download, returning zeros",
			"payloadID", payloadID)
	}

	return nil
}

// readLocalByHash serves [offset, offset+len(dest)) by walking the
// payload's CAS chunk manifest (via FileBlockStore.ListFileBlocks)
// finding each chunk whose absolute byte range intersects the
// requested window, and copying the matching slice of the local CAS
// chunk into dest. Returns (true, nil) when every requested byte was
// satisfied locally and (false, nil) when any portion of the window
// could not be served from local CAS — the caller treats the false
// outcome as "must fall back to remote-fetch".
//
// On any unexpected error (FileBlock store failure, local chunk store
// I/O error other than ErrChunkNotFound) the function returns
// (false, err) so the engine can surface it to the protocol layer.
//
// Chunk geometry: under the unified CAS surface chunk boundaries are
// FastCDC-derived (variable size, absolute Offset stored on the
// FileBlock row's ID-derived blockIdx slot). The walk is O(N) over
// the per-payload row list — acceptable for the test fixtures (small
// N) and for the steady-state production stream where N is bounded
// by the payload's total size divided by the average chunk size
// (~4 MiB).
func (bs *BlockStore) readLocalByHash(ctx context.Context, payloadID string, dest []byte, offset uint64) (bool, error) {
	if len(dest) == 0 {
		return true, nil
	}
	// The engine consults the same EngineFileBlockStore the syncer
	// uses; ListFileBlocks returns the per-payload row list in
	// blockIdx order, which is offset order under the persister's
	// blockIdx := chunkOffset / BlockSize derivation. Rows missing
	// from the list are sparse: the caller falls back to the
	// remote-fetch + zero-fill path.
	if bs.fileBlockStore == nil {
		return false, nil
	}
	rows, err := bs.fileBlockStore.ListFileBlocks(ctx, payloadID)
	if err != nil {
		return false, err
	}
	if len(rows) == 0 {
		return false, nil
	}
	endOff := offset + uint64(len(dest))
	for currentOffset := offset; currentOffset < endOff; {
		// Find the row whose chunk covers currentOffset. blockIdx is
		// chunkOffset / BlockSize so the chunk's absolute Offset is
		// not directly stored on the row, but we can reconstruct
		// the chunk start by walking rows in order and tracking a
		// running expected offset. For the test fixtures + steady-
		// state production stream chunks land in offset-ascending
		// order, and the row ID's blockIdx component is monotone.
		row := findRowCoveringOffset(rows, currentOffset)
		if row == nil || row.fb.Hash.IsZero() {
			return false, nil
		}
		data, err := bs.local.Get(ctx, row.fb.Hash)
		if err != nil {
			if errors.Is(err, blockstore.ErrChunkNotFound) {
				return false, nil
			}
			return false, err
		}
		// Clamp the visible data to FileBlock.DataSize so a padded
		// on-disk chunk surface doesn't leak garbage past the
		// rollup-emitted byte count.
		dataLen := uint64(len(data))
		if uint64(row.fb.DataSize) > 0 && uint64(row.fb.DataSize) < dataLen {
			dataLen = uint64(row.fb.DataSize)
		}
		chunkAbsEnd := row.absOffset + dataLen
		if currentOffset >= chunkAbsEnd {
			// Should not happen if findRowCoveringOffset returned
			// a row covering currentOffset — surface as sparse and
			// let the caller fall back.
			return false, nil
		}
		srcOff := currentOffset - row.absOffset
		copyLen := chunkAbsEnd - currentOffset
		if copyLen > endOff-currentOffset {
			copyLen = endOff - currentOffset
		}
		copy(dest[currentOffset-offset:currentOffset-offset+copyLen], data[srcOff:srcOff+copyLen])
		currentOffset += copyLen
	}
	return true, nil
}

// rowWithOffset bundles a FileBlock row with the absolute payload
// offset of its first byte. The persister encodes the chunk's
// absolute offset directly as the numeric component of the row ID
// ("<payloadID>/<chunkOffset>"), so absOffset is the parsed
// component verbatim.
type rowWithOffset struct {
	fb        *blockstore.FileBlock
	absOffset uint64
}

// findRowCoveringOffset returns the row whose absolute byte range
// [absOffset, absOffset+DataSize) contains target, or nil if no row
// in rows covers it. The walk is O(N) over the per-payload row
// list — acceptable for the FastCDC steady-state (chunks average ~4 MiB
// so even a 4 GiB file produces ~1000 rows).
func findRowCoveringOffset(rows []*blockstore.FileBlock, target uint64) *rowWithOffset {
	for _, fb := range rows {
		if fb == nil {
			continue
		}
		abs, ok := blockstore.ParseChunkOffset(fb.ID)
		if !ok {
			continue
		}
		if target >= abs && target < abs+uint64(fb.DataSize) {
			return &rowWithOffset{fb: fb, absOffset: abs}
		}
	}
	return nil
}

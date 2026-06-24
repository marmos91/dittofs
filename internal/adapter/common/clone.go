package common

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ErrCloneCrossShare is returned when a CLONE/reflink is requested between two
// files that live in different shares (different per-share block stores).
// Content-addressed dedup is per-share, so the destination store could not
// reference the source's CAS blocks. Protocol adapters map this to the
// "cross-device link" status of their wire protocol (NFSv4 NFS4ERR_XDEV is
// absent, so NFS maps it to NFS4ERR_INVAL; SMB maps it to
// STATUS_NOT_SAME_DEVICE).
var ErrCloneCrossShare = errors.New("clone: source and destination are in different shares")

// CloneRange performs an NFSv4.2 CLONE / SMB duplicate-extents style reflink:
// it makes the destination's [dstOffset, dstOffset+count) byte range reference
// the same content as the source's [srcOffset, srcOffset+count) range.
//
// It is the single cross-protocol clone primitive — NFS CLONE calls it today
// and SMB FSCTL_DUPLICATE_EXTENTS_TO_FILE can adopt it without a second engine.
//
// Two paths, both atomic in a single metadata transaction:
//
//   - Whole-file fast path (srcOffset==0 && dstOffset==0 && count spans the
//     entire source, including the count==0 "to EOF" form): O(1), zero data
//     movement. The destination inherits the source's BlockRef list verbatim
//     and engine.CopyPayload bumps the CAS RefCount once per unique hash. This
//     is the canonical `cp --reflink` case.
//
//   - Sub-range path: the source range bytes are read and written into the
//     destination range. Storage stays deduplicated because the block store is
//     content-addressed (identical bytes hash to the same CAS object, so the
//     write re-references existing blocks rather than duplicating them) — the
//     same property SMB FSCTL_SRV_COPYCHUNK already relies on. The copy is
//     streamed in cloneCopyChunk-sized pieces to bound memory.
//
// blockStore MUST be the per-share engine.Store resolved for BOTH handles; the
// caller verifies src and dst share it (see ResolveForRead/ResolveForWrite) and
// passes ErrCloneCrossShare upward otherwise.
//
// count==0 means "from srcOffset to the end of the source file" (RFC 7862
// Section 15.13). The caller is responsible for stateid/permission checks and
// for rejecting directories / non-regular files before calling this helper.
func CloneRange(
	ctx context.Context,
	blockStore *engine.Store,
	metadataStore metadata.Store,
	cache CacheInvalidator,
	srcHandle, dstHandle metadata.FileHandle,
	srcPayloadID, dstPayloadID metadata.PayloadID,
	srcOffset, dstOffset, count uint64,
) error {
	srcFile, err := metadataStore.GetFile(ctx, srcHandle)
	if err != nil {
		return fmt.Errorf("CloneRange: fetch src file: %w", err)
	}

	// Resolve count==0 ("to EOF") and validate the source range fits.
	if srcOffset > srcFile.Size {
		return metadata.NewInvalidArgumentError(
			fmt.Sprintf("CloneRange: src offset %d beyond src size %d", srcOffset, srcFile.Size))
	}
	effCount := count
	if effCount == 0 {
		effCount = srcFile.Size - srcOffset
	} else if srcOffset+effCount > srcFile.Size {
		return metadata.NewInvalidArgumentError(
			fmt.Sprintf("CloneRange: src range [%d,%d) exceeds src size %d", srcOffset, srcOffset+effCount, srcFile.Size))
	}

	wholeFile := srcOffset == 0 && dstOffset == 0 && effCount == srcFile.Size

	err = metadataStore.WithTransaction(ctx, func(tx metadata.Transaction) error {
		// Bind the active txn into the context so the per-share coordinator's
		// RefCount UPDATEs (driven by engine.CopyPayload / engine.WriteAt) join
		// the same txn as the destination PutFile and commit/roll back together.
		txCtx := metadata.WithTx(ctx, tx)

		dstFile, err := tx.GetFile(ctx, dstHandle)
		if err != nil {
			return fmt.Errorf("fetch dst file: %w", err)
		}

		if wholeFile {
			// O(1) reflink: share the source's BlockRef list, bump RefCount per
			// unique hash. No bytes read or written.
			newBlocks, err := blockStore.CopyPayload(txCtx, string(srcPayloadID), string(dstPayloadID), srcFile.Blocks)
			if err != nil {
				return fmt.Errorf("engine copy payload: %w", err)
			}
			dstFile.Blocks = newBlocks
			dstFile.Size = srcFile.Size
		} else {
			// Sub-range: stream the source bytes into the destination range.
			// CAS dedup keeps storage O(1); the metadata BlockRef list is
			// rebuilt by successive engine.WriteAt calls.
			newBlocks, newSize, err := cloneCopyBytes(
				txCtx, blockStore, srcFile.Blocks, dstFile.Blocks,
				string(srcPayloadID), string(dstPayloadID),
				srcOffset, dstOffset, effCount,
			)
			if err != nil {
				return err
			}
			dstFile.Blocks = newBlocks
			if newSize > dstFile.Size {
				dstFile.Size = newSize
			}
		}

		dstFile.Mtime = time.Now()
		dstFile.Ctime = dstFile.Mtime // content change is also a metadata change
		if err := tx.PutFile(ctx, dstFile); err != nil {
			return fmt.Errorf("persist dst file: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	// POST-txn: the destination content changed wholesale; drop its cache
	// entries. Files that still reference the shared CAS hashes via dedup keep
	// their entries warm (nil removedHashes => key off dstPayloadID only).
	if cache != nil {
		cache.InvalidateFile(dstPayloadID, nil)
	}
	return nil
}

// cloneCopyChunk bounds the per-iteration buffer for the sub-range clone copy.
const cloneCopyChunk = 1 << 20 // 1 MiB

// cloneCopyBytes streams [srcOffset, srcOffset+count) from the source payload
// into [dstOffset, dstOffset+count) of the destination payload, returning the
// destination's rebuilt BlockRef list and the highest byte offset written. The
// content-addressed store deduplicates identical bytes, so this re-references
// existing CAS blocks rather than duplicating data.
func cloneCopyBytes(
	ctx context.Context,
	blockStore *engine.Store,
	srcBlocks, dstBlocks []block.BlockRef,
	srcPayloadID, dstPayloadID string,
	srcOffset, dstOffset, count uint64,
) ([]block.BlockRef, uint64, error) {
	buf := make([]byte, cloneCopyChunk)
	blocks := dstBlocks
	var written uint64
	for written < count {
		chunk := count - written
		if chunk > cloneCopyChunk {
			chunk = cloneCopyChunk
		}
		data := buf[:chunk]
		n, err := blockStore.ReadAt(ctx, srcPayloadID, srcBlocks, data, srcOffset+written)
		if err != nil {
			return nil, 0, fmt.Errorf("CloneRange: read src: %w", err)
		}
		if uint64(n) < chunk {
			// Source truncated concurrently between size check and read.
			return nil, 0, metadata.NewInvalidArgumentError(
				fmt.Sprintf("CloneRange: short read (%d < %d)", n, chunk))
		}
		newBlocks, err := blockStore.WriteAt(ctx, dstPayloadID, blocks, data, dstOffset+written)
		if err != nil {
			return nil, 0, fmt.Errorf("CloneRange: write dst: %w", err)
		}
		blocks = newBlocks
		written += chunk
	}
	return blocks, dstOffset + count, nil
}

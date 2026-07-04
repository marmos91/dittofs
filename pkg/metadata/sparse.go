package metadata

import (
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
)

// sparse.go holds the metadata side of the NFSv4.2 sparse-file operations
// (RFC 7862): DEALLOCATE (PunchHole) and ALLOCATE (Allocate). Both mutate the
// authoritative content-addressed block list (FileAttr.Blocks) and/or the
// logical size, then persist atomically. The block-store reclaim of the
// physical bytes a DEALLOCATE frees is driven by the protocol adapter using the
// pre-op block snapshot these methods return, mirroring the truncate-reclaim
// split already used by SETATTR (CLAUDE.md invariants #1/#5: handlers
// coordinate, the store owns the metadata mutation).

// SparseResult reports the outcome of a PunchHole / Allocate so the adapter can
// reclaim block-store space for the freed range and update WCC/quota state.
type SparseResult struct {
	// File is the file as it stands after the mutation (post-op attrs).
	File *File
	// PreOpBlocks is the file's block list BEFORE the mutation. The adapter
	// passes this to BlockStore.Truncate so dropped/clipped tail chunks have
	// their dedup refcounts decremented and remote bytes reclaimed.
	PreOpBlocks []block.ChunkRef
	// PayloadID identifies the block-store payload for the reclaim call. Empty
	// when the file has no backing payload (nothing to reclaim).
	PayloadID PayloadID
	// ReclaimFrom is the lowest offset whose backing bytes may now be
	// unreferenced; the adapter reclaims storage at/after this offset.
	ReclaimFrom uint64
}

// PunchHole implements DEALLOCATE (RFC 7862 Section 15.4): it marks the byte
// range [offset, offset+length) of a regular file as a hole. Block refs lying
// entirely within the range are dropped from the hole map (block.PunchHole);
// the file's logical size is unchanged. Punching at or beyond EOF, or a zero
// length, is a no-op success. The returned SparseResult.PreOpBlocks lets the
// caller (the block store) reap the freed chunks and zero-overwrite the exact
// punched range so it reads back as zeros — including any sub-range still
// covered by a partially-overlapping block that PunchHole intentionally keeps
// to preserve that block's out-of-range data.
//
// Requires write permission. Returns ErrIsDirectory for non-regular files and
// ErrInvalidArgument when offset+length overflows uint64.
func (s *Service) PunchHole(ctx *AuthContext, handle FileHandle, offset, length uint64) (*SparseResult, error) {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return nil, err
	}

	// Commit any pending write metadata first so file.Blocks/Size reflect the
	// latest state before we mutate the block list.
	if _, err := s.FlushPendingWriteForFile(ctx, handle); err != nil {
		return nil, err
	}

	file, err := store.GetFile(ctx.Context, handle)
	if err != nil {
		return nil, err
	}
	if file.Type != FileTypeRegular {
		return nil, &StoreError{Code: ErrIsDirectory, Message: "DEALLOCATE on non-regular file", Path: file.Path}
	}
	if rangeOverflows(offset, length) {
		return nil, &StoreError{Code: ErrInvalidArgument, Message: "DEALLOCATE range overflow", Path: file.Path}
	}
	if err := s.checkWritePermission(ctx, handle); err != nil {
		return nil, err
	}

	res := &SparseResult{File: file, PayloadID: file.PayloadID, ReclaimFrom: offset}

	// No-op cases: zero length, nothing past offset to free, or no block list.
	if length == 0 || offset >= file.Size || len(file.Blocks) == 0 {
		return res, nil
	}

	res.PreOpBlocks = file.Blocks
	newBlocks := block.PunchHole(file.Blocks, offset, length)
	file.Blocks = newBlocks
	if !file.ObjectID.IsZero() {
		if len(newBlocks) == 0 {
			file.ObjectID = block.ObjectID{}
		} else {
			file.ObjectID = block.ComputeObjectID(newBlocks)
		}
	}

	now := time.Now()
	file.Mtime = now
	file.Ctime = now

	if err := store.PutFile(ctx.Context, file); err != nil {
		return nil, err
	}

	logger.Debug("metadata DEALLOCATE (punch hole)",
		"path", file.Path, "offset", offset, "length", length,
		"blocks_before", len(res.PreOpBlocks), "blocks_after", len(newBlocks))

	return res, nil
}

// Allocate implements ALLOCATE (RFC 7862 Section 15.1): it guarantees the byte
// range [offset, offset+length) is readable, extending the file's logical size
// to offset+length when that is larger than the current size. DittoFS is
// thin-provisioned over a content-addressed/dedup block store (and optionally
// S3), so ALLOCATE does NOT physically reserve space — it provides
// best-effort/logical preallocation: the range reads back as zeros (sparse
// hole) until written, and the size grows so subsequent reads see the extent.
// This matches the RFC, which permits a server to return success without a true
// physical reservation. A range that lies entirely within the current size is a
// success no-op (the file already covers it).
//
// Requires write permission. Returns ErrIsDirectory for non-regular files and
// ErrInvalidArgument when offset+length overflows uint64.
func (s *Service) Allocate(ctx *AuthContext, handle FileHandle, offset, length uint64) (*File, error) {
	store, err := s.storeForHandle(handle)
	if err != nil {
		return nil, err
	}

	if _, err := s.FlushPendingWriteForFile(ctx, handle); err != nil {
		return nil, err
	}

	file, err := store.GetFile(ctx.Context, handle)
	if err != nil {
		return nil, err
	}
	if file.Type != FileTypeRegular {
		return nil, &StoreError{Code: ErrIsDirectory, Message: "ALLOCATE on non-regular file", Path: file.Path}
	}
	if rangeOverflows(offset, length) {
		return nil, &StoreError{Code: ErrInvalidArgument, Message: "ALLOCATE range overflow", Path: file.Path}
	}
	if err := s.checkWritePermission(ctx, handle); err != nil {
		return nil, err
	}

	newEnd := offset + length
	if length == 0 || newEnd <= file.Size {
		// Range already within the file: nothing to grow. The bytes are already
		// readable (data or hole), satisfying the allocation guarantee.
		return file, nil
	}

	// Grow the logical size. The newly covered [oldSize, newEnd) region has no
	// block refs, so it is a hole that reads as zeros — exactly the allocation
	// contract for a thin-provisioned store. No block-store write is performed.
	file.Size = newEnd
	now := time.Now()
	file.Mtime = now
	file.Ctime = now

	if err := store.PutFile(ctx.Context, file); err != nil {
		return nil, err
	}

	logger.Debug("metadata ALLOCATE (logical preallocation)",
		"path", file.Path, "offset", offset, "length", length, "new_size", newEnd)

	return file, nil
}

// rangeOverflows reports whether the byte range [offset, offset+length) would
// overflow uint64 (a malformed ALLOCATE/DEALLOCATE range). A zero length never
// overflows.
func rangeOverflows(offset, length uint64) bool {
	return length > 0 && offset > ^uint64(0)-length
}

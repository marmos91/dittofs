package common

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ErrCloneCrossShare is returned when a CLONE/reflink is requested between two
// files that live in different shares (different per-share block stores).
// Content-addressed dedup is per-share, so the destination store could not
// reference the source's CAS blocks. Protocol adapters map this to the
// "cross-device" status of their wire protocol (NFSv4 has no NFS4ERR_XDEV, so
// NFS maps it to NFS4ERR_INVAL; SMB would map it to STATUS_NOT_SAME_DEVICE).
var ErrCloneCrossShare = errors.New("clone: source and destination are in different shares")

// CloneWholeFile performs a whole-file NFSv4.2 CLONE / SMB duplicate-extents
// reflink: the destination inherits the source's entire content by referencing
// the same content-addressed blocks. It is O(1) — engine.CopyPayload bumps the
// CAS RefCount once per unique source hash, no data is read or written, even on
// S3. This is the canonical `cp --reflink` case and the single cross-protocol
// clone primitive (SMB FSCTL_DUPLICATE_EXTENTS_TO_FILE can adopt it without a
// second engine).
//
// Copy-on-write is intrinsic to the content-addressed store: a later WRITE to
// either file produces new CAS blocks under a new hash, leaving the other side
// untouched.
//
// Everything is atomic in one metadata transaction:
//   - engine.CopyPayload's per-hash IncrementRefCount UPDATEs are bound to the
//     txn (via metadata.WithTx) so they commit/roll back together with the
//     destination PutFile. On any error nothing is committed — no partial
//     dstFileAttr, no leaked RefCount bumps.
//   - cache.InvalidateFile (if cache != nil) runs POST-txn, after the commit.
//
// blockStore and metadataStore MUST be the per-share stores resolved for the
// destination handle; the caller verifies src and dst share them and surfaces
// ErrCloneCrossShare otherwise. The caller is also responsible for
// stateid/permission checks and for rejecting non-regular files.
func CloneWholeFile(
	ctx context.Context,
	blockStore *engine.Store,
	metadataStore metadata.Store,
	cache CacheInvalidator,
	srcHandle, dstHandle metadata.FileHandle,
	srcPayloadID, dstPayloadID metadata.PayloadID,
) error {
	srcFile, err := metadataStore.GetFile(ctx, srcHandle)
	if err != nil {
		return fmt.Errorf("CloneWholeFile: fetch src file: %w", err)
	}

	err = metadataStore.WithTransaction(ctx, func(tx metadata.Transaction) error {
		// Bind the active txn into the context so the per-share coordinator's
		// RefCount UPDATEs (driven by engine.CopyPayload) join the same txn as
		// the destination PutFile and commit/roll back together.
		txCtx := metadata.WithTx(ctx, tx)

		dstFile, err := tx.GetFile(ctx, dstHandle)
		if err != nil {
			return fmt.Errorf("fetch dst file: %w", err)
		}

		newBlocks, err := blockStore.CopyPayload(txCtx, string(srcPayloadID), string(dstPayloadID), srcFile.Blocks)
		if err != nil {
			return fmt.Errorf("engine copy payload: %w", err)
		}
		dstFile.Blocks = newBlocks
		dstFile.Size = srcFile.Size
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

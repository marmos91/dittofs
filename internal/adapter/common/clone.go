package common

import (
	"context"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/metadata"
)

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
// CLONE copies the source's CAS block manifest (FileAttr.Blocks). A freshly
// written source whose bytes are still in the append log / in-memory buffer has
// an empty or partial manifest — the rollup into CAS is asynchronous — so this
// helper first calls blockStore.DrainRollups to force every dirty payload into
// CAS and persist its FileAttr.Blocks, then re-reads the source's manifest
// INSIDE the txn. Without the drain the clone would reference no blocks and read
// back as zeros: silent data loss when cloning un-rolled-up data (the CLONE twin
// of #1481). Re-reading the source post-drain (rather than trusting a manifest
// the caller fetched before the drain) closes the TOCTOU where the copy would
// otherwise capture the stale, pre-rollup empty manifest.
//
// blockStore and metadataStore MUST be the per-share stores resolved for the
// destination handle; the caller is responsible for confirming src and dst live
// in the same share and for stateid/permission/type checks.
func CloneWholeFile(
	ctx context.Context,
	blockStore *engine.Store,
	metadataStore metadata.Store,
	cache CacheInvalidator,
	srcHandle, dstHandle metadata.FileHandle,
	dstPayloadID metadata.PayloadID,
) error {
	// Force the source's pending writes into CAS + the FileChunk manifest before
	// we copy it. DrainRollups bypasses the stabilization window and persists
	// FileAttr.Blocks, so the post-drain GetFile below observes the complete
	// manifest rather than an empty/partial one.
	if err := blockStore.DrainRollups(ctx); err != nil {
		return fmt.Errorf("drain source rollups: %w", err)
	}

	selfClone := false
	err := metadataStore.WithTransaction(ctx, func(tx metadata.Transaction) error {
		// Bind the active txn into the context so the per-share coordinator's
		// RefCount UPDATEs (driven by engine.CopyPayload) join the same txn as
		// the destination PutFile and commit/roll back together.
		txCtx := metadata.WithTx(ctx, tx)

		// Re-read the source INSIDE the txn, AFTER the drain, so the copy uses the
		// freshly populated manifest — never a stale pre-rollup one.
		srcFile, err := tx.GetFile(ctx, srcHandle)
		if err != nil {
			return fmt.Errorf("fetch src file: %w", err)
		}

		// Self-clone (source and destination share a payload) is a no-op: cloning
		// a payload onto itself would IncrementRefCount on hashes the same payload
		// already owns, inflating the count with no offsetting reference. The
		// caller should reject this earlier, but guard here too — this helper is
		// the shared cross-protocol primitive and must stay safe on its own. The
		// destination content is unchanged, so the post-txn cache invalidation is
		// skipped too.
		if srcFile.PayloadID == dstPayloadID {
			selfClone = true
			return nil
		}

		dstFile, err := tx.GetFile(ctx, dstHandle)
		if err != nil {
			return fmt.Errorf("fetch dst file: %w", err)
		}

		newBlocks, err := blockStore.CopyPayload(txCtx, string(srcFile.PayloadID), string(dstPayloadID), srcFile.Blocks)
		if err != nil {
			return fmt.Errorf("engine copy payload: %w", err)
		}
		dstFile.Blocks = newBlocks
		// Wholesale manifest replacement on the destination — persist refs.
		dstFile.BlocksDirty = true
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
	if cache != nil && !selfClone {
		cache.InvalidateFile(dstPayloadID, nil)
	}
	return nil
}

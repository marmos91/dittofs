package common

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
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
//     destination UpdateAttrs. On any error nothing is committed — no partial
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

	// Local-only shares have no remote content-addressed tier to hydrate from,
	// and the destination payload owns no journal intervals of its own, so a
	// manifest-only reflink (engine.CopyPayload) would leave the destination
	// pointing at hashes whose bytes live only in the SOURCE's journal — a read
	// of the clone then finds no destination interval and zero-fills. Materialize
	// real bytes into the destination's own journal instead. Remote shares keep
	// the O(1) reflink below (their shared blocks are cold-hydratable).
	if !blockStore.HasRemoteStore() {
		return materializeLocalClone(ctx, blockStore, metadataStore, cache, srcHandle, dstHandle, dstPayloadID)
	}

	selfClone := false
	var copied []block.ChunkRef
	err := metadataStore.WithTransaction(ctx, func(tx metadata.Transaction) error {
		// Bind the active txn into the context so the per-share coordinator's
		// RefCount UPDATEs (driven by engine.CopyPayload) join the same txn as
		// the destination UpdateAttrs and commit/roll back together.
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
		copied = newBlocks
		// Wholesale manifest replacement on the destination — persist refs.
		dstFile.Size = srcFile.Size
		dstFile.Mtime = time.Now()
		dstFile.Ctime = dstFile.Mtime // content change is also a metadata change
		if err := tx.SetManifest(ctx, dstFile); err != nil {
			return fmt.Errorf("persist dst file: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	// A self-clone left the destination exactly as it was: it holds the content
	// its manifest describes, it inherited no ranges the local tier has to
	// account for, and its cached entries still describe what it holds.
	// Everything below is about a destination whose content changed.
	if selfClone {
		return nil
	}
	// Order matters between these two. The discard takes the destination's
	// replaced ranges back to holes; the seed then gives the local tier its
	// account of the ranges the copy put there. Seeding first would find those
	// ranges already described and skip them, leaving the tier holding the
	// content the copy replaced.
	if err := discardStaleDestination(ctx, blockStore, dstPayloadID); err != nil {
		return err
	}
	seedClonedRanges(ctx, blockStore, dstPayloadID, copied)

	// POST-txn: the destination content changed wholesale; drop its cache
	// entries. Files that still reference the shared CAS hashes via dedup keep
	// their entries warm (nil removedHashes => key off dstPayloadID only).
	if cache != nil {
		cache.InvalidateFile(dstPayloadID, nil)
	}
	return nil
}

// discardStaleDestination drops the local tier's account of a copy's
// destination. Both reflink helpers in this package call it, post-commit, on
// every destination whose content the copy replaced.
//
// The copy rewrites the destination's manifest wholesale and moves no byte, so
// every range the destination still holds locally describes content it no
// longer has. Those ranges are what the read path resolves first — a covered
// warm range reports neither hole nor cold, so the read never reaches the new
// manifest — and the destination serves its pre-copy content indefinitely with
// nothing logged. Dropping them puts the copied span back in the state a fresh
// destination is already in, where the manifest is what answers.
//
// It runs after the commit, never before: until the commit lands those bytes
// are the destination's real content, and a rolled-back copy that had already
// dropped them could not get them back. What is left is the window between the
// two, where a read still serves the replaced content.
//
// A failure fails the copy, which has already committed. The alternative is
// reporting success on a destination whose every read serves the content the
// copy replaced, and a caller that retries a copy it was told failed re-runs an
// operation that lands on the same manifest and gets another chance at this.
func discardStaleDestination(ctx context.Context, blockStore *engine.Store, dstPayloadID metadata.PayloadID) error {
	if err := blockStore.DiscardLocalContent(ctx, string(dstPayloadID)); err != nil {
		return fmt.Errorf("discard the copy destination's replaced local ranges: %w", err)
	}
	return nil
}

// seedClonedRanges tells the destination's local tier which ranges the copy just
// gave it. Both reflink helpers in this package call it. The copy moves no
// bytes, so the destination's ranges land in the
// manifest and nowhere in the index, and everything that reads residency off the
// index — the remote-only byte count, the offline-readiness answer — cannot
// account for them until this runs.
//
// It runs after the commit, never before: cold intervals over rows a rolled-back
// copy never wrote would make a read of the destination's sparse ranges fail
// closed where it should serve zeros.
//
// A failure is logged and the copy still succeeds. The copy is durable and its
// reads are correct either way — a hole on a remote-backed share hydrates from
// the manifest exactly as a cold range does — so the only casualty is the
// accounting, and failing a committed copy to report it would be the worse
// trade. The next seed of the same ranges picks them up.
func seedClonedRanges(ctx context.Context, blockStore *engine.Store, dstPayloadID metadata.PayloadID, blocks []block.ChunkRef) {
	if len(blocks) == 0 {
		return
	}
	if err := blockStore.SeedColdRefs(ctx, string(dstPayloadID), blocks); err != nil {
		logger.Warn("server-side copy: the destination's ranges are not recorded in the local tier; "+
			"reads are unaffected, its remote-only bytes are not counted",
			"payload", dstPayloadID, "error", err)
	}
}

// materializeLocalClone is the local-only (no remote store) clone path: it
// copies the source payload's bytes into the destination payload's OWN journal
// via the normal write path, then lets the normal carve build the destination's
// FileChunk manifest and derived File.Blocks. The reflink path cannot serve a
// local-only clone because the destination would carry manifest rows whose bytes
// live only in the source's journal, with no destination interval to read — a
// read of the clone would zero-fill.
//
// Cost: O(n) work AND O(n) local storage. The journal append-log local tier does
// not content-dedup, so this is a real byte copy, not a reflink — a strict
// improvement over silently reading zeros, but not O(1). Remote-backed shares
// keep the O(1) reflink (see the caller). This path touches no CAS RefCount, so
// there is no refcount-under-GC concern for it.
//
// It runs ENTIRELY OUTSIDE any metadata transaction: WriteAt / Flush /
// DrainRollups persist destination rows through the per-share coordinator under
// a non-reentrant metadata lock, so nesting them inside a held transaction would
// self-deadlock. Only the trailing size/mtime stamp opens its own short txn,
// after the block writes have committed.
func materializeLocalClone(
	ctx context.Context,
	blockStore *engine.Store,
	metadataStore metadata.Store,
	cache CacheInvalidator,
	srcHandle, dstHandle metadata.FileHandle,
	dstPayloadID metadata.PayloadID,
) error {
	// Re-read the source AFTER the caller's DrainRollups so Size and the source
	// journal intervals reflect the fully-materialized post-rollup view.
	srcFile, err := metadataStore.GetFile(ctx, srcHandle)
	if err != nil {
		return fmt.Errorf("materialize clone: fetch src file: %w", err)
	}

	// Self-clone (source and destination share a payload) is a no-op: the
	// destination content is unchanged, so skip the copy and the cache drop.
	if srcFile.PayloadID == dstPayloadID {
		return nil
	}

	// Copy the source bytes into the destination payload's own journal, chunked
	// to bound the transient buffer. ReadAt resolves the source's local journal
	// intervals (zero-filling any sparse hole); WriteAt appends real intervals to
	// the destination journal so a later read of the destination resolves bytes
	// rather than a hole.
	const copyChunk = 1 << 20 // 1 MiB
	buf := make([]byte, copyChunk)
	for off := uint64(0); off < srcFile.Size; {
		want := srcFile.Size - off
		if want > copyChunk {
			want = copyChunk
		}
		n, rerr := blockStore.ReadAt(ctx, string(srcFile.PayloadID), buf[:want], off)
		if n > 0 {
			if _, werr := blockStore.WriteAt(ctx, string(dstPayloadID), nil, buf[:n], off); werr != nil {
				return fmt.Errorf("materialize clone: write dst payload: %w", werr)
			}
			off += uint64(n)
		}
		if rerr != nil {
			if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
				break
			}
			return fmt.Errorf("materialize clone: read src payload: %w", rerr)
		}
		if n == 0 {
			break
		}
	}

	// Durability barrier + carve: Flush fsyncs the destination's appended
	// records; DrainRollups seals them into the destination's FileChunk manifest
	// and derived File.Blocks — the same path a normal write + commit drives.
	if _, err := blockStore.Flush(ctx, string(dstPayloadID)); err != nil {
		return fmt.Errorf("materialize clone: flush dst payload: %w", err)
	}
	if err := blockStore.DrainRollups(ctx); err != nil {
		return fmt.Errorf("materialize clone: drain dst rollups: %w", err)
	}

	// Block-level WriteAt does not set File.Size / Mtime / Ctime — stamp them on
	// the destination to match the source and record the content change. The
	// carve above already populated File.Blocks, so re-read inside the txn and
	// leave it intact.
	err = metadataStore.WithTransaction(ctx, func(tx metadata.Transaction) error {
		dstFile, err := tx.GetFile(ctx, dstHandle)
		if err != nil {
			return fmt.Errorf("fetch dst file: %w", err)
		}
		dstFile.Size = srcFile.Size
		dstFile.Mtime = time.Now()
		dstFile.Ctime = dstFile.Mtime // content change is also a metadata change
		if err := tx.UpdateAttrs(ctx, dstFile); err != nil {
			return fmt.Errorf("persist dst file: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	// POST-txn: the destination content changed wholesale; drop its cache entries.
	if cache != nil {
		cache.InvalidateFile(dstPayloadID, nil)
	}
	return nil
}

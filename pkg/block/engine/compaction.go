// Package engine — GC compaction of partially-dead blocks (#1487).
//
// Delete-only block GC (gc_block.go) frees a block object only once its LAST
// live chunk dies. A block that keeps a few live chunks but has accumulated
// many dead ones pins the dead chunks' bytes on the remote forever. Compaction
// closes that gap: it copies a mostly-dead block's still-live chunks into a
// fresh block, rewrites their locators to point at the new block, and deletes
// the old block object + record — reclaiming the dead bytes.
//
// # Liveness signal (no live-set rebuild, no schema change)
//
// Compaction runs AFTER the GC sweep, under the same per-remote lock. By then
// the sweep has already cleared the synced marker of every past-grace dead
// chunk (DeleteSynced). So a chunk resident in block B is "still live here"
// iff GetLocator(hash) is synced and points at B; a chunk that lost its marker
// (swept dead) or whose locator moved elsewhere is dropped. Chunks still within
// grace keep their marker and are carried forward conservatively — never a
// premature drop. This is exactly the keep/delete decision the sweep just made,
// so compaction never reclaims a chunk the sweep would have spared.
//
// # Candidate selection
//
// Byte-based live ratio: sum of the WireLength of every live locator pointing
// at a block, divided by the block's object Length. Below the configured
// threshold the block is a candidate. Computed from GetLocator + the block
// record alone — no per-block GET and no extra stored field.
//
// # Crash safety & idempotency (identical to the live carver / migration)
//
//  1. PutBlock(new) — a crash here leaves an orphan remote object with no
//     record (reconcile class 3), reclaimed by ReclaimOrphanObjects. Live
//     chunks still resolve to the intact old block. No data loss.
//  2. DefaultCommitBlock — one transaction writes the new record and overwrites
//     each moved chunk's locator (last-wins DeleteSynced→MarkSynced) to the new
//     block. All-or-nothing.
//  3. DeleteBlock(old) then DeleteBlockRecord(old) — a crash between leaves the
//     old block as a class-2 "leaked" record (no live locator points at it,
//     since all moved), reclaimed by ReclaimRecords.
//
// A re-run converges: moved chunks' locators no longer point at the old block,
// so compactOne finds nothing to move and simply deletes the husk. The whole
// old block is BLAKE3-verified against its record before any byte is trusted.
//
// Concurrency: the caller MUST hold the per-remote GC lock (as the sweep and
// reclaim do) so compaction cannot race a concurrent sweep's DecrLiveChunkCount
// or DeleteBlock on the same block. An in-flight reader that resolved a locator
// to the old block before step 2 and issues its GET after step 3 sees a 404 —
// the same narrow window the delete-only reclaim already has for its blocks;
// acceptable for an opt-in maintenance pass.
package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/blockcodec"
	"github.com/marmos91/dittofs/pkg/block/remote"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// CompactMetaView is the per-share metadata surface compaction needs: the
// read-only reconcile view (EnumerateSynced + GetLocator + WalkBlockRecords)
// plus record read/delete and a Transactor for DefaultCommitBlock's atomic
// record+locator rewrite. metadata.Store satisfies it structurally.
type CompactMetaView interface {
	ReconcileMetaView
	metadata.Transactor
	GetBlockRecord(ctx context.Context, blockID string) (block.BlockRecord, bool, error)
	DeleteBlockRecord(ctx context.Context, blockID string) error
}

// CompactOptions parameterizes a compaction pass.
type CompactOptions struct {
	// LiveRatio is the threshold below which a block is compacted: a block
	// whose live bytes / object length is < LiveRatio is a candidate. Must be
	// in (0, 1]; a non-positive value disables compaction (the pass no-ops).
	LiveRatio float64
	// DryRun reports candidates without moving or deleting anything.
	DryRun bool
}

// CompactReport is the output of a compaction pass.
type CompactReport struct {
	BlocksScanned   int64 `json:"blocks_scanned"`
	BlocksCompacted int64 `json:"blocks_compacted"`
	ChunksMoved     int64 `json:"chunks_moved"`
	// BytesReclaimed is the sum of (old block length − new block length) across
	// compacted blocks; for a husk (all chunks already dead/moved) it is the
	// whole old block length.
	BytesReclaimed int64 `json:"bytes_reclaimed"`
	Errors         int64 `json:"errors"`
	DryRun         bool  `json:"dry_run"`
}

// Merge folds other into r, for aggregating per-remote passes.
func (r *CompactReport) Merge(other CompactReport) {
	r.BlocksScanned += other.BlocksScanned
	r.BlocksCompacted += other.BlocksCompacted
	r.ChunksMoved += other.ChunksMoved
	r.BytesReclaimed += other.BytesReclaimed
	r.Errors += other.Errors
	if other.DryRun {
		r.DryRun = true
	}
}

// CompactBlocks repacks partially-dead blocks on one remote-store scope. views
// are the per-share metadata views sharing the remote block store rbs. A block
// is per-share (block IDs are minted per share), so each view is compacted
// against its own live locator set. Returns a non-nil error only on an
// enumeration/walk failure; per-block failures are counted in Report.Errors and
// leave that block intact for the next run.
//
// The caller MUST hold the per-remote GC lock and run this AFTER the sweep.
func CompactBlocks(
	ctx context.Context,
	views []CompactMetaView,
	rbs remote.RemoteBlockStore,
	opts CompactOptions,
) (CompactReport, error) {
	report := CompactReport{DryRun: opts.DryRun}
	if rbs == nil || opts.LiveRatio <= 0 {
		return report, nil // remote cannot hold blocks, or compaction disabled
	}

	for _, v := range views {
		// Live bytes per block: sum the WireLength of every live synced locator.
		// Drain EnumerateSynced THEN resolve locators — the sqlite pool is
		// MaxOpenConns(1), so GetLocator inside the enumerate callback (cursor
		// still open) would deadlock waiting for a second connection.
		var synced []block.ContentHash
		if err := v.EnumerateSynced(ctx, func(h block.ContentHash, _ time.Time) error {
			synced = append(synced, h)
			return nil
		}); err != nil {
			return report, fmt.Errorf("compaction: enumerate synced: %w", err)
		}
		liveBytes := make(map[string]int64)
		for _, h := range synced {
			if err := ctx.Err(); err != nil {
				return report, err
			}
			loc, ok, err := v.GetLocator(ctx, h)
			if err != nil {
				return report, fmt.Errorf("compaction: get locator: %w", err)
			}
			if ok && loc.BlockID != "" {
				liveBytes[loc.BlockID] += loc.WireLength
			}
		}

		// Collect candidate block IDs first — never GET/commit/delete while the
		// WalkBlockRecords cursor is open (sqlite single-connection rule).
		var candidates []string
		if err := v.WalkBlockRecords(ctx, func(rec block.BlockRecord) error {
			report.BlocksScanned++
			lb := liveBytes[rec.BlockID]
			if lb > 0 && rec.Length > 0 && float64(lb)/float64(rec.Length) < opts.LiveRatio {
				candidates = append(candidates, rec.BlockID)
			}
			return nil
		}); err != nil {
			return report, fmt.Errorf("compaction: walk block records: %w", err)
		}

		for _, blockID := range candidates {
			if err := ctx.Err(); err != nil {
				return report, err
			}
			compactOneBlock(ctx, v, rbs, blockID, opts, &report)
		}
	}
	return report, nil
}

// compactOneBlock compacts a single candidate block. Errors are logged and
// counted in report.Errors, leaving the block intact for the next run.
func compactOneBlock(
	ctx context.Context,
	v CompactMetaView,
	rbs remote.RemoteBlockStore,
	blockID string,
	opts CompactOptions,
	report *CompactReport,
) {
	rec, ok, err := v.GetBlockRecord(ctx, blockID)
	if err != nil {
		slog.Warn("compaction: get block record failed — skipping", "block_id", blockID, "err", err)
		report.Errors++
		return
	}
	if !ok {
		return // already gone (raced a concurrent reclaim in the same run)
	}

	data, err := rbs.GetBlock(ctx, blockID)
	if err != nil {
		if errors.Is(err, block.ErrChunkNotFound) {
			// Object gone but record survives (a prior compaction's DeleteBlock
			// landed, DeleteBlockRecord did not): finish the cleanup. The moved
			// chunks already point at the new block; the husk record is safe to
			// drop whatever its stale count says (block IDs are never reused).
			if opts.DryRun {
				return
			}
			if derr := v.DeleteBlockRecord(ctx, blockID); derr != nil {
				slog.Warn("compaction: delete husk record failed — will retry next run", "block_id", blockID, "err", derr)
				report.Errors++
			}
			return
		}
		slog.Warn("compaction: get block failed — skipping", "block_id", blockID, "err", err)
		report.Errors++
		return
	}

	// Whole-block BLAKE3 verify before trusting any byte (respects integrity;
	// the wire bodies are copied verbatim so per-chunk hashes are preserved).
	if block.ContentHash(blake3.Sum256(data)) != rec.BlockHash {
		slog.Error("compaction: block hash mismatch — leaving for the scrubber", "block_id", blockID)
		report.Errors++
		return
	}

	_, records, err := blockcodec.Parse(data, nil)
	if err != nil {
		slog.Warn("compaction: parse block failed — skipping", "block_id", blockID, "err", err)
		report.Errors++
		return
	}

	// Select the chunks still live in THIS block: a synced locator that still
	// points here. Dropped: dead chunks (marker cleared by the sweep) and any
	// already moved to another block (locator points elsewhere — re-run).
	type moved struct {
		hash block.ContentHash
		wire []byte
	}
	moveable := make([]moved, 0, len(records))
	for _, r := range records {
		loc, ok, err := v.GetLocator(ctx, r.Hash)
		if err != nil {
			slog.Warn("compaction: get locator failed — skipping block", "block_id", blockID, "hash", r.Hash.String(), "err", err)
			report.Errors++
			return
		}
		if ok && loc.BlockID == blockID {
			moveable = append(moveable, moved{hash: r.Hash, wire: data[r.WireOffset : r.WireOffset+r.WireLength]})
		}
	}

	if len(moveable) == len(records) {
		return // nothing dead to reclaim (all chunks still live here) — leave it
	}

	if opts.DryRun {
		report.BlocksCompacted++
		return
	}

	if len(moveable) == 0 {
		// Husk: every chunk is dead or already moved. Reclaim the whole object.
		if deleteOldBlock(ctx, v, rbs, blockID, report) {
			report.BlocksCompacted++
			report.BytesReclaimed += rec.Length
		}
		return
	}

	// Pack the live chunks into a fresh block (verbatim wire bodies; the codec
	// re-frames the record headers with the new block ID). nil Sealer matches
	// the carver/migration — per-chunk encryption already lives in the body.
	newID, err := newBlockID()
	if err != nil {
		slog.Warn("compaction: new block id failed — skipping", "block_id", blockID, "err", err)
		report.Errors++
		return
	}
	var buf bytes.Buffer
	builder, err := blockcodec.NewBuilder(&buf, newID, nil)
	if err != nil {
		slog.Warn("compaction: new builder failed — skipping", "block_id", blockID, "err", err)
		report.Errors++
		return
	}
	commits := make([]block.BlockChunkCommit, 0, len(moveable))
	for _, mv := range moveable {
		loc, err := builder.Add(mv.hash, mv.wire)
		if err != nil {
			slog.Warn("compaction: frame chunk failed — skipping block", "block_id", blockID, "hash", mv.hash.String(), "err", err)
			report.Errors++
			return
		}
		// Local left zero: the bytes are read from remote and the chunk's local
		// index entry (if any) already points at its log blob and is unchanged.
		commits = append(commits, block.BlockChunkCommit{Hash: mv.hash, Remote: loc})
	}
	if _, err := builder.Finish(); err != nil {
		slog.Warn("compaction: finish block failed — skipping", "block_id", blockID, "err", err)
		report.Errors++
		return
	}
	newBytes := buf.Bytes()

	// (1) PutBlock — orphan object on crash (reconcile class 3), never data loss.
	if err := rbs.PutBlock(ctx, newID, bytes.NewReader(newBytes)); err != nil {
		slog.Warn("compaction: put new block failed — skipping", "block_id", blockID, "new_block_id", newID, "err", err)
		report.Errors++
		return
	}
	// (2) Atomic record + last-wins locator rewrite onto the new block.
	newRec := block.BlockRecord{
		BlockID:        newID,
		BlockHash:      block.ContentHash(blake3.Sum256(newBytes)),
		Length:         int64(len(newBytes)),
		LiveChunkCount: uint32(len(commits)),
		SyncState:      block.BlockStateRemote,
	}
	if err := metadata.DefaultCommitBlock(ctx, v, newRec, commits); err != nil {
		// New block is an orphan object (no record) — reconcile class 3; the old
		// block is untouched and still resolves. Safe to abandon this attempt.
		slog.Warn("compaction: commit new block failed — old block kept, new object orphaned for reconcile", "block_id", blockID, "new_block_id", newID, "err", err)
		report.Errors++
		return
	}
	// (3) Delete the old block: object then record (matches reclaim order — a
	// crash between leaves a class-2 leaked record, reclaimed by ReclaimRecords).
	// The live chunks are already safe in the new block regardless of the outcome
	// here; a failed delete leaves the old block for the next run / reconcile.
	deleteOldBlock(ctx, v, rbs, blockID, report)

	report.BlocksCompacted++
	report.ChunksMoved += int64(len(commits))
	report.BytesReclaimed += rec.Length - int64(len(newBytes))
}

// deleteOldBlock deletes a block's remote object then its record (that order —
// a crash between leaves a class-2 leaked record, reclaimed by ReclaimRecords,
// never a record pointing at freed bytes). Returns true on full success; a
// failure is logged and counted in report.Errors, leaving the block for the
// next run.
func deleteOldBlock(ctx context.Context, v CompactMetaView, rbs remote.RemoteBlockStore, blockID string, report *CompactReport) bool {
	if err := rbs.DeleteBlock(ctx, blockID); err != nil {
		slog.Warn("compaction: delete old block failed — record kept, reclaim next run", "block_id", blockID, "err", err)
		report.Errors++
		return false
	}
	if err := v.DeleteBlockRecord(ctx, blockID); err != nil {
		slog.Warn("compaction: delete old record failed — retry next run", "block_id", blockID, "err", err)
		report.Errors++
		return false
	}
	return true
}

package fs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// ReadPayloadAt serves bytes for [offset, offset+len(dest)) for payloadID
// by consulting BOTH the in-flight append log (pre-rollup bytes) AND the
// rolled-up CAS chunks via the FileBlock manifest. This is the primary
// payload-keyed read entry on FSStore; the engine calls this BEFORE
// falling back to a CAS-hash-keyed walk on miss.
//
// Resolution order
//
//  1. Snapshot the per-payload log fd + lock; if present, replay the log
//     records (skipping the header) under the per-file mutex and copy any
//     matching bytes into dest. The append log is the source of truth for
//     pre-rollup writes — records that have not yet been rolled into CAS
//     live here and ONLY here.
//
//  2. For any portion of the requested window NOT satisfied from the log
//     walk the FileBlock manifest (via the engine-internal FileBlockStore)
//     and copy bytes from the rolled-up CAS chunks. This handles
//     post-rollup reads where the log records past rollup_offset may have
//     already been consumed.
//
//  3. If after both steps any byte of the requested window remains
//     uncovered, return (0, blockstore.ErrFileBlockNotFound) so the caller
//     falls back to remote-fetch + zero-fill.
//
// Returns (len(dest), nil) on full local satisfaction; (0
// ErrFileBlockNotFound) when nothing is available locally for the range
// (n, err) for genuine I/O errors.
func (bc *FSStore) ReadPayloadAt(ctx context.Context, payloadID string, dest []byte, offset uint64) (int, error) {
	if len(dest) == 0 {
		return 0, nil
	}
	if bc.isClosed() {
		return 0, ErrStoreClosed
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	// Track which bytes of dest have been filled. `covered[i]` is true
	// when dest[i] has been written from either the append log or a CAS
	// chunk. A short bitmap is overkill for the typical read window
	// (4-64 KiB); we use a parallel bool slice for clarity.
	covered := make([]bool, len(dest))
	end := offset + uint64(len(dest))

	// Step 1: replay the append log for any records that intersect
	// [offset, end). The log records are framed by writeRecord and
	// stored on disk after the 64-byte header; replaying them gives us
	// the pre-rollup byte contents at known file offsets. Later records
	// at the same offset overwrite earlier ones ("last record in
	// log wins" — the rollup respects log order; ReadPayloadAt must
	// too).
	if err := bc.replayLogIntoDest(ctx, payloadID, dest, offset, end, covered); err != nil {
		return 0, err
	}

	// Step 2: fill any remaining gaps from the FileBlock manifest
	// (rolled-up CAS chunks).
	if !allCovered(covered) {
		if err := bc.fillFromCASManifest(ctx, payloadID, dest, offset, covered); err != nil {
			return 0, err
		}
	}

	if !allCovered(covered) {
		// Some bytes of the requested window are not in local storage.
		// Surface as a miss so the caller falls back to remote.
		return 0, blockstore.ErrFileBlockNotFound
	}
	return len(dest), nil
}

// allCovered reports whether every entry in covered is true.
func allCovered(covered []bool) bool {
	for _, c := range covered {
		if !c {
			return false
		}
	}
	return true
}

// replayLogIntoDest fills the portions of dest that intersect
// [reqStart, reqEnd) from the payload's in-flight append-log records.
//
// Instead of replaying the FULL on-disk log (O(log-size) per read), it
// consults the per-payload logIndex to translate the requested file-offset
// window into the exact set of record frames that intersect it, then preads
// each one directly via readRecordAt. The index is trimmed at the
// compaction fence (logindex.go), so it holds only records NOT yet rolled
// into CAS — exactly the records that live ONLY in the log. Bytes whose
// records have already been consumed by the rollup are served from the CAS
// manifest in step 2 of ReadPayloadAt.
//
// Last-write-wins is preserved: EntriesForInterval returns entries in
// logPos (== arrival) order, and we copy each intersecting record into dest
// unconditionally in that order, so a later overwrite at the same offset
// supersedes an earlier record exactly as the previous full-log replay did.
//
// Concurrency: the logIndex entries carry ABSOLUTE log-byte positions
// (logPos). A concurrent rollup compaction (compaction.go) atomically
// rewrites the on-disk log AND rebases idx.entries[].logPos under the
// per-file mutex. To read a logPos and pread at it safely we therefore hold
// the SAME per-file mutex across both the index lookup and the preads — a
// snapshot taken without the lock could pread a stale (pre-rebase) logPos
// and decode a garbage frame. readRecordAt cross-checks the frame's
// declared payload_len and file_offset against the index entry, so any
// residual divergence surfaces as a hard error rather than wrong bytes.
//
// Returns nil when no log/index exists for payloadID (treated as "nothing
// to fill from the log") OR after a successful fill. Returns a non-nil
// error only for genuine I/O failures or log/index divergence.
func (bc *FSStore) replayLogIntoDest(_ context.Context, payloadID string, dest []byte, reqStart, reqEnd uint64, covered []bool) error {
	bc.logsMu.RLock()
	lf := bc.logFDs[payloadID]
	mu := bc.logLocks[payloadID]
	idx := bc.logIndices[payloadID]
	bc.logsMu.RUnlock()
	if lf == nil || mu == nil || idx == nil {
		return nil
	}

	// Hold the per-file mutex across the index lookup AND every pread so a
	// concurrent rollup compaction cannot rebase idx.entries[].logPos (and
	// rewrite the on-disk log) underneath us. AppendWrite also holds this
	// mutex, so we observe a consistent log/index pair.
	mu.Lock()
	defer mu.Unlock()

	if reqEnd <= reqStart {
		return nil
	}
	entries := idx.EntriesForInterval(reqStart, reqEnd-reqStart, nil)
	if len(entries) == 0 {
		return nil
	}

	// Open a separate read-only fd so we don't disturb the append fd's
	// EOF position. The path is captured at logFile construction time and
	// stable across reopens; compaction renames a fresh file into place
	// under the mutex we hold, so the path still resolves.
	rf, err := os.Open(lf.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Log was deleted between snapshot and open — treat as no log.
			return nil
		}
		return fmt.Errorf("ReadPayloadAt: open log: %w", err)
	}
	defer func() { _ = rf.Close() }()

	for _, e := range entries {
		// A single AppendWrite may legitimately exceed maxRecordPayload (the
		// writer's only ceiling is uint32; the engine appends whole writes —
		// e.g. a 32 MiB seed write lands as one frame). readRecordAt and the
		// sequential readRecord both REJECT frames above maxRecordPayload as a
		// torn/hostile-length DoS guard, so neither can decode such a record.
		// The pre-index full-log replay used readRecord, which skipped these
		// frames and let the byte coverage fall through to the CAS manifest /
		// remote-fetch miss path. We preserve that EXACT behavior: skip the
		// over-cap record rather than erroring. The bytes stay uncovered and
		// the caller resolves them downstream — byte-identical to the prior
		// replay. This is the case whose mishandling (a hard error) broke the
		// previous attempt's mixed-rw read of the 32 MiB seed at logPos=64.
		if e.payloadLen > maxRecordPayload {
			continue
		}
		recOff, payload, rerr := readRecordAt(rf, e.logPos, e.payloadLen)
		if rerr != nil {
			// A CRC mismatch or pread failure at a record the logIndex
			// claims is valid implies log-fd corruption or a logIndex/log
			// divergence bug. Surface it rather than silently serving wrong
			// bytes — a wrong read is data corruption.
			return fmt.Errorf("ReadPayloadAt: readRecordAt(logPos=%d): %w", e.logPos, rerr)
		}
		if recOff != e.fileOff {
			return fmt.Errorf("ReadPayloadAt: logIndex fileOff divergence at logPos=%d (indexed=%d frame=%d)",
				e.logPos, e.fileOff, recOff)
		}
		recEnd := recOff + uint64(len(payload))
		// EntriesForInterval already filtered to intersecting records, but
		// clamp defensively in case a record's tail extends past reqEnd or
		// its head sits below reqStart.
		if recEnd <= reqStart || recOff >= reqEnd {
			continue
		}
		copyStart := recOff
		if copyStart < reqStart {
			copyStart = reqStart
		}
		copyEnd := recEnd
		if copyEnd > reqEnd {
			copyEnd = reqEnd
		}
		destIdx := copyStart - reqStart
		srcIdx := copyStart - recOff
		n := copyEnd - copyStart
		copy(dest[destIdx:destIdx+n], payload[srcIdx:srcIdx+n])
		for i := destIdx; i < destIdx+n; i++ {
			covered[i] = true
		}
	}
	return nil
}

// fillFromCASManifest walks the FileBlock manifest for payloadID and
// fills any still-uncovered bytes of dest from the corresponding CAS
// chunks. This is the post-rollup read path — bytes that the rollup has
// already moved out of the append log into CAS storage. The manifest is
// the authoritative ordering once a chunk is committed.
//
// Silently no-ops when no manifest store is wired (fixtures that drive
// the FSStore directly without a coordinator). A nil manifest is
// indistinguishable from "no rolled-up chunks for this payload" so we
// just leave any uncovered bytes uncovered; the caller surfaces them as
// a miss.
func (bc *FSStore) fillFromCASManifest(ctx context.Context, payloadID string, dest []byte, reqStart uint64, covered []bool) error {
	if bc.blockStore == nil {
		return nil
	}
	rows, err := bc.blockStore.ListFileBlocks(ctx, payloadID)
	if err != nil {
		return fmt.Errorf("ReadPayloadAt: ListFileBlocks: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}
	// Walk rows in order (the manifest is already ID-sorted, which the
	// persister derives from the chunk's absolute offset). Stop early
	// once every byte is covered.
	type rowAbs struct {
		fb        *blockstore.FileBlock
		absOffset uint64
	}
	abs := make([]rowAbs, 0, len(rows))
	for _, fb := range rows {
		if fb == nil {
			continue
		}
		off, ok := blockstore.ParseChunkOffset(fb.ID)
		if !ok {
			continue
		}
		abs = append(abs, rowAbs{fb: fb, absOffset: off})
	}
	sort.Slice(abs, func(i, j int) bool { return abs[i].absOffset < abs[j].absOffset })

	reqEnd := reqStart + uint64(len(dest))
	for _, r := range abs {
		if r.fb.Hash.IsZero() {
			continue
		}
		chunkStart := r.absOffset
		chunkEnd := r.absOffset + uint64(r.fb.DataSize)
		if chunkEnd <= reqStart || chunkStart >= reqEnd {
			continue
		}
		// Fetch the chunk lazily — only when it intersects.
		data, gerr := bc.Get(ctx, r.fb.Hash)
		if gerr != nil {
			if errors.Is(gerr, blockstore.ErrChunkNotFound) {
				// Chunk evicted or never landed — leave uncovered bytes
				// uncovered and let the caller fall back.
				continue
			}
			return fmt.Errorf("ReadPayloadAt: Get chunk %s: %w", r.fb.Hash.String(), gerr)
		}
		// Clamp the visible data to DataSize so a padded on-disk chunk
		// does not leak garbage past the rollup-emitted byte count.
		dataLen := uint64(len(data))
		if uint64(r.fb.DataSize) > 0 && uint64(r.fb.DataSize) < dataLen {
			dataLen = uint64(r.fb.DataSize)
		}
		chunkEnd = chunkStart + dataLen

		copyStart := chunkStart
		if copyStart < reqStart {
			copyStart = reqStart
		}
		copyEnd := chunkEnd
		if copyEnd > reqEnd {
			copyEnd = reqEnd
		}
		if copyEnd <= copyStart {
			continue
		}
		destIdx := copyStart - reqStart
		srcIdx := copyStart - chunkStart
		n := copyEnd - copyStart
		// Fill ONLY bytes the append-log step (step 1) did not already
		// cover. The log holds the freshest, not-yet-rolled-up overwrites;
		// a CAS chunk may still span a file region whose latest bytes live
		// only in the log (a partial in-place overwrite of a rolled-up
		// extent). Stamping the chunk over a log-covered byte would discard
		// that overwrite — the data-loss class the partial-overwrite
		// regression test guards. Per-byte gating preserves last-write-wins
		// regardless of how the read window straddles the chunk boundary.
		for k := uint64(0); k < n; k++ {
			di := destIdx + k
			if covered[di] {
				continue
			}
			dest[di] = data[srcIdx+k]
			covered[di] = true
		}
		if allCovered(covered) {
			return nil
		}
	}
	return nil
}

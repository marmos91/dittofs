package fs

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/marmos91/dittofs/pkg/block"
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
//     uncovered, return (0, block.ErrFileBlockNotFound) so the caller
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
		return 0, block.ErrFileBlockNotFound
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

// replayLogIntoDest opens the payload's append log (if any) and replays
// every record, copying the portions that intersect [reqStart, reqEnd)
// into dest. Records are applied in log order so later writes at the
// same offset overwrite earlier ones.
//
// Returns nil when no log exists for payloadID (treated as "nothing to
// fill from the log") OR after a successful replay. Returns a non-nil
// error only for genuine I/O failures.
func (bc *FSStore) replayLogIntoDest(_ context.Context, payloadID string, dest []byte, reqStart, reqEnd uint64, covered []bool) error {
	sh := bc.shardFor(payloadID)
	sh.mu.RLock()
	lf := sh.logFDs[payloadID]
	mu := sh.logLocks[payloadID]
	sh.mu.RUnlock()
	if lf == nil || mu == nil {
		return nil
	}

	// Open a separate read-only fd so we don't disturb the append fd's
	// EOF position (writers seek to EOF on getOrCreateLog and we must
	// not race the append cursor). The path is captured at logFile
	// construction time and stable across reopens.
	rf, err := os.Open(lf.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Log was deleted between snapshot and open — treat as no log.
			return nil
		}
		return fmt.Errorf("ReadPayloadAt: open log: %w", err)
	}
	defer func() { _ = rf.Close() }()

	// We do NOT take the per-file mutex here. Writers append at EOF and
	// fsync each record before returning; the on-disk log is monotonic
	// (records only grow upward) so a concurrent reader sees either a
	// committed record or hits EOF/short-read mid-frame. readRecord
	// returns (_, _, false, nil) on torn-tail / EOF, terminating the
	// loop cleanly. The post-record CRC catches torn writes that
	// reached the platter but were not yet fsynced — those are skipped
	// as if the record did not exist (consistent with what the writer
	// thinks: a fsync that has not returned has not yet acknowledged
	// the write).

	// Seek past the 64-byte header.
	if _, err := rf.Seek(int64(logHeaderSize), io.SeekStart); err != nil {
		return fmt.Errorf("ReadPayloadAt: seek past header: %w", err)
	}

	for {
		recOff, payload, ok, rerr := readRecord(rf)
		if rerr != nil {
			return fmt.Errorf("ReadPayloadAt: readRecord: %w", rerr)
		}
		if !ok {
			break
		}
		recEnd := recOff + uint64(len(payload))
		// Skip records that do not intersect the requested window.
		if recEnd <= reqStart || recOff >= reqEnd {
			continue
		}
		// Compute intersection [hi, lo) and copy into dest.
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
		fb        *block.FileBlock
		absOffset uint64
	}
	abs := make([]rowAbs, 0, len(rows))
	for _, fb := range rows {
		if fb == nil {
			continue
		}
		off, ok := block.ParseChunkOffset(fb.ID)
		if !ok {
			continue
		}
		abs = append(abs, rowAbs{fb: fb, absOffset: off})
	}
	slices.SortFunc(abs, func(a, b rowAbs) int { return cmp.Compare(a.absOffset, b.absOffset) })

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
			if errors.Is(gerr, block.ErrChunkNotFound) {
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
		copy(dest[destIdx:destIdx+n], data[srcIdx:srcIdx+n])
		for i := destIdx; i < destIdx+n; i++ {
			covered[i] = true
		}
		if allCovered(covered) {
			return nil
		}
	}
	return nil
}

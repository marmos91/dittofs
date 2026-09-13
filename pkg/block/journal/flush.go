package journal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/marmos91/dittofs/pkg/block/chunker"
)

// Run is one offer: a contiguous dirty region of one file, plus its bytes.
// It is NOT a block. journal does not know what a block is — that is the whole
// point of being chunk-agnostic. The caller's carver turns Runs into Blocks.
type Run struct {
	ID     FileID
	Extent Extent // the dirty region being offered
	// DurableTail holds the contiguous already-durable extents immediately
	// after Extent — BOTH Resident and Remote, because a clobbered manifest
	// row's owed range can straddle evicted bytes. It supplies residency only:
	// where the manifest row ends is the caller's metadata store's answer, and
	// re-chunking the tail goes through ReadAt, not this reader — ReaderAt is
	// scoped to Extent.
	DurableTail []Extent
	// Final marks the last call for this file: drain anything buffered.
	Final bool
	// ReaderAt serves the run's bytes, addressed by file offset. It is a lazy
	// pass-through to the underlying record reads — no pre-materialized
	// buffer — and is valid only for the duration of the fn call it was
	// offered in. An implementation needing the bytes longer must copy them.
	io.ReaderAt
}

// FlushOptions selects what a Flush pass offers and who is told when the file
// is done.
type FlushOptions struct {
	// MaxAge is the dirty-age eligibility gate: a file whose oldest dirty
	// record is at least this old is offered even below MinSize.
	MaxAge time.Duration
	// MinSize is the dirty-bytes eligibility gate: a file below it is skipped
	// unless aged or forced.
	MinSize int64
	// Force offers the file regardless of the age/size gates.
	Force bool
	// AfterFile runs once per file after the last flip, WHILE journal still
	// holds the shard's flush lock — on success, on a partial-credit error and
	// on cancellation alike. The manifest reap belongs here: releasing the
	// lock before it re-introduces the re-entrant-pass corruption where a
	// second pass commits a row inside the first pass's about-to-be-reaped
	// span and the delayed reap then deletes that load-bearing row. It needs
	// no parameters beyond the id: fn and AfterFile share the caller's
	// closure, so the state the reap needs is already in scope.
	AfterFile func(ctx context.Context, id FileID) error
}

// FlushFunc is the callback a Flush pass offers each run to: the caller's
// carver assembly, upload pipeline and manifest commit. It and its accumulator
// MUST be constructed fresh per Flush call (see Flush's C9 note).
type FlushFunc func(ctx context.Context, r Run) ([]Extent, error)

// Flush runs one pass over id's dirty ranges: it offers each contiguous dirty
// run to fn — strictly sequentially, ascending offset, call N+1 not begun
// until call N returns — and flips exactly the fragments fn reports durable,
// validated per fragment against the offered snapshot.
//
// The contract, stated completely:
//
//   - fn is called strictly sequentially for the file, one run at a time,
//     ascending file offset (a block boundary is the accumulator's business,
//     and the accumulator is fn's).
//   - The shard's flushMu is held across the whole pass (serializing passes
//     for the shard), but the index lock sh.mu is NOT held across fn: it is
//     taken and released in short sections around the snapshot and each flip,
//     because fn performs network I/O.
//   - fn may return an empty durable slice any number of calls while it
//     buffers toward its own batch, reporting extents whose uploads have
//     since completed on a later call. Credit is fn's to report; journal
//     flips only what a report names.
//   - Each returned extent is decomposed against the fragments journal
//     offered, and every covered fragment whose version still matches flips —
//     per fragment, never whole-extent. A concurrent partial overwrite splits
//     an offered fragment into same-version survivors and a new-version
//     replacement; the survivors flip, the replacement is skipped and stays
//     dirty for the next pass. The version check is journal's own bookkeeping
//     — Extent carries no version and fn never sees one.
//   - A non-empty durable slice TOGETHER with a non-nil error means "these
//     committed, then I failed": journal flips the validated extents, stops
//     offering runs for the file, still calls AfterFile (the committed prefix
//     must be reaped), then returns the error.
//   - On ctx cancellation journal stops offering runs, flips whatever was
//     already validated, and still calls AfterFile.
//
// fn and its accumulator MUST be constructed fresh per Flush call. journal is
// chunk-agnostic and cannot detect a violation: a carver hoisted to a
// Syncer-lifetime field interleaves two files' bytes into one block — silent
// cross-file corruption.
//
// One Flush per file runs at a time: flushMu is shard-scoped and shardFor is
// deterministic, so a concurrent pass for the same file blocks rather than
// interleaving.
//
// A file is eligible when opts.Force is set, or its dirty-byte count crosses
// opts.MinSize, or its oldest dirty record is older than opts.MaxAge. Zero
// MinSize and MaxAge together fall back to the store's configured batching
// defaults, so a caller that leaves the gates unset gets the store's
// backpressure economics rather than an every-tick flush.
func (s *Store) Flush(ctx context.Context, id FileID, opts FlushOptions, fn FlushFunc) error {
	if fn == nil {
		return errors.New("journal: Flush requires fn")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closed.Load() {
		return errClosed
	}

	sh := s.shardFor(id)
	sh.flushMu.Lock()
	defer sh.flushMu.Unlock()

	// Snapshot the file's live dirty intervals (short section). fn runs with
	// the index lock released, so appends on the shard proceed.
	sh.mu.Lock()
	fi := sh.index[id]
	var snap []interval
	var dirty int64
	var firstDirty int64
	if fi != nil {
		firstDirty = fi.firstDirtyNanos
		for _, iv := range fi.ivs {
			if !iv.synced && !iv.cold {
				snap = append(snap, iv)
				dirty += iv.length
			}
		}
	}
	sh.mu.Unlock()
	if len(snap) == 0 {
		return nil
	}

	minSize, maxAge := opts.MinSize, opts.MaxAge
	if !opts.Force && minSize == 0 && maxAge == 0 {
		minSize, maxAge = s.cfg.CarveBlockSize, s.cfg.CarveMaxAge
	}
	if !opts.Force {
		aged := firstDirty != 0 && maxAge > 0 && s.clock.Now().UnixNano()-firstDirty >= int64(maxAge)
		if dirty < minSize && !aged {
			return nil
		}
	}

	runs := splitRuns(snap)
	fnCalled := false
	var firstErr error
	for _, run := range runs {
		if err := ctx.Err(); err != nil {
			// Cancellation: stop offering, flip already-validated (done below
			// each call), still call AfterFile.
			break
		}
		r := Run{
			ID:          id,
			Extent:      Extent{Off: run[0].fileOff, Len: run[len(run)-1].end() - run[0].fileOff, State: StateDirty},
			DurableTail: durableTail(sh, id, run[len(run)-1].end()),
			Final:       true,
			ReaderAt:    &flushReader{s: s, sh: sh, id: id, ivs: run},
		}
		fnCalled = true
		durable, err := fn(ctx, r)
		// Flip the validated extents even when fn failed — "these committed,
		// then I failed".
		if ferr := s.flipReported(sh, id, snap, durable); ferr != nil && firstErr == nil {
			firstErr = ferr
		}
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			break // stop offering runs for this file
		}
	}

	// AfterFile is mandatory and runs under the flush lock, whatever the
	// outcome above. The reap cannot move outside the lock: releasing at
	// Flush return lets the next pass commit rows inside this pass's
	// about-to-be-reaped span before the delayed reap lands.
	if fnCalled && opts.AfterFile != nil {
		if aerr := opts.AfterFile(ctx, id); aerr != nil && firstErr == nil {
			firstErr = aerr
		}
	}
	s.maybeResetDirtyClock(sh, id)
	return firstErr
}

// splitRuns groups a file's dirty interval snapshot into contiguous runs,
// splitting at every hole. Each run becomes one offer to fn.
func splitRuns(snap []interval) [][]interval {
	var runs [][]interval
	for start := 0; start < len(snap); {
		end := start + 1
		for end < len(snap) && snap[end].fileOff == snap[end-1].end() {
			end++
		}
		runs = append(runs, snap[start:end])
		start = end
	}
	return runs
}

// recordHasDirtyFragment reports whether any live interval still backed by the
// given physical record (segment + record offset) is dirty. Caller holds sh.mu.
func recordHasDirtyFragment(fi *fileIndex, seg uint64, recOff int64) bool {
	if fi == nil {
		return false
	}
	for k := range fi.ivs {
		if fi.ivs[k].loc.SegmentID == seg && fi.ivs[k].recOff == recOff &&
			!fi.ivs[k].synced && !fi.ivs[k].cold {
			return true
		}
	}
	return false
}

// flipRecordSynced sets a record's on-disk synced bit with a one-byte
// read-modify-write, preserving any other flag bits. It returns false without
// writing when the bit is already set. The header CRC excludes Flags, so no CRC
// rewrite is needed.
func flipRecordSynced(seg *segmentMeta, recOff int64) (bool, error) {
	var b [1]byte
	if _, err := seg.fd.ReadAt(b[:], recOff+recordFlagsOffset); err != nil {
		return false, fmt.Errorf("journal: read record flags seg %d off %d: %w", seg.id, recOff, err)
	}
	if b[0]&flagSynced != 0 {
		return false, nil
	}
	b[0] |= flagSynced
	if _, err := seg.fd.WriteAt(b[:], recOff+recordFlagsOffset); err != nil {
		return false, fmt.Errorf("journal: flip synced seg %d off %d: %w", seg.id, recOff, err)
	}
	return true, nil
}

// maybeResetDirtyClock clears a file's dirty-age marker once no dirty interval
// remains, so a later dirty write re-stamps a fresh age.
func (s *Store) maybeResetDirtyClock(sh *shard, id FileID) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	fi := sh.index[id]
	if fi == nil {
		return
	}
	for k := range fi.ivs {
		if !fi.ivs[k].synced && !fi.ivs[k].cold {
			return
		}
	}
	fi.firstDirtyNanos = 0
}

// durableTail returns the coalesced extents of the durable (resident or
// remote) live intervals immediately following end, stopping at the first
// hole or dirty interval. It is the residency answer behind Run.DurableTail:
// the caller uses it to decide what a replaced manifest row still owes, and
// nothing else — the row's own end is its metadata store's answer.
func durableTail(sh *shard, id FileID, end int64) []Extent {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	fi := sh.index[id]
	if fi == nil {
		return nil
	}
	// First interval that could reach past end; everything before it ends at
	// or before end and is skipped without a scan.
	k := sort.Search(len(fi.ivs), func(i int) bool { return fi.ivs[i].end() > end })
	if k >= len(fi.ivs) || fi.ivs[k].fileOff != end {
		return nil // hole at end: nothing durable is contiguous with the run
	}
	var out []Extent
	for ; k < len(fi.ivs); k++ {
		iv := fi.ivs[k]
		if len(out) > 0 && iv.fileOff != out[len(out)-1].Off+out[len(out)-1].Len {
			break
		}
		if !iv.synced && !iv.cold {
			break // dirty: the next pass's business, not this run's tail
		}
		out = append(out, Extent{Off: iv.fileOff, Len: iv.length, State: iv.state(), Durable: true})
	}
	return out
}

// flipReported flips the offered fragments that the reported durable extents
// cover. Each extent is decomposed against the offered snapshot and every
// covered fragment flips — but only if its version still matches the live
// index, via findRecord: a concurrent overwrite leaves the untouched
// sub-ranges under their old version (they flip — their bytes were uploaded)
// and the replaced sub-range under a new version (skipped, stays dirty). The
// on-disk synced bit is a record-level flag recovery replays over the whole
// record, so it flips only once none of that record's live fragments remain
// dirty.
func (s *Store) flipReported(sh *shard, id FileID, offered []interval, durable []Extent) error {
	if len(durable) == 0 {
		return nil
	}
	sh.mu.Lock()
	defer sh.mu.Unlock()
	fi := sh.index[id]

	type recKey struct {
		seg uint64
		off int64
	}
	touched := map[recKey]struct{}{}
	for _, e := range durable {
		k := sort.Search(len(offered), func(i int) bool { return offered[i].end() > e.Off })
		for ; k < len(offered) && offered[k].fileOff < e.Off+e.Len; k++ {
			iv := offered[k]
			if fi == nil {
				continue
			}
			if j := fi.findRecord(iv.fileOff, iv.version); j >= 0 && !fi.ivs[j].synced {
				fi.ivs[j].synced = true
				s.unsynced.Add(-fi.ivs[j].length)
				touched[recKey{iv.loc.SegmentID, iv.recOff}] = struct{}{}
			}
		}
	}

	for rk := range touched {
		if recordHasDirtyFragment(fi, rk.seg, rk.off) {
			continue // a live fragment of this record is not durable yet
		}
		seg := sh.segment(rk.seg)
		if seg == nil {
			continue // segment relocated/evicted; nothing to flip
		}
		flipped, err := flipRecordSynced(seg, rk.off)
		if err != nil {
			return err
		}
		if flipped {
			seg.syncedRecords.Add(1)
		}
	}
	return nil
}

// corruptRangeError names the file range a failed record read or extent
// mismatch covers, so the caller heals the range from a remote store or fails
// closed.
func corruptRangeError(id FileID, off, n int64) error {
	return &CorruptRangeError{FileID: id, Offset: off, Len: n}
}

// flushReader serves a contiguous run's bytes at arbitrary offsets, out of the
// owning records' CRC-verified payloads. It backs Run.ReaderAt: a lazy
// pass-through to the record reads, no pre-materialized buffer — the caller
// reads straight into its own scratch, keeping the copy count at two (record
// -> caller scratch -> the caller's block arena). Reads race nothing (segment
// fds are stable once created).
//
// The verification is load-bearing rather than defensive: the caller hashes
// whatever it reads and commits it to the remote store as content-addressed
// data, so a raw pread here would promote latent local bit rot to the
// authoritative copy of the file. A failed check fails the read instead,
// leaving the bytes dirty and local — recoverable, and loud.
type flushReader struct {
	s   *Store
	sh  *shard
	id  FileID
	ivs []interval
}

func (fr *flushReader) ReadAt(p []byte, off int64) (int, error) {
	i := sort.Search(len(fr.ivs), func(i int) bool { return fr.ivs[i].end() > off })
	if i >= len(fr.ivs) || off < fr.ivs[i].fileOff {
		return 0, io.EOF
	}
	var (
		rec    *record
		recSeg uint64
		total  int
	)
	for len(p) > 0 && i < len(fr.ivs) {
		iv := fr.ivs[i]
		sub := off - iv.fileOff
		if sub < 0 || sub >= iv.length {
			break // past this interval: runs are contiguous, so the read is done
		}
		if rec == nil || recSeg != iv.loc.SegmentID || rec.segOff != iv.recOff {
			r, err := fr.s.readRecord(fr.sh, iv.loc.SegmentID, iv.recOff, fr.id)
			if err != nil {
				if errors.Is(err, errTornRecord) {
					return total, corruptRangeError(fr.id, iv.fileOff, iv.length)
				}
				return total, err
			}
			rec, recSeg = &r, iv.loc.SegmentID
		}
		n := min(int64(len(p)), iv.length-sub)
		src, ok := rec.payloadRange(iv.loc.Offset+sub, n)
		if !ok {
			return total, corruptRangeError(fr.id, off, n)
		}
		copy(p, src)
		p = p[n:]
		off += n
		total += int(n)
	}
	return total, nil
}

// ChunkParams reports the FastCDC sizing this store's chunks were cut with, so
// a caller assembling blocks from Run bytes sizes its carver to the same
// profile. Stores without a chunking policy return the zero value (the caller
// degrades to its own default).
func (s *Store) ChunkParams() chunker.Params { return s.cfg.ChunkParams }

// BlockSize reports the pack size this store hands each block's worth of
// chunks to the caller's sink at — the same CarveBlockSize the batching gate
// uses. The caller's carver emits at this target; sizing it from anything else
// (a chunk-profile ratio, say) makes the objects a file drains the wrong size
// for every share that configured one.
func (s *Store) BlockSize() int64 { return s.cfg.CarveBlockSize }

// UploadConcurrency reports how many of one file's packed blocks a caller may
// commit concurrently — the same CarveUploadConcurrency the in-store carve
// path bounded its commit window with. Zero means "no policy": the caller
// applies its own default.
func (s *Store) UploadConcurrency() int { return s.cfg.CarveUploadConcurrency }

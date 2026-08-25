package journal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"sync"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block/chunker"
)

// carveScratchPool recycles the chunker accumulator buffer (cap one max chunk)
// across carve passes. Its contents are always overwritten before use and it
// never escapes packRuns, so recycling it is a pure allocation win — no per-op
// scratch.
//
// ponytail: the buffer is sized from the package-wide chunker.MaxChunkSize
// rather than the share's ChunkParams.Max, so a share chunking small still
// reserves 16 MiB for it — and the block arena adds the same overhang term on
// top; size both from ChunkParams.Max if that headroom ever shows up in a
// memory profile.
var carveScratchPool = sync.Pool{New: func() any {
	b := make([]byte, 0, chunker.MaxChunkSize)
	return &b
}}

// carveArenaPool recycles the per-block arenas backing the pending chunk copies
// handed to the sink. Both production sinks consume CarveChunk.Data synchronously
// inside CommitBlock (localBlockSink reads only len; engineBlockSink seals/frames
// into its own buffer before returning) — neither retains it — so a block's arena
// is safe to return to the pool once its CommitBlock has returned. Each concurrent
// block owns a distinct arena so overlapping commits never share backing bytes.
var carveArenaPool = sync.Pool{New: func() any {
	var b []byte
	return &b
}}

// Carve packs a shard's dirty ranges into fixed-size remote blocks and marks the
// records it moved as synced. The flow, per file:
//
//  1. Under the shard lock, snapshot the file's live dirty intervals (synced=false,
//     non-cold) in file-offset order, then release the lock so the CDC and upload
//     run without blocking appends.
//  2. Stream the dirty bytes through FastCDC -> BLAKE3 -> per-share dedup; novel
//     chunks accumulate into a block-sized batch.
//  3. At CarveBlockSize hand the novel chunks to the sink, which seals, frames,
//     uploads and atomically commits them. A block is not cut at a run boundary,
//     only at that size and once at the end of the file, so a scattered dirty set
//     drains as few full-size objects rather than one tiny object per run.
//     Successive blocks of one file commit concurrently through a bounded worker
//     pool (CarveUploadConcurrency) so a single large file's carve is not one
//     PutBlock at a time; packing itself stays sequential.
//  4. Only after a block's commit returns — and after every earlier block flipped —
//     flip each carved record's synced flag in place with a one-byte pwrite (the
//     header CRC excludes Flags, so no rewrite). The dispatcher applies the flips in
//     submission order regardless of which upload finishes first.
//
// Flipping strictly after the commit is the crash-safety invariant: a crash
// between the two leaves the records synced=false, so restart re-carves them, and
// content-addressed dedup makes the re-commit a no-op. The reverse order could
// mark records durable that never reached the remote — data loss — so it is never
// done.

// ChunkHash is the BLAKE3-256 content hash of a chunk's plaintext. Carve computes
// it so the deduper and the sink key on identical bytes without journal importing
// pkg/block's ContentHash.
type ChunkHash [32]byte

// Deduper reports whether a chunk is already durable on the remote store. Carve
// skips packing a chunk it reports present and still marks the covering records
// synced (the bytes are provably remote). Production wiring backs this with the
// per-share synced-hash oracle: a true result MUST mean "remote-durable", never
// merely "seen locally", or a flip could clean bytes that never reached remote.
type Deduper interface {
	IsChunkDurable(ctx context.Context, hash ChunkHash) (bool, error)
}

// CarveChunk is one content-defined chunk handed to the sink for packing.
type CarveChunk struct {
	Hash       ChunkHash
	FileID     FileID
	FileOffset int64  // logical offset of the chunk within the file
	Size       int    // chunk length; authoritative when Data is nil
	Data       []byte // plaintext; nil when the chunk deduped (nothing to upload)
}

// BlockSink seals, frames, uploads (PutBlock) and atomically commits one block's
// worth of novel chunks — every step that touches pkg/block, blockcodec and the
// metadata store, kept behind this interface so journal stays standalone.
// CommitBlock is atomic: a non-nil error means nothing became durable, so carve
// leaves the covered records dirty to re-carve next pass. Content-addressed
// commit makes a re-carve after a crash (or a duplicate concurrent carve) a
// no-op.
//
// Lifetime contract: CarveChunk.Data slices are backed by a pooled arena that
// the next carve flush reuses. An implementation MUST NOT retain any Data slice
// after CommitBlock returns; copy the bytes first if it needs them longer.
type BlockSink interface {
	CommitBlock(ctx context.Context, chunks []CarveChunk) error
}

// supersededReaper is an optional BlockSink capability. Once a carve pass has
// committed a file's rows, journal calls ReapSupersededManifest so the sink can
// delete the manifest rows they superseded — keeping the per-file FileChunk
// manifest a gap-free, overlap-free tiling of [0,size) after a partial overwrite.
// spans are the committed parts of the pass's re-carved (dirty) runs, disjoint
// and ascending; newOffsets are the chunk offsets the pass wrote (so the reap
// keeps them and deletes only stale straddlers/interior rows). One call per file
// rather than one per run: the sink re-reads the whole manifest to answer it, and
// that read happens under this shard's carve lock. Sinks without a metadata store
// (test fakes) simply don't implement it and the reap is skipped.
type supersededReaper interface {
	ReapSupersededManifest(ctx context.Context, id FileID, spans [][2]int64, newOffsets map[int64]struct{}) error
}

// manifestRowEnder is an optional BlockSink capability: it reports how far the
// manifest coverage straddling an offset reaches. Carve uses it to widen a run to
// a row boundary before packing it, so the fresh tiling covers every row the
// run-end reap deletes. Sinks without a metadata store (test fakes) don't
// implement it: a run is carved exactly as snapshotted.
//
// The answer is the greatest end among rows starting strictly before off and
// reaching past it, or off itself when none does — never a value below off. A
// fake that answers with a constant, or with zero, is not answering this
// question, and a caller that gates on the result will behave differently
// against it than against a metadata store.
type manifestRowEnder interface {
	ManifestRowEndAfter(ctx context.Context, id FileID, off int64) (int64, error)
}

// clobberGuard is an optional BlockSink capability, and it exists because a
// manifest row is keyed by the file offset of its first claimed byte while the
// commit that writes a row is an upsert. A run starting exactly on an existing
// row's offset therefore REPLACES that row rather than superseding it: the row
// is gone before the run-end reap ever lists the manifest, and everything it
// claimed past the run's end is left with no cover at all.
//
// Journal calls PreserveClobberedRow once per run, before the run is packed and
// so while the row still exists, naming the run's final bounds and the ranges
// past its end that are still OWED. The sink re-keys whatever the row about to
// be replaced still owns, restricted to those ranges.
//
// owed is what makes this safe, and it is the reason the question is asked here
// rather than at commit time: the manifest alone cannot tell a range that lost
// its cover from a range that is SUPPOSED to have none. A punched hole must read
// as zeros, and re-covering it with the replaced row's pre-punch content is its
// own corruption — one that a straightforward "keep whatever was there" does
// commit. Journal answers from the interval index, which distinguishes them:
// owed carries only ranges durable on the remote (evicted or resident), and
// excludes both holes and ranges still dirty for a later pass.
//
// Sinks without a metadata store (test fakes) don't implement it, and a run then
// replaces a row exactly as it did before.
type clobberGuard interface {
	PreserveClobberedRow(ctx context.Context, id FileID, runStart, runEnd int64, owed [][2]int64) error
}

// errCarveNotWired is returned by Carve when the dedup/sink collaborators have
// not been injected via SetCarveTargets.
var errCarveNotWired = errors.New("journal: carve targets not wired (SetCarveTargets)")

// SetCarveTargets injects the carve collaborators. Call once before the first
// Carve; production wires the real impls, tests pass fakes.
func (s *Store) SetCarveTargets(d Deduper, sink BlockSink) {
	s.deduper = d
	s.sink = sink
}

// CarveOptions selects what an explicit Carve targets.
type CarveOptions struct {
	// FileID, if set, carves only that file; empty means every eligible file.
	FileID FileID
	// Force carves eligible files regardless of the age/size batching gates.
	Force bool
}

// CarveResult reports what a carve pass moved to the remote store.
type CarveResult struct {
	BlocksWritten int
	BytesCarved   int64
}

// Carve packs eligible files' dirty ranges into remote blocks and flips their
// records to synced. A file is eligible when its dirty-byte count crosses
// CarveBlockSize, its oldest dirty record is older than CarveMaxAge, or opts.Force
// is set. It returns the first error encountered but continues past a per-file
// failure so one bad file does not strand the rest; failed files stay dirty.
func (s *Store) Carve(ctx context.Context, opts CarveOptions) (CarveResult, error) {
	var res CarveResult
	if err := ctx.Err(); err != nil {
		return res, err
	}
	if s.closed.Load() {
		return res, errClosed
	}
	if s.sink == nil || s.deduper == nil {
		return res, errCarveNotWired
	}

	shards := s.shards
	if opts.FileID != "" {
		shards = []*shard{s.shardFor(opts.FileID)}
	}
	now := s.clock.Now().UnixNano()
	maxAge := int64(s.cfg.CarveMaxAge)

	var firstErr error
	for _, sh := range shards {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		// Serialize this shard's carve against a concurrent carve pass; appends
		// still proceed (they take sh.mu, which carve only grabs briefly).
		sh.carveMu.Lock()
		for _, id := range s.carveCandidates(sh, opts, now, maxAge) {
			if err := ctx.Err(); err != nil {
				sh.carveMu.Unlock()
				return res, err
			}
			if err := s.carveFile(ctx, sh, id, &res); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		sh.carveMu.Unlock()
	}
	return res, firstErr
}

// carveCandidates returns the shard's files that meet the carve trigger. Held
// under sh.mu; the O(intervals) dirty-byte scan is fine because carve is a
// background/explicit pass, not a hot path.
func (s *Store) carveCandidates(sh *shard, opts CarveOptions, now, maxAge int64) []FileID {
	sh.mu.Lock()
	defer sh.mu.Unlock()

	var out []FileID
	eligible := func(id FileID, fi *fileIndex) {
		if fi == nil {
			return
		}
		var dirty int64
		for k := range fi.ivs {
			if !fi.ivs[k].synced && !fi.ivs[k].cold {
				dirty += fi.ivs[k].length
			}
		}
		if dirty == 0 {
			return
		}
		aged := fi.firstDirtyNanos != 0 && now-fi.firstDirtyNanos >= maxAge
		if opts.Force || dirty >= s.cfg.CarveBlockSize || aged {
			out = append(out, id)
		}
	}
	if opts.FileID != "" {
		eligible(opts.FileID, sh.index[opts.FileID])
		return out
	}
	for id, fi := range sh.index {
		eligible(id, fi)
	}
	return out
}

// carveFile snapshots one file's live dirty intervals, splits them into maximal
// contiguous runs (a hole resets FastCDC), packs them all through one packer,
// and reaps the rows each run's committed prefix superseded.
func (s *Store) carveFile(ctx context.Context, sh *shard, id FileID, res *CarveResult) error {
	sh.mu.Lock()
	fi := sh.index[id]
	var snap []interval
	if fi != nil {
		for _, iv := range fi.ivs {
			if !iv.synced && !iv.cold {
				snap = append(snap, iv)
			}
		}
	}
	sh.mu.Unlock()
	if len(snap) == 0 {
		return nil
	}

	// Runs cover disjoint ranges (extendRunToRowEnd stops at any non-warm
	// interval), so no live interval is shared between them.
	//
	// Two runs can still reference one physical record, because an overwrite
	// splits a record into fragments that a later carve can leave on either side
	// of a warm gap — and since one block spans runs, those two fragments can now
	// also flip from the same block, not only from different ones. That stays
	// safe because flipUpTo marks a fragment synced, checks whether the record
	// has any dirty fragment left, and flips the on-disk bit all under sh.mu:
	// the earlier call sees the sibling fragment still dirty and leaves the bit
	// alone, so whichever flip lands last is the one that observes zero remaining
	// and flips. Within one block the per-run flips run back to back on the
	// block's own worker, and across blocks the prev/mine chain serialises them,
	// so exactly one call ever observes the last dirty fragment.
	runs := splitRuns(snap)
	rs := make([]*runState, len(runs))
	for i, run := range runs {
		rs[i] = &runState{ivs: run, committedTo: run[0].fileOff}
	}
	res2, err := s.packRuns(ctx, sh, id, rs)
	res.BlocksWritten += res2.BlocksWritten
	res.BytesCarved += res2.BytesCarved

	// Reap what each run superseded, over the span its rows actually reached:
	// with those rows committed, the ones they replaced (stale straddlers,
	// interior chunks the fresh tiling covers) are safe to delete.
	//
	// The span is the run's committed frontier, not its end, so a run the pass
	// abandoned half way is reaped over the part it did commit. That part has
	// already flipped its records synced, so no later pass re-carves them and no
	// later pass reaps for them either: skipping it here means it never happens,
	// and the stale rows outlive the fresh ones forever. Overlap resolution is
	// greatest-start, so a stale row starting later than a fresh one then wins
	// and serves old bytes on a cold read.
	//
	// For the same reason this runs even when packing failed: every run that got
	// anything committed is reaped for, including the ones after a run that
	// failed mid-way.
	//
	// All of the file's spans go in one call. The sink answers a reap by reading
	// the file's whole manifest and re-projecting File.Blocks from it, so one call
	// per run made both costs scale with the run count — a file with tens of
	// thousands of dirty runs spent minutes of that under this shard's carve lock,
	// after its records had already flipped synced and its uploads had drained.
	//
	// ponytail: a caller cancelling between the flip and the reap still strands
	// those rows, since nothing retries a reap and the records are no longer
	// dirty; persist a pending-reap intent, or defer the flip until the reap
	// lands, if that window ever shows up in the field.
	if r, ok := s.sink.(supersededReaper); ok {
		spans := make([][2]int64, 0, len(rs))
		newOffsets := make(map[int64]struct{})
		for _, st := range rs {
			if st.committedTo <= st.start() {
				continue
			}
			spans = append(spans, [2]int64{st.start(), st.committedTo})
			maps.Copy(newOffsets, st.newOffsets)
		}
		if len(spans) > 0 {
			if rerr := r.ReapSupersededManifest(ctx, id, spans, newOffsets); rerr != nil && err == nil {
				err = rerr
			}
		}
	}
	if err != nil {
		return err
	}
	s.maybeResetDirtyClock(sh, id)
	return nil
}

// splitRuns groups an offset-ordered snapshot into maximal contiguous runs; a
// hole between two intervals starts a new run.
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

// packRuns streams every dirty run of one file through FastCDC in file-offset
// order, dedups each chunk, packs the novel ones into blocks, and flips records
// to synced as the durable frontier advances. Each run is widened to its
// manifest row end as the packer reaches it.
//
// Chunks stay inside a run — a fresh chunker and reader per run means no chunk
// spans the hole between two of them — but blocks do not: a block is flushed
// only when it reaches CarveBlockSize, and once at the end of the file. That is
// what keeps a scattered dirty set from emitting one tiny remote object per run;
// the objects a randomly-written file drains are sized by CarveBlockSize, not by
// how far apart its writes landed.
func (s *Store) packRuns(ctx context.Context, sh *shard, id FileID, rs []*runState) (CarveResult, error) {
	var res CarveResult

	// One semaphore for the whole file: it bounds the in-flight block arenas
	// across every run, so that term stays cap(sem) x (CarveBlockSize + one
	// overhang chunk) however many runs the file has. The chunker scratch buffer
	// is a single pooled buffer for the whole pass, not one per run.
	sem := make(chan struct{}, s.cfg.CarveUploadConcurrency)

	// disp overlaps successive blocks' CommitBlock (upload + commit) while packing
	// stays sequential. It owns the bounded worker pool, the per-block buffers and
	// the ordered flip chain; flush hands it a completed block or a bare watermark.
	disp := newCarveDispatcher(ctx, s, sh, id, rs, &res, sem)

	// Each packed block gets its OWN buffer (cap one block plus one overhang chunk)
	// so its bytes stay live while its CommitBlock runs concurrently with the next
	// block's packing — the recycled arena of the sequential path can't do that.
	// Compute in int64 and clamp before the int conversion so a pathological
	// CarveBlockSize can't silently wrap on 32-bit platforms.
	arenaCap64 := s.cfg.CarveBlockSize + int64(chunker.MaxChunkSize)
	if arenaCap64 > math.MaxInt {
		arenaCap64 = math.MaxInt
	}
	arenaCap := int(arenaCap64)

	// The block currently being packed. arena is its private buffer (nil until the
	// first novel chunk claims a pool buffer and a concurrency slot); arenaOff is
	// the fill cursor. On any early exit these are returned to disp so the slot and
	// buffer are not leaked.
	// batchBytes counts the run bytes this batch tiles, deduped chunks included,
	// so a fully deduped batch (empty arena) is still bounded and committed on the
	// same cadence as one carrying bytes.
	var (
		pending    []CarveChunk
		arenap     *[]byte
		arena      []byte
		arenaOff   int
		batchBytes int64
	)
	ensureArena := func() error {
		if arenap != nil {
			return nil
		}
		p, err := disp.acquire(arenaCap)
		if err != nil {
			return err
		}
		arenap, arena, arenaOff = p, *p, 0
		return nil
	}

	// blockFirstRun is the index of the run the block being packed started in;
	// every run from there to the one being packed contributes to it, which is
	// what the flush's flipPlan names.
	blockFirstRun := 0

	// flush hands the packed block (if any) and its flip plan to the dispatcher,
	// which commits then flips in submission order. Packing continues immediately;
	// the commit and flip happen on the pool. Ownership of the buffer moves to the
	// dispatcher, so the local arena state resets to "no block".
	flush := func(plan flipPlan) {
		disp.submit(pending, arenap, arena, plan)
		pending, arenap, arena, arenaOff, batchBytes = nil, nil, nil, 0, 0
	}

	// buf accumulates bytes for the chunker; it never exceeds one max chunk, so
	// RAM stays at FastCDC-chunk scale even for a multi-GiB file. One buffer
	// serves every run of the pass and is read into directly (no separate read
	// buffer); it is recycled across passes through the pool.
	bufp := carveScratchPool.Get().(*[]byte)
	buf := (*bufp)[:0]
	defer func() {
		*bufp = buf
		carveScratchPool.Put(bufp)
	}()

	// packErr is the first error hit while packing (read/dedup/context). It stops
	// packing but the already-dispatched blocks still drain via disp.wait so no
	// goroutine or buffer leaks; disp.wait folds it together with any commit error.
	var packErr error
	for ri := range rs {
		// Widen this run to its manifest row end here, immediately before packing
		// it, rather than widening every run of the file up front. Each lookup is
		// a whole-manifest read, so resolving them all first buys nothing durable
		// until the last one returns: on a scattered dirty set that prologue runs
		// for as long as the file has runs, and the unsynced counter does not move
		// for any of it. Resolving one run at a time keeps a block committing and
		// flipping every CarveBlockSize, so the counter falls throughout the pass.
		// A run only ever grows at its tail, so the next run's start — the limit
		// this one may not cross — is still the snapshotted one.
		//
		// ponytail: serial, one whole-manifest read per run on the packing
		// critical path, so a file with many dirty runs pays that read once per
		// run before its bytes move; answer the straddle query from a single
		// per-pass listing (or a point lookup at the offset) if that read ever
		// dominates a carve profile.
		limit := int64(math.MaxInt64)
		if ri+1 < len(rs) {
			limit = rs[ri+1].start()
		}
		ivs, rowEnd, err := s.extendRunToRowEnd(ctx, sh, id, rs[ri].ivs, limit)
		if err != nil {
			packErr = err
			break
		}
		rs[ri].ivs = ivs

		// The run's first fresh chunk lands on the run's start offset, so if a
		// manifest row is keyed there it is about to be replaced. Ask the sink to
		// keep what that row still owns past where this run will stop — but only
		// over ranges the interval index says are still owed, since the row may
		// equally be spanning a hole it has no business re-covering.
		if guard, ok := s.sink.(clobberGuard); ok {
			runEnd := rs[ri].end()
			if owed := syncedRanges(sh, id, runEnd, rowEnd); len(owed) > 0 {
				if err := guard.PreserveClobberedRow(ctx, id, rs[ri].start(), runEnd, owed); err != nil {
					packErr = err
					break
				}
			}
		}

		// A fresh chunker and reader per run, and an empty accumulator, so a chunk
		// never spans the hole between two runs. The block being packed carries
		// over: that is what lets one block span them.
		c := chunker.NewChunkerWithParams(s.cfg.ChunkParams)
		rr := &runReader{s: s, sh: sh, id: id, ivs: rs[ri].ivs}
		fileOff := rs[ri].start()
		rs[ri].newOffsets = make(map[int64]struct{})
		buf = buf[:0]
		eof := false

		for {
			if err := ctx.Err(); err != nil {
				packErr = err
				break
			}
			// A commit already failed: stop packing so the watermark can't advance past
			// the failed block. In-flight commits drain in disp.wait.
			if disp.aborted() {
				break
			}
			for !eof && len(buf) < chunker.MaxChunkSize {
				n, err := rr.Read(buf[len(buf):cap(buf)])
				if n > 0 {
					buf = buf[:len(buf)+n]
				}
				if errors.Is(err, io.EOF) {
					eof = true
					break
				}
				if err != nil {
					packErr = err
					break
				}
			}
			if packErr != nil {
				break
			}
			if len(buf) == 0 {
				break
			}
			boundary, _ := c.Next(buf, eof)
			if boundary == 0 {
				if !eof {
					continue // below MinChunkSize and more is coming: read more
				}
				boundary = len(buf)
			}

			h := ChunkHash(blake3.Sum256(buf[:boundary]))
			// Dedup consults the committed synced-hash oracle. A block being committed
			// concurrently has NOT yet marked its hashes durable, so this never observes
			// a sibling block's uncommitted hash as durable — at worst a duplicate chunk
			// is re-packed, which the content-addressed commit collapses to a no-op.
			durable, err := s.deduper.IsChunkDurable(ctx, h)
			if err != nil {
				packErr = err
				break
			}
			// A deduped chunk has nothing to upload but still needs its manifest row:
			// the reap deletes every row in the run span the run did not write, so
			// dropping it here leaves the range on a stale straddler or on nothing.
			cc := CarveChunk{Hash: h, FileID: id, FileOffset: fileOff, Size: boundary}
			if !durable {
				if err := ensureArena(); err != nil {
					packErr = err
					break
				}
				// Bound proof: this block's bytes < CarveBlockSize before this append
				// (else the prior iteration flushed and started a fresh arena), and
				// boundary <= MaxChunkSize, so arenaOff+boundary <= CarveBlockSize-1+
				// MaxChunkSize <= cap. The grow is a fail-loud belt: if that invariant
				// ever breaks (e.g. a config change), realloc rather than slice out of
				// bounds. Already-pending Data slices keep pointing at the old backing
				// (still live), so no copy is needed — the new chunk lands in the larger
				// arena and the grown slice ships to the dispatcher.
				if arenaOff+boundary > cap(arena) {
					arena = make([]byte, arenaOff+boundary)
				}
				data := arena[arenaOff : arenaOff+boundary : arenaOff+boundary]
				copy(data, buf[:boundary])
				arenaOff += boundary
				cc.Data = data
				res.BytesCarved += int64(boundary)
			}
			pending = append(pending, cc)
			batchBytes += int64(boundary)
			rs[ri].newOffsets[fileOff] = struct{}{}
			fileOff += int64(boundary)
			buf = append(buf[:0], buf[boundary:]...)

			if batchBytes >= s.cfg.CarveBlockSize {
				flush(flipPlan{first: blockFirstRun, last: ri, lastOff: fileOff})
				blockFirstRun = ri
			}
			if eof && len(buf) == 0 {
				break
			}
		}
		if packErr != nil || disp.aborted() {
			break
		}
	}

	if packErr != nil || disp.aborted() {
		// A read/dedup error (packErr) or an in-flight commit failure (aborted)
		// ends the pass. Abandon the half-packed block (return its slot/buffer) and
		// drain the blocks already in flight, but submit nothing more: advancing
		// the watermark or committing the tail past a failure only adds orphan
		// uploads. disp.wait returns the commit error in watermark order.
		disp.discard(arenap, arena)
		if err := disp.wait(); err != nil {
			return res, err
		}
		return res, packErr
	}

	// Tail: commit any remainder and flip through the end of the last run (records
	// covered only by already-durable chunks flip here too, via the bare watermark).
	last := len(rs) - 1
	flush(flipPlan{first: blockFirstRun, last: last, lastOff: rs[last].end()})
	if err := disp.wait(); err != nil {
		return res, err
	}
	return res, nil
}

// extendRunToRowEnd grows the run forward to the end of the manifest coverage its
// end lands inside, so the fresh tiling covers every row the run-end reap is
// about to delete. The added bytes are already durable; they are re-chunked only
// to re-tile the range they share with a superseded row, which is the same price
// a partial overwrite of a warm interval already pays.
//
// It also reports how far the manifest coverage straddling the run's end
// reaches, so a caller that has to act on the rows the run stops inside does not
// pay the straddle lookup a second time. That answer is the run's own end
// whenever the lookup was not needed or the extension consumed the row.
//
// The extension is skipped whole unless every byte of it is live, contiguous and
// warm. An evicted range holds no local bytes to re-chunk, and half an extension
// still ends inside the row.
//
// Skipping costs a re-chunk, not correctness: the run then still ends inside a
// row, and the reap narrows that row off its head so the fresh tiling owns every
// offset the run covered. Extending buys a tiling whose rows line up with the
// old boundaries instead of one that leaves a narrowed remnant behind.
//
// A row reaching past limit, the offset the next run starts at, is refused
// outright. It is redundant today: runs are packed in ascending file-offset
// order on one goroutine and nothing flips ahead of the packer, so every
// interval this pass has already flipped lies below the window warmAt and
// warmTail inspect, the interval at limit is still the next run's own dirty
// head, and warmTail's bail on it already stops the extension there. It stays
// because that redundancy is a property of the packer being sequential — runs
// were carved concurrently before and would be again — and under any such
// ordering a sibling's already-flipped head is indistinguishable here from
// pre-existing warm data. It has to be a refusal rather than a truncation to
// limit: a run truncated there still ends inside the row, so the reap narrows
// the row off its head just as it would with no extension at all — the truncated
// extension would re-chunk those bytes for nothing.
//
// ponytail: a row reaching past a later dirty run is left alone, so that run
// still ends mid-row; covering it means merging the two runs, which is worth
// building only if that shape shows up in practice.
func (s *Store) extendRunToRowEnd(ctx context.Context, sh *shard, id FileID, run []interval, limit int64) ([]interval, int64, error) {
	runEnd := run[len(run)-1].end()
	ender, ok := s.sink.(manifestRowEnder)
	if !ok {
		return run, runEnd, nil
	}
	// The manifest lookup is a whole-manifest read, so it is gated on the cheap
	// in-memory answer first. With nothing durable past the run's end there is
	// neither anything to extend over nor anything a replaced row could still be
	// owed — which is every append, the case that would otherwise pay this read
	// once per run for the life of the file.
	if !anySyncedFrom(sh, id, runEnd) {
		return run, runEnd, nil
	}
	rowEnd, err := ender.ManifestRowEndAfter(ctx, id, runEnd)
	if err != nil {
		return nil, 0, err
	}
	if rowEnd <= runEnd {
		return run, runEnd, nil
	}
	if !warmAt(sh, id, runEnd) {
		return run, rowEnd, nil // nothing warm past the run: no extension is possible
	}
	if rowEnd > limit {
		// See the doc comment above: redundant while the packer is sequential
		// and forward, kept for any ordering that carves runs concurrently.
		return run, rowEnd, nil
	}
	tail := warmTail(sh, id, runEnd, rowEnd)
	if len(tail) == 0 {
		return run, rowEnd, nil
	}
	// A fresh slice: run aliases the carve snapshot, whose next entries belong to
	// the following run.
	out := make([]interval, 0, len(run)+len(tail))
	out = append(out, run...)
	return append(out, tail...), rowEnd, nil
}

// anySyncedFrom reports whether any range at or after off is durable on the
// remote. It is the cheap gate on the whole straddle question: a run with
// nothing synced past its end can neither be extended nor leave a replaced row
// owed anything.
func anySyncedFrom(sh *shard, id FileID, off int64) bool {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	fi := sh.index[id]
	if fi == nil {
		return false
	}
	for k := range fi.ivs {
		if fi.ivs[k].synced && fi.ivs[k].end() > off {
			return true
		}
	}
	return false
}

// syncedRanges returns the sub-ranges of [from, to) that hold data durable on
// the remote store — resident or evicted — coalesced and ascending. Live
// intervals are held in ascending file order, so one forward walk emits them
// already ordered and only touching pieces need joining.
//
// It is the discriminator the manifest cannot supply. A range with no interval
// is a hole: nothing was ever written there, or a punch took it away, and it
// must read as zeros. A range whose interval is still dirty is about to be
// re-carved and is served from the local tier until it is. Neither is owed a
// manifest row, and covering either one with a row that used to span it puts
// back bytes the file no longer has.
func syncedRanges(sh *shard, id FileID, from, to int64) [][2]int64 {
	if from >= to {
		return nil
	}
	sh.mu.Lock()
	defer sh.mu.Unlock()
	fi := sh.index[id]
	if fi == nil {
		return nil
	}
	var out [][2]int64
	for k := range fi.ivs {
		iv := fi.ivs[k]
		if !iv.synced || iv.end() <= from || iv.fileOff >= to {
			continue
		}
		lo, hi := max(iv.fileOff, from), min(iv.end(), to)
		if n := len(out); n > 0 && lo <= out[n-1][1] {
			out[n-1][1] = max(out[n-1][1], hi)
			continue
		}
		out = append(out, [2]int64{lo, hi})
	}
	return out
}

// warmAt reports whether a warm live interval (durable locally, not evicted, not
// dirty) starts exactly at off. A run with nothing warm past its end cannot be
// extended whatever the manifest says, so this keeps the manifest lookup off the
// common path: a carve of freshly appended bytes ends at the file's end, where
// nothing lives at all.
func warmAt(sh *shard, id FileID, off int64) bool {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	fi := sh.index[id]
	if fi == nil {
		return false
	}
	for k := range fi.ivs {
		if fi.ivs[k].fileOff == off {
			return fi.ivs[k].synced && !fi.ivs[k].cold
		}
	}
	return false
}

// warmTail returns the live intervals covering [from, to), clipped to that range,
// when every one of them is present, contiguous and warm (durable locally, not
// evicted, not still dirty). Anything else yields nil: a hole or an evicted range
// has no local bytes to re-chunk, and a dirty range belongs to a later run that
// would then carve it twice.
func warmTail(sh *shard, id FileID, from, to int64) []interval {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	fi := sh.index[id]
	if fi == nil {
		return nil
	}
	var out []interval
	cur := from
	for k := range fi.ivs {
		iv := fi.ivs[k]
		if iv.end() <= cur {
			continue
		}
		if iv.fileOff > cur || iv.cold || !iv.synced {
			return nil
		}
		stop := min(iv.end(), to)
		out = append(out, iv.clamp(cur, stop))
		cur = stop
		if cur >= to {
			return out
		}
	}
	return nil
}

// flipUpTo advances the durable frontier to watermark. It marks each live
// interval fragment whose range ends there as synced in memory, then flips a
// physical record's on-disk synced bit — but only once none of that record's
// live fragments remain dirty.
//
// The distinction is load-bearing. A newer overlapping write splits one physical
// record into several live fragments that can become durable in different
// flushes, yet the on-disk synced bit is a single record-level flag that
// recovery replays over the record's whole original range. Flipping it after
// only the first fragment is durable would, on a crash, make recovery treat the
// record's still-dirty fragments as synced — silent data loss. So the bit is set
// strictly after the record has no dirty live coverage left.
//
// The flip is a read-modify-write of the flags byte (preserving tombstone / any
// other bits) with no fsync — a lost flip just re-carves, which dedup makes a
// no-op. A concurrent overwrite that replaced a fragment since the snapshot
// leaves findRecord empty; the newer record carves next pass.
func (s *Store) flipUpTo(sh *shard, id FileID, run []interval, flipIdx *int, watermark int64) error {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	fi := sh.index[id]

	type recKey struct {
		seg uint64
		off int64
	}
	touched := map[recKey]struct{}{}
	for *flipIdx < len(run) && run[*flipIdx].end() <= watermark {
		iv := run[*flipIdx]
		*flipIdx++
		if fi == nil {
			continue
		}
		if k := fi.findRecord(iv.fileOff, iv.version); k >= 0 && !fi.ivs[k].synced {
			fi.ivs[k].synced = true
			s.unsynced.Add(-fi.ivs[k].length)
			touched[recKey{iv.loc.SegmentID, iv.recOff}] = struct{}{}
		}
	}

	for rk := range touched {
		if recordHasDirtyFragment(fi, rk.seg, rk.off) {
			continue // a live fragment of this record is not durable yet
		}
		seg := sh.segment(rk.seg)
		if seg == nil {
			continue // segment relocated/evicted (later PRs); nothing to flip
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

// runReader streams a contiguous run's live bytes in file-offset order, serving
// each interval out of its owning record's CRC-verified payload. Reads race
// nothing (segment fds are stable once created).
//
// The verification is load-bearing rather than defensive: carve hashes whatever
// it reads and commits it to the remote store as content-addressed data, so a
// raw pread here would promote latent local bit rot to the authoritative copy of
// the file. A failed check aborts the run instead, leaving the bytes dirty and
// local — recoverable, and loud.
type runReader struct {
	s   *Store
	sh  *shard
	id  FileID
	ivs []interval
	i   int   // current interval
	off int64 // bytes already read from the current interval
	// rec is the verified record backing the current interval, kept so a record
	// spanning several Read calls or several intervals is read and verified once
	// rather than per call. recSeg pairs with rec.segOff to identify it.
	rec    *record
	recSeg uint64
}

func (rr *runReader) Read(p []byte) (int, error) {
	for rr.i < len(rr.ivs) {
		iv := rr.ivs[rr.i]
		remain := iv.length - rr.off
		if remain <= 0 {
			rr.i++
			rr.off = 0
			continue
		}
		if rr.rec == nil || rr.recSeg != iv.loc.SegmentID || rr.rec.segOff != iv.recOff {
			rec, err := rr.s.readRecord(rr.sh, iv.loc.SegmentID, iv.recOff, rr.id)
			if err != nil {
				if errors.Is(err, errTornRecord) {
					return 0, rr.corruptRange(iv, remain)
				}
				return 0, err
			}
			rr.rec, rr.recSeg = &rec, iv.loc.SegmentID
		}
		n := min(int64(len(p)), remain)
		src, ok := rr.rec.payloadRange(iv.loc.Offset+rr.off, n)
		if !ok {
			return 0, rr.corruptRange(iv, remain)
		}
		copy(p, src)
		rr.off += n
		return int(n), nil
	}
	return 0, io.EOF
}

// corruptRange names the still-unread part of iv as the corrupt file range, so
// the carve failure points at the bytes it refused to hash.
func (rr *runReader) corruptRange(iv interval, remain int64) error {
	return &CorruptRangeError{FileID: rr.id, Offset: iv.fileOff + rr.off, Len: remain}
}

package journal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"sync/atomic"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block/chunker"
)

// carveScratchPool recycles the chunker accumulator buffer (cap one max chunk)
// across carve runs. Its contents are always overwritten before use and it never
// escapes carveRun, so recycling it is a pure allocation win — no per-op scratch.
var carveScratchPool = sync.Pool{New: func() any {
	b := make([]byte, 0, chunker.MaxChunkSize)
	return &b
}}

// carveArenaPool recycles the arena backing the pending chunk copies handed to the
// sink. Both production sinks consume CarveChunk.Data synchronously inside
// CommitBlock (localBlockSink reads only len; engineBlockSink seals/frames into its
// own buffer before returning) — neither retains it — so the arena is safe to reuse
// once flush's CommitBlock returns and pending is cleared.
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
//  3. At CarveBlockSize (or the end of a contiguous run) hand the novel chunks to
//     the sink, which seals, frames, uploads and atomically commits them.
//  4. Only after the commit returns, flip each carved record's synced flag in
//     place with a one-byte pwrite (the header CRC excludes Flags, so no rewrite).
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
	Data       []byte // plaintext; the sink seals it before framing
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

// supersededReaper is an optional BlockSink capability. After a carve run has
// committed every row it produced, journal calls ReapSupersededManifest so the
// sink can delete the manifest rows the run superseded — keeping the per-file
// FileChunk manifest a gap-free, overlap-free tiling of [0,size) after a partial
// overwrite (#953). runStart/runEnd bound the re-carved (dirty) range; newOffsets
// are the chunk offsets this run wrote (so the reap keeps them and deletes only
// stale straddlers/interior rows). Sinks without a metadata store (test fakes)
// simply don't implement it and the reap is skipped.
type supersededReaper interface {
	ReapSupersededManifest(ctx context.Context, id FileID, runStart, runEnd int64, newOffsets map[int64]struct{}) error
}

// errCarveNotWired is returned by Carve when the dedup/sink collaborators have
// not been injected via SetCarveTargets.
var errCarveNotWired = errors.New("journal: carve targets not wired (SetCarveTargets)")

// SetCarveTargets injects the carve collaborators. Call once before the first
// Carve; the production impls are wired here at PR7, tests pass fakes.
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
// contiguous runs (a hole resets FastCDC), and carves each run.
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

	for start := 0; start < len(snap); {
		end := start + 1
		for end < len(snap) && snap[end].fileOff == snap[end-1].end() {
			end++
		}
		if err := s.carveRun(ctx, sh, id, snap[start:end], res); err != nil {
			return err
		}
		start = end
	}
	s.maybeResetDirtyClock(sh, id)
	return nil
}

// carveUploadConcurrency is the in-flight upload window for one run, at least 1.
func (s *Store) carveUploadConcurrency() int {
	if n := s.cfg.CarveUploadConcurrency; n >= 1 {
		return n
	}
	return 1
}

// carveRun streams one contiguous dirty run through FastCDC, dedups each chunk,
// packs novel chunks into blocks (dispatched at CarveBlockSize and at the run's
// end), and flips the run's records to synced as the durable frontier advances.
//
// Packing stays strictly sequential (the chunker reads the run's bytes in order),
// but each packed block's CommitBlock (PutBlock + metadata commit) runs in its own
// goroutine, so up to CarveUploadConcurrency block uploads overlap within one file
// — the lever a single large file's drain needs. The flips stay ordered and
// crash-safe: block K's flipUpTo runs only after block K-1's flip and after block
// K's own CommitBlock returned nil (an ordered handoff over prevFlipped). The first
// CommitBlock error cancels the run and stops the watermark advancing past that
// block; blocks that already uploaded past it are left as GC-reclaimable orphans.
//
// Dedup visibility stays commit-gated: IsChunkDurable is consulted only from this
// packing goroutine and a hash becomes durable solely inside CommitBlock, so block
// K never dedups against block K-1's not-yet-committed hashes — the ordered flip
// then cannot clean bytes that never reached the remote.
func (s *Store) carveRun(ctx context.Context, sh *shard, id FileID, run []interval, res *CarveResult) error {
	c := chunker.NewChunkerWithParams(s.cfg.ChunkParams)
	rr := &runReader{s: s, sh: sh, ivs: run}

	fileOff := run[0].fileOff
	flipIdx := 0
	var pending []CarveChunk
	var pendingBytes int64
	// Offsets of every chunk this run tiles (novel or deduped), so the run-end
	// reap keeps this run's own rows and deletes only superseded ones (#953).
	newOffsets := make(map[int64]struct{})

	// Each packed block gets its OWN block-sized buffer (not one recycled arena):
	// a block's Data slices are read by its upload goroutine while packing already
	// fills the next block, so the buffers cannot alias. Peak carve RAM is thus
	// CarveUploadConcurrency block buffers. A buffer holds at most one block's
	// pending bytes: pendingBytes dispatches at CarveBlockSize and one appended
	// chunk is at most MaxChunkSize, so CarveBlockSize + MaxChunkSize bounds it.
	// Compute in int64 and clamp before the int conversion so a pathological
	// CarveBlockSize can't silently wrap on 32-bit platforms.
	arenaCap64 := s.cfg.CarveBlockSize + int64(chunker.MaxChunkSize)
	if arenaCap64 > math.MaxInt {
		arenaCap64 = math.MaxInt
	}
	arenaCap := int(arenaCap64)

	var (
		blockp   *[]byte // current block's pooled backing; nil until the first novel chunk
		block    []byte
		blockOff int
	)
	getBlock := func() {
		blockp = carveArenaPool.Get().(*[]byte)
		block = *blockp
		if cap(block) < arenaCap {
			block = make([]byte, arenaCap)
		}
		block = block[:cap(block)]
		blockOff = 0
	}
	// If packing exits early (error/cancel) holding an undispatched block, return
	// its buffer to the pool. dispatchPending sets blockp = nil once a block is
	// handed to its worker, so a dispatched buffer is never double-put here.
	defer func() {
		if blockp != nil {
			*blockp = block
			carveArenaPool.Put(blockp)
		}
	}()

	// runCtx cancels queued/in-flight uploads on the first CommitBlock error so the
	// packing loop and the other workers stop promptly.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// sem bounds in-flight uploads; wg joins them; blocksWritten counts committed
	// blocks (workers race, so it is atomic and folded into res after the join).
	sem := make(chan struct{}, s.carveUploadConcurrency())
	var (
		wg            sync.WaitGroup
		errMu         sync.Mutex
		firstErr      error
		blocksWritten atomic.Int64
	)
	setErr := func(e error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = e
			cancel()
		}
		errMu.Unlock()
	}
	hadErr := func() bool {
		errMu.Lock()
		defer errMu.Unlock()
		return firstErr != nil
	}

	// prevFlipped chains the ordered flip: a block waits on the previous block's
	// channel before flipping and closes its own for the next. Seeded closed so the
	// first block proceeds immediately.
	prevFlipped := make(chan struct{})
	close(prevFlipped)

	// dispatch hands one packed block to an upload goroutine and returns the channel
	// the next block waits on. It transfers ownership of bp (returned to the pool
	// once CommitBlock has consumed the chunk copies). The flip runs strictly after
	// this block's commit succeeds AND the previous block's flip completed.
	dispatch := func(chunks []CarveChunk, watermark int64, bp *[]byte, prev chan struct{}) chan struct{} {
		mine := make(chan struct{})
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(mine) // release the next block's flip turn, success or not
			err := s.sink.CommitBlock(runCtx, chunks)
			carveArenaPool.Put(bp) // CommitBlock consumed the copies; recycle
			<-sem
			if err != nil {
				setErr(err)
			} else {
				blocksWritten.Add(1)
			}
			<-prev // flip in submission order, strictly after the previous block
			if err != nil || hadErr() {
				return // this block failed, or an earlier one did: do not flip
			}
			if e := s.flipUpTo(sh, id, run, &flipIdx, watermark); e != nil {
				setErr(e)
			}
		}()
		return mine
	}
	dispatchPending := func(watermark int64) {
		if len(pending) == 0 {
			return
		}
		*blockp = block // hand back the possibly-grown buffer so the pool recycles it
		prevFlipped = dispatch(pending, watermark, blockp, prevFlipped)
		pending = nil
		pendingBytes = 0
		blockp = nil // the dispatched buffer is now owned by its worker
	}

	// buf accumulates bytes for the chunker; it never exceeds one max chunk, so
	// RAM stays at FastCDC-chunk scale even for a multi-GiB run. It is recycled
	// across runs and read into directly (no separate read buffer).
	bufp := carveScratchPool.Get().(*[]byte)
	buf := (*bufp)[:0]
	defer func() {
		*bufp = buf
		carveScratchPool.Put(bufp)
	}()
	eof := false
	// Loop while the run is live: an upload failure (or the caller) cancels runCtx,
	// which stops packing so the join below can surface the error.
	for runCtx.Err() == nil {
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
				setErr(err)
				break
			}
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
		durable, err := s.deduper.IsChunkDurable(runCtx, h)
		if err != nil {
			setErr(err)
			break
		}
		if !durable {
			if blockp == nil {
				getBlock()
			}
			// Bound proof: pendingBytes < CarveBlockSize before this append (else
			// the prior iteration dispatched and reset the buffer), and boundary <=
			// MaxChunkSize, so blockOff+boundary <= CarveBlockSize-1+MaxChunkSize <
			// cap. The grow is a fail-loud belt: if that invariant ever breaks (e.g.
			// a config change), realloc rather than slice out of bounds. Already-
			// pending Data slices keep pointing at the old backing (still live), so
			// no copy is needed — the new chunk just lands in the larger buffer.
			if blockOff+boundary > cap(block) {
				block = make([]byte, blockOff+boundary)
			}
			data := block[blockOff : blockOff+boundary : blockOff+boundary]
			copy(data, buf[:boundary])
			blockOff += boundary
			pending = append(pending, CarveChunk{Hash: h, FileID: id, FileOffset: fileOff, Data: data})
			pendingBytes += int64(boundary)
			res.BytesCarved += int64(boundary)
		}
		newOffsets[fileOff] = struct{}{}
		fileOff += int64(boundary)
		buf = append(buf[:0], buf[boundary:]...)

		if pendingBytes >= s.cfg.CarveBlockSize {
			dispatchPending(fileOff)
		}
		if eof && len(buf) == 0 {
			break
		}
	}
	// Tail: dispatch any remainder; its watermark is the run end, so its flip
	// covers every record up to there. Skip when the run was cancelled (an upload
	// error, or the caller) — the join below surfaces the reason.
	if runCtx.Err() == nil {
		dispatchPending(run[len(run)-1].end())
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	res.BlocksWritten += int(blocksWritten.Load())
	// Flip any trailing records covered only by already-durable (deduped) chunks:
	// no block carried their watermark, so advance the frontier to the run end now
	// that every block committed. A no-op when the tail block already reached it.
	if err := s.flipUpTo(sh, id, run, &flipIdx, run[len(run)-1].end()); err != nil {
		return err
	}
	// #953: with every row this run produced now committed, reap the manifest rows
	// the run superseded (stale straddlers / interior chunks the fresh tiling
	// replaced). One pass at run end is correct across a multi-batch run — no single
	// batch span contains a seam-spanning straddler, and reaping the run span per
	// batch would delete a sibling batch's fresh rows. Optional: sinks without a
	// metadata store (test fakes) skip it.
	if r, ok := s.sink.(supersededReaper); ok {
		if err := r.ReapSupersededManifest(ctx, id, run[0].fileOff, run[len(run)-1].end(), newOffsets); err != nil {
			return err
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

// runReader streams a contiguous run's live bytes in file-offset order by
// preading each interval's payload. It reuses the store's positioned-read path so
// reads race nothing (segment fds are stable once created).
type runReader struct {
	s   *Store
	sh  *shard
	ivs []interval
	i   int   // current interval
	off int64 // bytes already read from the current interval
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
		n := int64(len(p))
		if n > remain {
			n = remain
		}
		got, err := rr.s.readPayload(rr.sh, iv.loc, rr.off, p[:n])
		rr.off += int64(got)
		return got, err
	}
	return 0, io.EOF
}

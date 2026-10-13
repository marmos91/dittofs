package engine

import (
	"context"
	"sort"
	"sync"

	"github.com/marmos91/dittofs/pkg/block/carver"
	"github.com/marmos91/dittofs/pkg/block/chunker"
	"github.com/marmos91/dittofs/pkg/block/journal"
	"github.com/marmos91/dittofs/pkg/block/syncer"
)

// defaultBlockUploadWindow is the fallback window for a local store exposing no
// UploadConcurrency of its own. It bounds how many of one pass's packed blocks
// are in flight at once; packing itself stays sequential. The remote path does
// not reach it — that shares the syncer's own upload limiter, so the window
// bounding PutBlock is the one the config declares.
const defaultBlockUploadWindow = 8

// flushClosure is the fn + AfterFile pair one Flush pass calls back into: the
// carver assembly, the dedup Skip hook, the ordered block uploads, and the
// pass-end manifest reap.
//
// It MUST be constructed fresh per Flush call (journal's C9 caller
// obligation): the carver and the flight chain carry state across the runs of
// one file — that is what lets one block span runs — and hoisting the
// closure to a Syncer-lifetime field would interleave two files' bytes into
// one block. Callers build it inside the Flush call they pass it to.
type flushClosure struct {
	local     journal.LocalStore
	params    chunker.Params
	blockSize int64
	deduper   Deduper
	sink      BlockSink

	cv   *carver.Carver // fresh per closure (per Flush call)
	disp *uploadChain

	// Pass-end reap state, written by fn (sequential) and read by AfterFile
	// after the last flip, all within the Flush call that owns this closure.
	committed  []journal.Extent   // durable extents reported to journal, in order
	newOffsets map[int64]struct{} // chunk offsets the pass tiled
}

// newFlushClosure builds the fn + AfterFile pair for one Flush pass. See the
// type comment for the per-call freshness obligation.
func newFlushClosure(l journal.LocalStore, params chunker.Params, blockSize int64, deduper Deduper, sink BlockSink, slots *syncer.DynamicSemaphore) (journal.FlushFunc, func(context.Context, journal.FileID) error) {
	c := &flushClosure{
		local:      l,
		params:     params,
		blockSize:  blockSize,
		deduper:    deduper,
		sink:       sink,
		newOffsets: map[int64]struct{}{},
	}
	c.disp = newUploadChain(sink, slots)
	return c.fn, c.afterFile
}

// fn offers one run through the carver, submits the blocks it emits, and
// returns the durable extents of the committed prefix — empty while uploads
// are still in flight (deferred credit, journal's C3), the resolved prefix on
// later calls, and prefix-together-with-error on a commit failure (C5).
//
// On any error it joins the flights it launched before returning, so no run
// hands an error back to the journal with uploads still running. The normal
// path needs no join of its own: collect() has already waited on every flight
// this pass submitted.
func (c *flushClosure) fn(ctx context.Context, r journal.Run) (out []journal.Extent, err error) {
	defer func() {
		if err != nil {
			c.disp.drain()
		}
	}()
	if c.cv == nil {
		c.cv = carver.New(carver.Options{
			Params:    c.params,
			BlockSize: c.blockSize,
			Skip: func(ctx context.Context, h carver.Hash) (bool, error) {
				if c.deduper == nil {
					return false, nil
				}
				return c.deduper.IsChunkDurable(ctx, h)
			},
		})
	}

	// Widen the run to the manifest row its end lands inside, so the fresh
	// tiling covers every row the pass-end reap deletes, and keep what a
	// replaced row still owns past the widened end. The tail supplies
	// residency only; the row's end is the metadata store's answer (C8).
	runEnd := r.Extent.Off + r.Extent.Len
	_, extEnd, err := c.extendToRowEnd(ctx, r, runEnd)
	if err != nil {
		return nil, err
	}
	if err := c.guardClobberedRow(ctx, r, runEnd, extEnd); err != nil {
		return nil, err
	}

	// Stream the run (and its resident extension) through the carver. Box
	// absorbs every byte it is fed (its accumulator holds the un-cut
	// residual), but tiles only the bytes it actually cut into chunks. The
	// read cursor therefore advances by bytes FED while the cut cursor —
	// Box's off parameter, which names the accumulator's first byte —
	// advances by tiled: driving both from one cursor re-feeds bytes the
	// accumulator already holds, duplicating them into every chunk they
	// span. Boxing only a full buffer (or the stream's end) keeps the
	// accumulator's residual below Min, so a chunk never spans a re-feed.
	buf := make([]byte, chunker.MaxChunkSize)
	read := r.Extent.Off // next byte to feed the carver
	cut := r.Extent.Off  // next uncut byte; the off Box cuts chunks at
	final := r.Final
	for {
		fed := 0
		if read < runEnd {
			n := min(int64(len(buf)), runEnd-read)
			var err error
			fed, err = r.ReadAt(buf[:n], read)
			if err != nil {
				return nil, err
			}
		} else if read < extEnd {
			n := min(int64(len(buf)), extEnd-read)
			var err error
			fed, _, err = c.local.ReadAt(ctx, r.ID, read, buf[:n])
			if err != nil {
				return nil, err
			}
		}
		if fed == 0 && !final {
			break
		}
		// Box always ends at the stream's end (extEnd): the below-Min tail is
		// cut there and the boundary search resets, so a chunk never spans the
		// gap between two streams — the residual bytes' true offsets lie before
		// it, and tiling them with the next run's data would assign them the
		// next run's offsets. Drain (the trailing partial block) waits for the
		// file's last call (r.Final), so a scattered set packs into whole
		// blocks instead of one partial block per run.
		isFinal := read+int64(fed) >= extEnd
		blocks, tiled, err := c.cv.Box(ctx, buf[:fed], cut, isFinal)
		if err != nil {
			return nil, err
		}
		cut += tiled
		read += int64(fed)
		for _, b := range blocks {
			c.submit(ctx, r.ID, b)
		}
		if fed == 0 {
			// The final drain ran: everything the accumulator held is cut.
			break
		}
	}
	if final {
		for _, b := range c.cv.Drain() {
			c.submit(ctx, r.ID, b)
		}
	}

	// Report the committed prefix: flights resolve in submission order, and
	// the first failure stops the chain (its own extents — committed or not —
	// and everything after it go unreported).
	extents, cerr := c.disp.collect()
	if cerr != nil {
		return extents, cerr
	}
	// Deferred credit from earlier runs rides here; record everything the
	// chain has resolved so the reap spans exactly the committed ranges.
	c.committed = append(c.committed, extents...)
	return extents, nil
}

// extendToRowEnd grows the offered run forward over the resident contiguous
// prefix of DurableTail, up to the manifest coverage's end, so the fresh
// tiling re-covers the rows the run-end reap deletes. It returns the
// extension bytes and the widened end. An extension is skipped whole when the
// tail is empty (nothing durable past the run — every append), when the first
// tail extent is not resident (evicted bytes have no local copy to re-chunk),
// or when the coverage does not reach past the run.
func (c *flushClosure) extendToRowEnd(ctx context.Context, r journal.Run, runEnd int64) ([]byte, int64, error) {
	if len(r.DurableTail) == 0 {
		return nil, runEnd, nil
	}
	ender, ok := c.sink.(ManifestRowEnder)
	if !ok {
		return nil, runEnd, nil
	}
	rowEnd, err := ender.ManifestRowEndAfter(ctx, r.ID, runEnd)
	if err != nil {
		return nil, 0, err
	}
	if rowEnd <= runEnd {
		return nil, runEnd, nil
	}
	// The extension is all-or-nothing over the contiguous resident prefix:
	// half an extension still ends inside the row, and a remote (evicted)
	// extent holds no local bytes to re-chunk.
	cur := runEnd
	var total int64
	for _, e := range r.DurableTail {
		if e.State != journal.StateResident || e.Off != cur || cur >= rowEnd {
			break
		}
		total += min(e.Len, rowEnd-cur)
		cur += min(e.Len, rowEnd-cur)
	}
	if total == 0 {
		return nil, runEnd, nil
	}
	buf := make([]byte, total)
	if _, _, err := c.local.ReadAt(ctx, r.ID, runEnd, buf); err != nil {
		return nil, 0, err
	}
	return buf, runEnd + total, nil
}

// guardClobberedRow asks the sink to re-key whatever the manifest row about to
// be replaced (a row keyed at the run's start, which the commit upserts) still
// owns past the widened run end — restricted to ranges still durable on the
// remote, since a punched hole must read as zeros. The tail supplies those
// ranges: it is exactly the durable coverage contiguous after the run.
func (c *flushClosure) guardClobberedRow(ctx context.Context, r journal.Run, runEnd, extEnd int64) error {
	if extEnd >= runEnd+r.Extent.Len && extEnd == runEnd {
		return nil
	}
	guard, ok := c.sink.(ClobberGuard)
	if !ok {
		return nil
	}
	rowEnd, err := c.sink.(ManifestRowEnder).ManifestRowEndAfter(ctx, r.ID, extEnd)
	if err != nil {
		return err
	}
	if rowEnd <= extEnd {
		return nil
	}
	var owed [][2]int64
	for _, e := range r.DurableTail {
		lo, hi := max(e.Off, extEnd), min(e.Off+e.Len, rowEnd)
		if hi <= lo {
			continue
		}
		if n := len(owed); n > 0 && lo <= owed[n-1][1] {
			owed[n-1][1] = hi
			continue
		}
		owed = append(owed, [2]int64{lo, hi})
	}
	if len(owed) == 0 {
		return nil
	}
	return guard.PreserveClobberedRow(ctx, r.ID, r.Extent.Off, extEnd, owed)
}

// submit hands one carved block to the ordered upload chain and records the
// chunk offsets it tiles for the pass-end reap. A block intentionally spans
// separate dirty runs, so its bounding Extent includes the holes between
// them — coalescing that into one reap span would punch those holes shut
// (ReapSupersededManifest requires holes between spans to remain untouched).
// The chunks are therefore grouped into contiguous file ranges and one
// durable extent per range is reported, while a single upload block ships.
func (c *flushClosure) submit(ctx context.Context, id journal.FileID, b carver.Block) {
	chunks := make([]CarveChunk, len(b.Chunks))
	for i, ch := range b.Chunks {
		chunks[i] = CarveChunk{
			Hash:       ch.Hash,
			FileID:     id,
			FileOffset: ch.Offset,
			Size:       int(ch.Size),
			Data:       ch.Data,
		}
		c.newOffsets[ch.Offset] = struct{}{}
	}
	ranges := contiguousRanges(b.Chunks)
	extents := make([]journal.Extent, len(ranges))
	for i, r := range ranges {
		extents[i] = journal.Extent{
			Off:   r[0].Offset,
			Len:   r[len(r)-1].Offset + r[len(r)-1].Size - r[0].Offset,
			State: journal.StateResident,
		}
	}
	c.disp.submit(ctx, chunks, extents)
}

// contiguousRanges groups a block's chunks into contiguous file-offset runs,
// splitting at every hole. A run ends where the next chunk's offset begins.
func contiguousRanges(chunks []carver.Chunk) [][]carver.Chunk {
	var out [][]carver.Chunk
	for start := 0; start < len(chunks); {
		end := start + 1
		for end < len(chunks) && chunks[end].Offset == chunks[end-1].Offset+chunks[end-1].Size {
			end++
		}
		out = append(out, chunks[start:end])
		start = end
	}
	return out
}

// afterFile runs once per file after the last flip, still under the shard's
// flush lock: it reaps the manifest rows the committed tiling superseded. The
// reap cannot move outside the lock — a second pass entering between the flip
// and a deferred reap commits rows inside the first pass's about-to-be-reaped
// span, and the delayed reap then deletes a load-bearing row.
func (c *flushClosure) afterFile(ctx context.Context, id journal.FileID) error {
	if len(c.committed) == 0 {
		return nil
	}
	reaper, ok := c.sink.(SupersededReaper)
	if !ok {
		return nil
	}
	return reaper.ReapSupersededManifest(ctx, id, coalesce(c.committed), c.newOffsets)
}

// coalesce merges reported extents into disjoint ascending spans.
func coalesce(extents []journal.Extent) [][2]int64 {
	sorted := make([]journal.Extent, len(extents))
	copy(sorted, extents)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Off < sorted[j].Off })
	var out [][2]int64
	for _, e := range sorted {
		lo, hi := e.Off, e.Off+e.Len
		if n := len(out); n > 0 && lo <= out[n-1][1] {
			out[n-1][1] = max(out[n-1][1], hi)
			continue
		}
		out = append(out, [2]int64{lo, hi})
	}
	return out
}

// uploadChain overlaps successive blocks' CommitBlock (upload + commit) while
// reporting their durable extents in submission order. Each flight waits for
// its predecessor's resolution before resolving itself, so collect() returns
// exactly the committed prefix: a failed block stops the chain, its own
// extents and everything after it unreported. That mirrors the deferred-credit
// contract — fn may return empty while uploads drain and reports completed
// extents on a later call — without any shared completion stream: the chain
// is per-closure (per Flush call), so one file's completions can never be
// delivered to another file's report.
type uploadChain struct {
	sink BlockSink
	// slots bounds how many blocks are in flight at once, and with them how
	// many carver arenas are live: peak flush RAM is the window, not the file
	// size. A plain field rather than a capability the sink might implement —
	// an upload window that a type assertion can silently decline to apply is
	// the same as no window at all.
	slots *syncer.DynamicSemaphore
	prev  chan struct{} // resolution of the last-submitted flight
	// wg counts launched flights and is Waited only by drain(), on the error
	// path. The normal path needs no Wait: collect() is the join there, and it
	// blocks on every flight it has not yet reported, so a run that returns
	// successfully leaves nothing inside CommitBlock. The overlap the chain
	// exists to build is between blocks WITHIN a run, and it ends at the run
	// boundary. The only escape is a run that returns early with an error
	// before reaching collect(), which is exactly what drain() closes.
	wg sync.WaitGroup

	mu        sync.Mutex
	flights   []*flight
	firstErr  error
	aborted   bool
	collected int // flights already reported by collect(); each flight reports once
}

type flight struct {
	extents  []journal.Extent
	resolved chan struct{}
	ok       bool
	err      error
}

func newUploadChain(sink BlockSink, slots *syncer.DynamicSemaphore) *uploadChain {
	prev := make(chan struct{}, 1)
	close(prev) // pre-resolved head: the first block resolves as soon as it commits
	return &uploadChain{sink: sink, slots: slots, prev: prev}
}

// submit launches one block's commit. The commit runs regardless of any
// predecessor's failure (the upload is content-addressed and harmless), but
// the flight resolves ok only if the whole prefix before it did.
//
// The slot is acquired in the CALLER, before the goroutine spawns, so a
// submission blocks once the window is full — that back-pressure is what stops
// the carver running ahead and allocating arenas nothing is waiting to free.
// It is released inside the goroutine the moment CommitBlock returns, which is
// exactly when the chunk arena backing this block dies. Releasing in submit
// instead would bound submissions rather than in-flight arenas, which is to say
// it would bound nothing: submit returns as soon as the goroutine is spawned.
// The release deliberately precedes the <-prev ordering wait, so a slow
// predecessor delays the flip but never holds a successor's memory.
//
// decision: the slot spans the whole of CommitBlock — PutBlock *and* the
// metadata commit after it — not just the upload, because what it bounds is the
// chunk arena's lifetime and the arena outlives the upload. Releasing at
// PutBlock would bound submissions rather than live arenas, which bounds
// nothing, so the span is not movable.
//
// The consequence is that window OCCUPANCY carries commit backpressure: a slow
// per-file commit holds slots while the uplink sits idle. The controller no
// longer reads that as saturation — it samples PutBlock concurrency directly
// (RemoteSync.takePutPeak) rather than this semaphore's peak — but two things
// remain true and are worth stating rather than implying:
//
//   - A slow commit still REFUSES new uploads a healthy link could carry. Only
//     the misreading was fixed, not the throttling.
//   - The sample is a high-water mark over the control interval and brackets
//     the PutBlock call, so time a client spends queued on its own connection
//     pool counts as in flight, and one brief burst of real uploads can fill
//     the window. Both are upload time, so this is honest, but it does mean a
//     single burst can read as saturation for that interval.
//
// Overturn the whole arrangement by giving arenas a lifetime independent of the
// slot; then the window would bound uploads alone and admit them while commits
// drain.
func (u *uploadChain) submit(ctx context.Context, chunks []CarveChunk, extents []journal.Extent) {
	held := false
	if u.slots != nil {
		if err := u.slots.Acquire(ctx); err != nil {
			// A cancelled acquire means this block is never submitted, so it
			// gets no flight and collect() would otherwise see nothing wrong.
			// Record it as a chain failure: journal checks cancellation at the
			// TOP of each run, so on the file's last run the loop simply ends
			// and a dropped block would return a successful flush over bytes
			// that never left — reported durable, still dirty.
			u.mu.Lock()
			u.aborted = true
			if u.firstErr == nil {
				u.firstErr = err
			}
			u.mu.Unlock()
			return
		}
		held = true
	}
	f := &flight{extents: extents, resolved: make(chan struct{})}
	mine := f.resolved
	prev := u.prev
	u.prev = mine
	u.mu.Lock()
	u.flights = append(u.flights, f)
	u.mu.Unlock()
	u.wg.Add(1)
	go func() {
		defer u.wg.Done()
		err := u.sink.CommitBlock(ctx, chunks)
		if held {
			u.slots.Release() // the arena is dead; free the slot before ordering
		}
		<-prev // predecessor resolved
		u.mu.Lock()
		prevOK := !u.aborted
		if err != nil {
			u.aborted = true
			if u.firstErr == nil {
				u.firstErr = err
			}
			f.err = err
		}
		u.mu.Unlock()
		f.ok = prevOK && err == nil
		close(mine)
	}()
}

// drain waits for every launched flight to resolve, discarding what they
// report. It is the error path's join: a run that returns early — a read
// failure, a Box failure, or collect() stopping at the first failed flight —
// otherwise leaves its siblings uploading with nobody waiting on them, and
// shutdown can then close the stores those commits write through.
//
// decision: this DRAINS rather than cancels. Cancelling would abandon commits
// that are already durable at the remote and leave the manifest not knowing
// it, which is the more expensive of the two failures; the wait is bounded
// because every flight holds an upload slot for the whole of CommitBlock, so
// at most one window's worth can be outstanding, and a shutdown has already
// cancelled the context those commits run on, which is what makes them return
// promptly rather than run to completion. Revisit if a commit is ever made to
// ignore its context, because then this bound becomes the remote's timeout
// rather than the window's.
//
// Safe to call after the last submit and only from fn's own goroutine: submit
// is caller-driven, so no Add can race this Wait.
func (u *uploadChain) drain() { u.wg.Wait() }

// collect waits for the flight chain in submission order and returns the
// resolved committed prefix. The first failed flight's error is returned
// together with the prefix before it — "these committed, then I failed".
// A collected cursor keeps each flight reporting once across runs: without
// it a file with N scattered runs re-reports its earlier extents O(N²) times
// and the pass-end coalesce/journal validation pays for every copy.
func (u *uploadChain) collect() ([]journal.Extent, error) {
	var out []journal.Extent
	u.mu.Lock()
	flights := u.flights
	collected := u.collected
	u.mu.Unlock()
	for _, f := range flights[collected:] {
		<-f.resolved
		if !f.ok {
			u.mu.Lock()
			firstErr := u.firstErr
			u.collected = len(flights)
			u.mu.Unlock()
			return out, firstErr
		}
		out = append(out, f.extents...)
	}
	u.mu.Lock()
	u.collected = len(flights)
	// A chain aborted without a failed flight is a submission that never
	// happened (a cancelled upload-slot acquire). Every flight that exists
	// resolved ok, so the loop above found nothing; the error still has to
	// reach the caller or the prefix reads as the whole file.
	firstErr := u.firstErr
	u.mu.Unlock()
	return out, firstErr
}

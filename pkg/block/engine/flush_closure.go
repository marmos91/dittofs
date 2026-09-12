package engine

import (
	"context"
	"sort"
	"sync"

	"github.com/marmos91/dittofs/pkg/block/carver"
	"github.com/marmos91/dittofs/pkg/block/chunker"
	"github.com/marmos91/dittofs/pkg/block/journal"
	"github.com/marmos91/dittofs/pkg/block/local"
)

// defaultBlockUploadWindow bounds how many of one file's packed blocks are
// committed (uploaded + committed) at once inside one flush pass. Packing
// itself stays sequential. Matches the journal's historical default.
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
	local     local.LocalStore
	params    chunker.Params
	blockSize int64
	window    int
	deduper   Deduper
	sink      BlockSink

	cv      *carver.Carver // fresh per closure (per Flush call)
	disp    *uploadChain
	fileOff int64 // stream cursor: next byte offset Box tiles

	// Pass-end reap state, written by fn (sequential) and read by AfterFile
	// after the last flip, all within the Flush call that owns this closure.
	committed  []journal.Extent   // durable extents reported to journal, in order
	newOffsets map[int64]struct{} // chunk offsets the pass tiled
}

// newFlushClosure builds the fn + AfterFile pair for one Flush pass. See the
// type comment for the per-call freshness obligation.
func newFlushClosure(l local.LocalStore, params chunker.Params, blockSize int64, window int, deduper Deduper, sink BlockSink) (journal.FlushFunc, func(context.Context, journal.FileID) error) {
	if window <= 0 {
		window = defaultBlockUploadWindow
	}
	c := &flushClosure{
		local:      l,
		params:     params,
		blockSize:  blockSize,
		window:     window,
		deduper:    deduper,
		sink:       sink,
		newOffsets: map[int64]struct{}{},
	}
	c.disp = newUploadChain(sink)
	return c.fn, c.afterFile
}

// runSpan tracks one offered run's committed frontier so the reap covers
// exactly what landed, never the range a failed upload left dirty.
type runSpan struct {
	start       int64
	committedTo int64
}

// fn offers one run through the carver, submits the blocks it emits, and
// returns the durable extents of the committed prefix — empty while uploads
// are still in flight (deferred credit, journal's C3), the resolved prefix on
// later calls, and prefix-together-with-error on a commit failure (C5).
func (c *flushClosure) fn(ctx context.Context, r journal.Run) ([]journal.Extent, error) {
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
		c.fileOff = r.Extent.Off
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

	// Stream the run (and its resident extension) through the carver in
	// max-chunk slices; the last call of the file's last run drains.
	buf := make([]byte, 0, chunker.MaxChunkSize)
	off := r.Extent.Off
	final := r.Final
	for {
		var p []byte
		if off < runEnd {
			n := min(int64(cap(buf)), runEnd-off)
			p = buf[:n]
			if _, err := r.ReadAt(p, off); err != nil {
				return nil, err
			}
		} else if off < extEnd {
			n := min(int64(cap(buf)), extEnd-off)
			p = buf[:n]
			if _, _, err := c.local.ReadAt(ctx, r.ID, off, p); err != nil {
				return nil, err
			}
		} else {
			break
		}
		blocks, tiled, err := c.cv.Box(ctx, p, off, final && off+int64(len(p)) >= extEnd)
		if err != nil {
			return nil, err
		}
		off += tiled
		for _, b := range blocks {
			c.submit(ctx, r.ID, b)
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
	for _, e := range extents {
		for o := e.Off; o < e.Off+e.Len; {
			// Chunk offsets, not byte walks: newOffsets is populated at
			// submit time from the block's chunk list; nothing to do here.
			break
		}
	}
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
// chunk offsets it tiles for the pass-end reap.
func (c *flushClosure) submit(ctx context.Context, id journal.FileID, b carver.Block) {
	chunks := make([]CarveChunk, len(b.Chunks))
	extent := journal.Extent{
		Off:   b.Chunks[0].Offset,
		Len:   b.Chunks[len(b.Chunks)-1].Offset + b.Chunks[len(b.Chunks)-1].Size - b.Chunks[0].Offset,
		State: journal.StateResident,
	}
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
	c.disp.submit(ctx, chunks, extent)
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
	prev chan struct{} // resolution of the last-submitted flight
	wg   sync.WaitGroup

	mu       sync.Mutex
	flights  []*flight
	firstErr error
	aborted  bool
}

type flight struct {
	extent   journal.Extent
	resolved chan struct{}
	ok       bool
	err      error
}

func newUploadChain(sink BlockSink) *uploadChain {
	prev := make(chan struct{}, 1)
	close(prev) // pre-resolved head: the first block resolves as soon as it commits
	return &uploadChain{sink: sink, prev: prev}
}

// submit launches one block's commit. The commit runs regardless of any
// predecessor's failure (the upload is content-addressed and harmless), but
// the flight resolves ok only if the whole prefix before it did.
func (u *uploadChain) submit(ctx context.Context, chunks []CarveChunk, extent journal.Extent) {
	f := &flight{extent: extent, resolved: make(chan struct{})}
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

// collect waits for the flight chain in submission order and returns the
// resolved committed prefix. The first failed flight's error is returned
// together with the prefix before it — "these committed, then I failed".
func (u *uploadChain) collect() ([]journal.Extent, error) {
	var out []journal.Extent
	u.mu.Lock()
	flights := u.flights
	aborted := u.aborted
	firstErr := u.firstErr
	u.mu.Unlock()
	if aborted {
		// A failure already resolved: drain the prefix before it and stop.
		for _, f := range flights {
			<-f.resolved
			if !f.ok {
				return out, firstErr
			}
			out = append(out, f.extent)
		}
		return out, firstErr
	}
	for _, f := range flights {
		<-f.resolved
		if !f.ok {
			return out, u.firstErr
		}
		out = append(out, f.extent)
	}
	return out, nil
}

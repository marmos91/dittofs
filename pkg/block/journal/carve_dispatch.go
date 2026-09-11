package journal

import (
	"context"
	"sync"
	"sync/atomic"
)

// carveDispatcher overlaps the CommitBlock (upload + commit) of one file's
// successive packed blocks while preserving the crash-safety ordering. Packing
// stays sequential in packRuns; the dispatcher only parallelizes the commits and
// then applies each block's synced flip in submission order.
//
// Two invariants make this safe to run concurrently:
//
//  1. Commit strictly precedes flip, per block. A worker calls flipUpTo only
//     after its own CommitBlock returned nil, so a record flips to synced only
//     once its block is durable remotely.
//  2. Flips apply in submission (watermark) order even if a later block's upload
//     finishes first. Each worker waits on its predecessor's completion channel
//     before flipping, so flipIdx advances monotonically and a mid-run commit
//     failure stops the watermark at the failed block: every later worker sees
//     proceed=false and skips its flip (its already-uploaded block becomes a
//     GC-reclaimable orphan, matching the PutBlock-first semantics).
//
// One chain covers the whole file, not one per dirty run, so a block that fails
// while packing run i also stops every run after it from flipping — even the
// ones whose own commits succeeded. Those commits did write manifest rows, so
// the rows they superseded stay alive alongside the fresh ones until the next
// pass. That is self-correcting rather than lost: the records were never
// flipped, so they stay unsynced, which keeps them local and unevictable, reads
// keep being served from the journal, and the next carve re-carves the same
// ranges and reaps them.
//
// Concurrency and peak RAM are bounded by sem: a block holds a slot from the
// moment it is submitted until its flip completes, so at most cap(sem) blocks —
// and thus CommitBlocks — are in flight. Peak carve RAM is cap(sem) x
// (CarveBlockSize + one overhang chunk) for the submitted blocks' arenas, plus
// one further block the carver has packed ahead of the window.
type carveDispatcher struct {
	ctx context.Context
	s   *Store
	sh  *shard
	id  FileID
	rs  []*runState // every dirty run of the file, in file-offset order
	res *CarveResult

	sem  chan struct{} // bounds in-flight blocks (buffers + concurrent commits)
	prev chan bool     // completion of the last-submitted block; feeds the next worker
	wg   sync.WaitGroup

	mu       sync.Mutex
	firstErr error
	abort    atomic.Bool
}

func newCarveDispatcher(ctx context.Context, s *Store, sh *shard, id FileID, rs []*runState, res *CarveResult, sem chan struct{}) *carveDispatcher {
	// A pre-satisfied predecessor so the first block flips as soon as it commits.
	prev := make(chan bool, 1)
	prev <- true
	return &carveDispatcher{
		ctx:  ctx,
		s:    s,
		sh:   sh,
		id:   id,
		rs:   rs,
		res:  res,
		sem:  sem,
		prev: prev,
	}
}

// hasNovelBytes reports whether any chunk carries payload. A fully deduped
// batch commits manifest rows but writes no block, so it must not count as one.
func hasNovelBytes(chunks []CarveChunk) bool {
	for _, cc := range chunks {
		if cc.Data != nil {
			return true
		}
	}
	return false
}

// submit hands a packed block to the pool. chunks may be empty, which submits a
// bare watermark advance (records covered only by already-durable chunks flip
// there) — that carries no bytes and holds no slot. The chunks' Data slices are
// backed by the carving carver's per-block arena and stay live until CommitBlock
// returns. plan names the runs the block covers and how far into the last of
// them it reached.
func (d *carveDispatcher) submit(chunks []CarveChunk, plan flipPlan) {
	// A batch of only deduped chunks carries no bytes but still commits manifest
	// rows, so its commit is throttled like any other. Giving up on a cancelled
	// context is safe: the commit below fails on the same context.
	slot := false
	if len(chunks) > 0 {
		select {
		case d.sem <- struct{}{}:
			slot = true
		case <-d.ctx.Done():
		}
	}
	mine := make(chan bool, 1)
	prev := d.prev
	d.prev = mine
	d.wg.Add(1)
	go d.commitAndFlip(chunks, plan, prev, mine, slot)
}

func (d *carveDispatcher) commitAndFlip(chunks []CarveChunk, plan flipPlan, prev, mine chan bool, slot bool) {
	defer d.wg.Done()
	if slot {
		// Release the slot only after CommitBlock has consumed the Data slices
		// (the sink copies them before returning) and the flip ran.
		defer func() { <-d.sem }()
	}

	var commitErr error
	if len(chunks) > 0 {
		commitErr = d.s.sink.CommitBlock(d.ctx, chunks)
		if commitErr != nil {
			// Stop packing as soon as any commit fails, even if this block is not
			// yet the head of the flip chain, so no further blocks are packed and
			// uploaded past a known failure. firstErr is still recorded in watermark
			// order below (only the earliest-watermark failure reaches setErr).
			d.abort.Store(true)
		}
	}

	// Wait for the predecessor before flipping so flips apply in watermark order.
	proceed := <-prev
	ok := proceed && commitErr == nil
	switch {
	case ok:
		// One block covers runs plan.first..plan.last: every run but the last is
		// covered through to its own end, the last only to the offset actually
		// packed. Each run flips through its own interval slice and its own
		// flipIdx, which is flipUpTo's existing per-run contract.
		for i := plan.first; i <= plan.last; i++ {
			wm := d.rs[i].end()
			if i == plan.last {
				wm = plan.lastOff
			}
			// This block's rows are committed, so the run's manifest is durable
			// through wm whether or not the flip below then succeeds.
			d.rs[i].committedTo = wm
			if err := d.s.flipUpTo(d.sh, d.id, d.rs[i].ivs, &d.rs[i].flipIdx, wm); err != nil {
				d.setErr(err)
				ok = false
				break
			}
		}
		// A batch carrying no novel bytes writes manifest rows but no block:
		// deduped chunks tile the range without shipping payload.
		if ok && hasNovelBytes(chunks) {
			d.res.BlocksWritten++
		}
	case proceed && commitErr != nil:
		// This block is the first failure on the ordered chain: record it. Its
		// predecessors already flipped; the watermark stops here.
		d.setErr(commitErr)
	}
	mine <- ok
}

// wait blocks until every submitted block has committed and flipped (or drained
// after a failure) and returns the first error observed, if any.
func (d *carveDispatcher) wait() error {
	d.wg.Wait()
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.firstErr
}

// aborted reports whether a commit or flip has already failed, so packing can
// stop feeding new blocks past the failed watermark.
func (d *carveDispatcher) aborted() bool { return d.abort.Load() }

func (d *carveDispatcher) setErr(err error) {
	d.mu.Lock()
	if d.firstErr == nil {
		d.firstErr = err
	}
	d.mu.Unlock()
	d.abort.Store(true)
}

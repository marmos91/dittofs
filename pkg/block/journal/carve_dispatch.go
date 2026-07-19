package journal

import (
	"context"
	"sync"
	"sync/atomic"
)

// carveDispatcher overlaps the CommitBlock (upload + commit) of one file's
// successive packed blocks while preserving the crash-safety ordering. Packing
// stays sequential in carveRun; the dispatcher only parallelizes the commits and
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
// Concurrency and peak RAM are bounded by sem: a block holds a slot (and its own
// buffer) from the moment it starts packing until its flip completes, so at most
// cap(sem) blocks — and thus CommitBlocks — are in flight, and peak carve RAM is
// cap(sem) x (CarveBlockSize + one overhang chunk).
type carveDispatcher struct {
	ctx     context.Context
	s       *Store
	sh      *shard
	id      FileID
	run     []interval
	res     *CarveResult
	flipIdx *int // advanced only by the flipping worker, one at a time via the chain

	sem  chan struct{} // bounds in-flight blocks (buffers + concurrent commits)
	prev chan bool     // completion of the last-submitted block; feeds the next worker
	wg   sync.WaitGroup

	mu       sync.Mutex
	firstErr error
	abort    atomic.Bool
}

func newCarveDispatcher(ctx context.Context, s *Store, sh *shard, id FileID, run []interval, res *CarveResult, flipIdx *int) *carveDispatcher {
	// A pre-satisfied predecessor so the first block flips as soon as it commits.
	prev := make(chan bool, 1)
	prev <- true
	return &carveDispatcher{
		ctx:     ctx,
		s:       s,
		sh:      sh,
		id:      id,
		run:     run,
		res:     res,
		flipIdx: flipIdx,
		sem:     make(chan struct{}, s.cfg.CarveUploadConcurrency),
		prev:    prev,
	}
}

// acquire reserves a concurrency slot and returns a pooled buffer with capacity
// at least arenaCap for the next block being packed. It blocks while the window
// is full and returns the context error if it is cancelled meanwhile. The slot
// is released by the block's worker (or by discard for a block never submitted).
func (d *carveDispatcher) acquire(arenaCap int) (*[]byte, error) {
	select {
	case d.sem <- struct{}{}:
	case <-d.ctx.Done():
		return nil, d.ctx.Err()
	}
	p := carveArenaPool.Get().(*[]byte)
	a := *p
	if cap(a) < arenaCap {
		a = make([]byte, arenaCap)
	}
	*p = a[:cap(a)]
	return p, nil
}

// submit hands a packed block to the pool. chunks may be empty and arenap nil,
// which submits a bare watermark advance (records covered only by already-durable
// chunks flip there) — that carries no buffer and holds no slot. arena is the
// block's final backing slice (it may have grown past the pooled buffer while
// packing), stored back into the pool on completion.
func (d *carveDispatcher) submit(chunks []CarveChunk, arenap *[]byte, arena []byte, watermark int64) {
	mine := make(chan bool, 1)
	prev := d.prev
	d.prev = mine
	d.wg.Add(1)
	go d.commitAndFlip(chunks, arenap, arena, watermark, prev, mine)
}

func (d *carveDispatcher) commitAndFlip(chunks []CarveChunk, arenap *[]byte, arena []byte, watermark int64, prev, mine chan bool) {
	defer d.wg.Done()
	if arenap != nil {
		// Free the buffer and the slot only after CommitBlock has consumed the
		// Data slices (the sink copies them before returning) and the flip ran.
		defer func() {
			*arenap = arena
			carveArenaPool.Put(arenap)
			<-d.sem
		}()
	}

	var commitErr error
	if len(chunks) > 0 {
		commitErr = d.s.sink.CommitBlock(d.ctx, chunks)
	}

	// Wait for the predecessor before flipping so flips apply in watermark order.
	proceed := <-prev
	ok := proceed && commitErr == nil
	switch {
	case ok:
		if err := d.s.flipUpTo(d.sh, d.id, d.run, d.flipIdx, watermark); err != nil {
			d.setErr(err)
			ok = false
		} else if len(chunks) > 0 {
			d.res.BlocksWritten++
		}
	case proceed && commitErr != nil:
		// This block is the first failure on the ordered chain: record it. Its
		// predecessors already flipped; the watermark stops here.
		d.setErr(commitErr)
	}
	mine <- ok
}

// discard returns an acquired-but-never-submitted buffer and its slot, used when
// packing aborts mid-block.
func (d *carveDispatcher) discard(arenap *[]byte, arena []byte) {
	if arenap == nil {
		return
	}
	*arenap = arena
	carveArenaPool.Put(arenap)
	<-d.sem
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

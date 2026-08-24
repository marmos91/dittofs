package journal

// runState carries one dirty run through a carve pass: its live intervals, as
// widened by extendRunToRowEnd, plus the packing state that outlives the run's
// own turn in the packer, since one block spans several runs.
type runState struct {
	ivs []interval
	// flipIdx is how far into ivs the durable frontier has advanced. It is
	// mutated only by the flipping worker, one at a time via the dispatcher's
	// prev/mine chain, so it needs no lock of its own.
	flipIdx int
	// newOffsets holds the offset of every chunk the run tiles (novel or
	// deduped), so the reap keeps the run's own rows and deletes only the ones
	// it superseded. Written only by the single packer goroutine.
	newOffsets map[int64]struct{}
	// committedTo is how far into the run the manifest rows are durable: the
	// watermark of the last block that committed and flipped, which is a chunk
	// boundary and so a boundary of the run's own fresh rows. It starts at the
	// run's start (nothing committed) and, like flipIdx, is written only by the
	// flipping worker, one at a time via the dispatcher's prev/mine chain.
	//
	// A run the pass abandoned half way still has one, and it is what the reap
	// spans: the records below it are already flipped synced, so no later pass
	// re-carves them and no later pass would reap the rows they superseded.
	committedTo int64
}

func (r *runState) start() int64 { return r.ivs[0].fileOff }
func (r *runState) end() int64   { return r.ivs[len(r.ivs)-1].end() }

// flipPlan names the runs one packed block contributed to. A block flushes at
// CarveBlockSize, so it may cover the tail of one run, several whole runs, and a
// prefix of the last: runs first..last-1 flip to their own end, run last flips
// only up to lastOff.
type flipPlan struct {
	first, last int
	lastOff     int64
}

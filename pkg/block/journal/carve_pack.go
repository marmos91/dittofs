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
}

func (r *runState) start() int64 { return r.ivs[0].fileOff }
func (r *runState) end() int64   { return r.ivs[len(r.ivs)-1].end() }

// complete reports whether every interval of the run reached the durable
// frontier — the run committed and flipped in full, so nothing will re-carve it.
func (r *runState) complete() bool { return r.flipIdx == len(r.ivs) }

// flipPlan names the runs one packed block contributed to. A block flushes at
// CarveBlockSize, so it may cover the tail of one run, several whole runs, and a
// prefix of the last: runs first..last-1 flip to their own end, run last flips
// only up to lastOff.
type flipPlan struct {
	first, last int
	lastOff     int64
}

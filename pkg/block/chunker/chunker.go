package chunker

// Chunker performs FastCDC content-defined chunking over a byte stream.
// It holds no cross-call state beyond its immutable sizing Params — each Next
// invocation is computed purely from its arguments and p.
type Chunker struct{ p Params }

// NewChunker returns a chunker configured with the default 1M/4M/16M profile.
func NewChunker() *Chunker { return &Chunker{p: DefaultParams()} }

// NewChunkerWithParams returns a chunker using the given sizing. Callers are
// responsible for having validated p (see Params.Validate); invalid params fall
// back to defaults so a misconfiguration degrades to the historical behaviour
// rather than producing degenerate chunks.
func NewChunkerWithParams(p Params) *Chunker {
	if p.Validate() != nil {
		p = DefaultParams()
	}
	return &Chunker{p: p}
}

// Next returns the boundary (exclusive end index) of the next chunk within data.
//
// Contract
//   - final == false: return the first breakpoint at i >= MinChunkSize where
//     (fp & mask) == 0, OR MaxChunkSize if no breakpoint is hit, OR 0 if
//     the input is shorter than MinChunkSize (caller accumulates more).
//   - final == true: if len(data) <= MinChunkSize, return (len(data), true)
//
// per (small / final chunk). Otherwise return the first breakpoint
//
//	MaxChunkSize, or len(data), whichever comes first.
//
// The returned done flag is true when the returned boundary equals len(data)
// and no further data is expected (either final==true with a short tail, or
// the breakpoint landed exactly at len(data)).
func (c *Chunker) Next(data []byte, final bool) (int, bool) {
	n := len(data)
	if n == 0 {
		return 0, final
	}

	// final chunk may be smaller than Min.
	if final && n <= c.p.Min {
		return n, true
	}

	// Not enough data to reach min; caller must accumulate (unless final
	// handled above).
	if n < c.p.Min {
		return 0, false
	}

	// Scanning window: [Min, min(n, Max)]
	end := n
	if end > c.p.Max {
		end = c.p.Max
	}

	var fp uint64
	// Warm up the gear hash up to Min without emitting.
	for i := 0; i < c.p.Min; i++ {
		fp = (fp << 1) + gearTable[data[i]]
	}

	// Small-region: apply MaskS in [Min, Avg).
	smallEnd := c.p.Avg
	if smallEnd > end {
		smallEnd = end
	}
	for i := c.p.Min; i < smallEnd; i++ {
		fp = (fp << 1) + gearTable[data[i]]
		if (fp & MaskS) == 0 {
			return i + 1, final && (i+1 == n)
		}
	}

	// Large-region: apply MaskL in [Avg, end).
	for i := smallEnd; i < end; i++ {
		fp = (fp << 1) + gearTable[data[i]]
		if (fp & MaskL) == 0 {
			return i + 1, final && (i+1 == n)
		}
	}

	// Hit max window without finding breakpoint.
	if end == c.p.Max {
		return c.p.Max, false
	}
	// Not final and we ran out without breakpoint and without hitting max
	// ask caller for more.
	if !final {
		return 0, false
	}
	// Final and no breakpoint: emit what we have.
	return n, true
}

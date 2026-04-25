package chunker

// Chunker performs FastCDC content-defined chunking over a byte stream.
// Zero value is not usable — construct via NewChunker. Chunker state is
// reset between calls; callers treat each Next invocation as stateless on
// the prior call's tail.
type Chunker struct{}

// NewChunker returns a chunker configured with the package defaults.
func NewChunker() *Chunker { return &Chunker{} }

// Reset clears internal state. Placeholder for future stateful extensions;
// currently Chunker holds no cross-call state.
func (c *Chunker) Reset() {}

// Next returns the boundary (exclusive end index) of the next chunk within data.
//
// Contract:
//   - final == false: return the first breakpoint at i >= MinChunkSize where
//     (fp & mask) == 0, OR MaxChunkSize if no breakpoint is hit, OR 0 if
//     the input is shorter than MinChunkSize (caller accumulates more).
//   - final == true: if len(data) <= MinChunkSize, return (len(data), true)
//     per D-30 (small / final chunk). Otherwise return the first breakpoint,
//     MaxChunkSize, or len(data), whichever comes first.
//
// The returned done flag is true when the returned boundary equals len(data)
// and no further data is expected (either final==true with a short tail, or
// the breakpoint landed exactly at len(data)).
func (c *Chunker) Next(data []byte, final bool) (int, bool) {
	n := len(data)
	if n == 0 {
		return 0, final
	}

	// D-30: final chunk may be smaller than MinChunkSize.
	if final && n <= MinChunkSize {
		return n, true
	}

	// Not enough data to reach min; caller must accumulate (unless final,
	// handled above).
	if n < MinChunkSize {
		return 0, false
	}

	// Scanning window: [MinChunkSize, min(n, MaxChunkSize)]
	end := n
	if end > MaxChunkSize {
		end = MaxChunkSize
	}

	var fp uint64
	// Warm up the gear hash up to MinChunkSize without emitting.
	for i := 0; i < MinChunkSize; i++ {
		fp = (fp << 1) + gearTable[data[i]]
	}

	// Small-region: apply MaskS in [MinChunkSize, AvgChunkSize).
	smallEnd := AvgChunkSize
	if smallEnd > end {
		smallEnd = end
	}
	for i := MinChunkSize; i < smallEnd; i++ {
		fp = (fp << 1) + gearTable[data[i]]
		if (fp & MaskS) == 0 {
			return i + 1, final && (i+1 == n)
		}
	}

	// Large-region: apply MaskL in [AvgChunkSize, end).
	for i := smallEnd; i < end; i++ {
		fp = (fp << 1) + gearTable[data[i]]
		if (fp & MaskL) == 0 {
			return i + 1, final && (i+1 == n)
		}
	}

	// Hit max window without finding breakpoint.
	if end == MaxChunkSize {
		return MaxChunkSize, false
	}
	// Not final and we ran out without breakpoint and without hitting max:
	// ask caller for more.
	if !final {
		return 0, false
	}
	// Final and no breakpoint: emit what we have.
	return n, true
}

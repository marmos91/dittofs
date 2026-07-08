package chunker

import "fmt"

// FastCDC parameters. Normalization level 2.
//
// NOTE (#1569): these are the DEFAULT profile only. Measured effective average
// on random data is ≈ MinChunkSize (~1 MiB), NOT AvgChunkSize — the masks below
// (MaskS popcount 15 → breakpoint ~32 KiB into the scan) fire shortly after the
// min warm-up, so AvgChunkSize/MaxChunkSize are rarely reached. MinChunkSize is
// therefore the dominant knob for effective chunk size. Do not "fix" the masks
// to genuinely hit 4 MiB: that re-chunks all existing data (different
// boundaries → different content hashes → dedup reset) and is a migration
// event, out of scope. Smaller chunks are obtained by lowering MinChunkSize via
// a per-share Params (see Params below), keeping these masks unchanged so the
// default stays byte-identical.
const (
	MinChunkSize = 1 * 1024 * 1024  // 1 MiB — smallest emitted chunk (except final)
	AvgChunkSize = 4 * 1024 * 1024  // 4 MiB — small/large mask-region boundary
	MaxChunkSize = 16 * 1024 * 1024 // 16 MiB — hard ceiling per chunk

	// NormalizationLevel controls breakpoint-mask bias toward the average.
	// Level 2 follows Xia et al. (USENIX FAST '16) normalization Table 3.
	NormalizationLevel = 2
)

// Params holds the per-chunker FastCDC sizing. The masks are shared across all
// profiles (they set a fixed ~32 KiB content-defined breakpoint search); Min is
// the dominant lever for effective chunk size, Max is the hard ceiling that
// bounds read amplification, and Avg is the small/large mask-region boundary.
//
// A random-access share lowers Min (and Max) to cut read amplification; the
// default profile keeps the historical 1M/4M/16M so existing data re-chunks
// identically. Params are a write-time, per-share property — reads never
// re-chunk (the FileChunk manifest freezes boundaries), so any Params produce
// data readable under any other Params.
type Params struct {
	Min int
	Avg int
	Max int
}

// DefaultParams returns the historical 1M/4M/16M profile — byte-identical
// chunking to pre-#1569 so existing shares keep their content hashes.
func DefaultParams() Params {
	return Params{Min: MinChunkSize, Avg: AvgChunkSize, Max: MaxChunkSize}
}

// Validate rejects nonsensical sizing. Min must be ≥ 4 KiB (below a filesystem
// page there is no point) and the ordering Min ≤ Avg ≤ Max must hold.
func (p Params) Validate() error {
	const minFloor = 4 * 1024
	if p.Min < minFloor {
		return fmt.Errorf("chunker: Min %d below floor %d", p.Min, minFloor)
	}
	if p.Avg < p.Min || p.Max < p.Avg {
		return fmt.Errorf("chunker: require Min(%d) ≤ Avg(%d) ≤ Max(%d)", p.Min, p.Avg, p.Max)
	}
	return nil
}

// Breakpoint masks for normalization level 2.
//
//   - MaskS (small-region): applied while current chunk length is in
//     [MinChunkSize, AvgChunkSize). More bits set => harder to hit
//     boundary => biases against emitting undersized chunks.
//   - MaskL (large-region): applied while current chunk length is in
//     [AvgChunkSize, MaxChunkSize). Fewer bits set => easier to hit
//     boundary => biases toward the average chunk size.
const (
	MaskS uint64 = 0x0003590703530000
	MaskL uint64 = 0x0000d90003530000
)

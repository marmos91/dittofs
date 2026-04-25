package chunker

// FastCDC parameters. Normalization level 2.
// Min/avg/max chunk sizes match restic-FastCDC's published defaults.
const (
	MinChunkSize = 1 * 1024 * 1024  // 1 MiB — smallest emitted chunk (except final)
	AvgChunkSize = 4 * 1024 * 1024  // 4 MiB — target/expected chunk size
	MaxChunkSize = 16 * 1024 * 1024 // 16 MiB — hard ceiling per chunk

	// NormalizationLevel controls breakpoint-mask bias toward the average.
	// Level 2 follows Xia et al. (USENIX FAST '16) normalization Table 3.
	NormalizationLevel = 2
)

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

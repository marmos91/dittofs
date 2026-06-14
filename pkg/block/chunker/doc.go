// Package chunker implements FastCDC content-defined chunking (Xia et al.
// USENIX ATC '16) with normalization level 2.
//
// Parameters: min=1 MiB, avg=4 MiB, max=16 MiB. Small-region mask
// MaskS biases against short chunks; large-region mask MaskL biases toward
// the average. Breakpoints are detected via a rolling Gear hash
// (gear.go). This package is consumed by the local block store's rollup
// pool (pkg/block/local/fs/rollup.go) and is pure / stateless across
// calls to NewChunker.
//
// Boundary-stability guarantee: random 1-4096 byte prefix shifts preserve
// >=70% of chunk boundaries; enforced by TestChunker_BoundaryStability_70pct.
//
// CAS key format: see ContentHash.CASKey() in pkg/block/types.go.
package chunker

// Package chunker implements FastCDC content-defined chunking (Xia et al.,
// USENIX ATC '16) with normalization level 2.
//
// Parameters (D-05): min=1 MiB, avg=4 MiB, max=16 MiB. Small-region mask
// MaskS biases against short chunks; large-region mask MaskL biases toward
// the average. Breakpoints are detected via a rolling Gear hash (see
// gear.go). This package is consumed by the local block store's rollup
// pool (pkg/blockstore/local/fs/rollup.go) and is pure / stateless across
// calls to NewChunker.
//
// Boundary-stability guarantee: random 1-4096 byte prefix shifts preserve
// >=70% of chunk boundaries (D-21, D-42); enforced by TestChunker_BoundaryStability_70pct.
//
// CAS key format (D-06): see ContentHash.CASKey() in pkg/blockstore/types.go.
package chunker

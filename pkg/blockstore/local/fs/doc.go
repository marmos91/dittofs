// Package fs implements a filesystem-based local block store for DittoFS.
//
// Writes are buffered in memory and flushed to disk atomically on NFS COMMIT
// or when memory budget is exceeded. This avoids per-4KB disk I/O and OS page
// bloat that caused OOM on servers with large local stores.
//
// Key design:
//   - Memory buffer tier: 4KB NFS writes go to in-memory []byte buffers (no disk I/O)
//   - Atomic flush: complete blocks written to .blk files with FADV_DONTNEED
//   - Backpressure: memory budget limits dirty buffers, oldest flushed first
//   - Flat addressing: blockIdx = fileOffset / 8MB
package fs

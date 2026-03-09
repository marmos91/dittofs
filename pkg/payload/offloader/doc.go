// Package offloader implements cache-to-store transfer orchestration.
//
// The offloader is responsible for moving data between the in-memory/disk cache
// and the backend block store (S3 or memory). It handles:
//
//   - Periodic upload: Scan for local blocks and upload them in the background
//   - Download: Fetch blocks from block store on cache miss, with download priority
//   - Prefetch: Speculatively fetch upcoming blocks for sequential reads
//   - Flush: Write dirty memory blocks to disk on NFS COMMIT / SMB CLOSE
//   - Content-addressed deduplication: Skip uploads when identical blocks exist
//   - Finalization callback: Notify metadata layer when all blocks are uploaded
//
// Key design principles:
//
//   - Dedicated worker pools: Separate pools for uploads and downloads prevent starvation
//   - Priority scheduling: Downloads > Uploads > Prefetch
//   - Parallel I/O: Upload/download multiple 8MB blocks concurrently
//   - Protocol agnostic: Works with both NFS COMMIT and SMB CLOSE
//   - In-flight deduplication: Avoid duplicate downloads for the same block
//   - Non-blocking: Most operations return immediately; I/O happens in background
//
// The Offloader struct is the main entry point.
// It is created via New() and requires a Cache, BlockStore, and FileBlockStore.
package offloader

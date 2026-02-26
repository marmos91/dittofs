// Package offloader implements cache-to-store transfer orchestration.
//
// The offloader is responsible for moving data between the in-memory/mmap cache
// and the durable block store (S3, filesystem, etc.). It handles:
//
//   - Eager upload: Upload complete 4MB blocks immediately in background goroutines
//   - Download: Fetch blocks from block store on cache miss, with download priority
//   - Prefetch: Speculatively fetch upcoming blocks for sequential reads
//   - Flush: Upload remaining partial blocks on NFS COMMIT / SMB CLOSE
//   - Content-addressed deduplication: Skip uploads when identical blocks exist
//   - Finalization callback: Notify metadata layer when all blocks are uploaded
//
// Key design principles:
//
//   - Unified queue: All transfers (upload, download, prefetch) use a single worker pool
//   - Priority scheduling: Downloads > Uploads > Prefetch
//   - Parallel I/O: Upload/download multiple blocks concurrently
//   - Protocol agnostic: Works with both NFS COMMIT and SMB CLOSE
//   - In-flight deduplication: Avoid duplicate downloads for the same block
//   - Non-blocking: Most operations return immediately; I/O happens in background
//
// The Offloader struct is the main entry point.
// It is created via New() and requires a Cache, BlockStore, and ObjectStore.
package offloader

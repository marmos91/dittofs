// Package sync implements local-to-remote transfer orchestration.
//
// The syncer is responsible for moving data between the local store
// and the remote block store (S3 or memory). It handles:
//
//   - Periodic sync: Scan for local blocks and upload them in the background
//   - Fetch: Retrieve blocks from remote store on local miss, with download priority
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
// The Syncer struct is the main entry point.
// It is created via New() and requires a LocalStore, RemoteStore, and FileBlockStore.
//
// IMPORTANT: This package is named "sync" which shadows Go's standard library
// sync package. All files in this package use the import alias:
//
//	import gosync "sync"
package sync

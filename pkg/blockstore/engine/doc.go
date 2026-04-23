// Package engine provides the BlockStore orchestrator that composes local
// store, remote store, syncer, read buffer / prefetcher, and block garbage
// collection into the blockstore.Store interface.
//
// The orchestrator lives in a sub-package (not the root blockstore package) to
// avoid import cycles: blockstore/local and the former blockstore/sync both
// import the root blockstore package for types and interfaces, so the root
// package cannot import them back.
//
// As of the TD-01 merge (Phase 08), the previously separate sibling packages
// pkg/blockstore/{readbuffer,sync,gc} are folded into this package. The merge
// preserves all public behaviour; only the package path, a handful of
// collision-resolving type/function names, and the package-qualified call
// sites changed.
//
// # Read buffer and prefetch (formerly pkg/blockstore/readbuffer)
//
// ReadBuffer is an LRU block buffer that stores full 8MB blocks (matching
// blockstore.BlockSize) as heap-allocated []byte slices with copy-on-read
// semantics. It uses RWMutex for concurrent access: reads take RLock,
// mutations take WLock. Eviction is synchronous and inline during Put
// (dropping a []byte reference is O(1), no I/O needed).
//
// Each per-share BlockStore gets its own ReadBuffer instance, ensuring one
// share's sequential scan cannot evict another share's working set. Setting
// maxBytes to 0 disables the read buffer entirely (NewReadBuffer returns nil).
//
// A secondary index (payloadID -> set of blockIdx) enables O(1) lookup of all
// buffered blocks for a file, allowing efficient per-file invalidation
// (O(buffered_blocks_for_file)) for delete and truncate operations.
//
// Prefetcher detects sequential access patterns per file and pre-loads
// upcoming blocks into the ReadBuffer using a bounded worker pool. It uses
// adaptive depth (1->2->4->8 blocks) following the Linux readahead pattern.
// Non-blocking submit drops requests when the worker channel is full,
// providing natural backpressure.
//
// # Syncer (formerly pkg/blockstore/sync)
//
// The syncer is responsible for moving data between the local store and the
// remote block store (S3 or memory). It handles:
//
//   - Periodic sync: Scan for local blocks and upload them in the background
//   - Fetch: Retrieve blocks from remote store on local miss, with download priority
//   - Prefetch: Speculatively fetch upcoming blocks for sequential reads
//   - Flush: Write dirty memory blocks to disk on NFS COMMIT / SMB CLOSE
//   - Content-addressed deduplication: Skip uploads when identical blocks exist
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
// The Syncer struct is created via NewSyncer() and requires a LocalStore,
// RemoteStore, and FileBlockStore.
//
// # Block garbage collection (formerly pkg/blockstore/gc)
//
// Orphan blocks are blocks that exist in the block store but have no
// corresponding metadata. This can happen when file deletion fails after
// metadata is removed but before blocks are deleted, or when the server
// crashes during file deletion.
//
// The CollectGarbage function scans the block store, groups blocks by
// payloadID, and checks each payloadID against the metadata store. Blocks
// without metadata are deleted.
//
// Usage:
//
//	// Dry run first
//	dryStats := engine.CollectGarbage(ctx, remoteStore, registry, &engine.Options{DryRun: true})
//	logger.Info("Would delete", "orphanBlocks", dryStats.OrphanBlocks)
//
//	// Then actually delete
//	stats := engine.CollectGarbage(ctx, remoteStore, registry, nil)
//
// The garbage collector has zero coupling to the Syncer - it only needs a
// RemoteStore and a MetadataReconciler to check metadata existence.
package engine

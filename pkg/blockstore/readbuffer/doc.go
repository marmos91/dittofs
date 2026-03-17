// Package readbuffer provides an in-memory read buffer for hot blocks
// and a sequential prefetch system for the DittoFS BlockStore engine.
//
// ReadBuffer is an LRU block buffer that stores full 8MB blocks (matching
// blockstore.BlockSize) as heap-allocated []byte slices with copy-on-read
// semantics. It uses RWMutex for concurrent access: reads take RLock,
// mutations take WLock. Eviction is synchronous and inline during Put
// (dropping a []byte reference is O(1), no I/O needed).
//
// Each per-share BlockStore gets its own ReadBuffer instance, ensuring one
// share's sequential scan cannot evict another share's working set.
// Setting maxBytes to 0 disables the read buffer entirely (New returns nil).
//
// A secondary index (payloadID -> set of blockIdx) enables O(1) lookup of
// all buffered blocks for a file, allowing efficient per-file invalidation
// (O(number_of_buffered_blocks_for_file)) for delete and truncate operations.
//
// Prefetcher detects sequential access patterns per file and pre-loads
// upcoming blocks into the ReadBuffer using a bounded worker pool. It uses
// adaptive depth (1->2->4->8 blocks) following the Linux readahead pattern.
// Non-blocking submit drops requests when the worker channel is full,
// providing natural backpressure.
package readbuffer

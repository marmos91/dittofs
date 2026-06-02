// Package common provides shared helpers used by every protocol adapter
// (NFSv3, NFSv4, SMB2/3) so that block-store resolution, pooled read
// buffers, and metadata→protocol error mapping live in exactly one place.
//
// # Narrow interfaces, not *runtime.Runtime
//
// Helpers accept BlockStoreRegistry (and a narrow MetadataService interface)
// instead of *runtime.Runtime. This keeps common/ testable with trivial mocks
// and avoids a circular import with pkg/controlplane/runtime. The concrete
// *runtime.Runtime satisfies these interfaces implicitly — no runtime change
// is required.
//
// # Pool over-cap fallback
//
// ReadFromBlockStore allocates its response buffer via internal/adapter/pool.
// The pool has 4 KB / 64 KB / 1 MB tiers and falls through to a direct
// make([]byte, size) allocation when size exceeds LargeSize (pool.Get at
// bufpool.go:157-162). pool.Put silently ignores oversized/undersized
// buffers. As a result:
//
//   - Today every DittoFS read fits the 1 MB LargeSize tier (MaxReadSize
//     is 1 MB on both NFS and SMB), so over-cap fallback is dormant.
//   - If a future change raises MaxReadSize toward the SMB 3.1.1 ceiling
//     (8 MB), the pool continues to work correctly via the direct-alloc
//     fallback; no handler code change is required.
//   - We deliberately do NOT bump LargeSize speculatively — sync.Pool is
//     per-P and bumping to 8 MB would pin ~128 MB of idle pool memory on a
//     16-core host for an optimization that does not fire at the current
//     negotiated cap. Revisit when a perf profile shows large reads
//     dominating.
//
// # []BlockRef plumbing
//
// The helpers ReadFromBlockStore, WriteToBlockStore, and CommitBlockStore are
// the canonical fan-in points for []BlockRef plumbing. The engine wires
// engine.Store.ReadAt / WriteAt with `[]BlockRef` parameter on ReadAt
// and `[]BlockRef` returned from WriteAt, plus a post-transaction
// CacheInvalidator interface and the CopyPayload helper.
//
// Caller-snapshot []BlockRef threading from FileAttr.Blocks into
// engine.ReadAt / WriteAt is deferred: common.ReadFromBlockStore and
// common.WriteToBlockStore continue passing nil []BlockRef so the engine
// routes through the dual-read shim. The actual snapshot threading lands
// when the engine's cache rewrite exposes a coordinator-side
// GetBlocksForPayload accessor (which avoids re-introducing a metadata
// dependency at the adapter call sites).
//
// Wiring of common.CopyPayload into NFS/SMB CREATE-file copy paths is also
// deferred: file-level dedup will route copy operations through this helper.
// The helper exists with full test coverage and is consumed only by tests.
//
// # Caller-snapshot wins
//
// Once threading lands, the engine trusts the []BlockRef the adapter handed
// it. If the snapshot is stale (a concurrent WriteAt updated FileAttr.Blocks
// after the snapshot was taken), the read returns bytes per the snapshot
// (or zero-fills past the snapshot's last offset). Snapshot freshness is
// the caller's responsibility via metadata transaction isolation. This
// avoids a per-read metadata round-trip on the hot path.
//
// # Post-transaction cache invalidation
//
// Cache invalidation is post-transaction by design: caller commits the
// metadata transaction first (new BlockRefs persisted), then invokes
// CacheInvalidator.InvalidateFile with the diff between the old and new
// BlockRef hashes. If invalidation ran pre-commit and the transaction
// later rolled back, warm cache entries would be dropped unnecessarily.
// Post-txn ordering ensures the cache reflects metadata truth.
//
// The CacheInvalidator interface (cache_invalidator.go) is defined in this
// package, not imported from pkg/blockstore/engine, so common helpers stay
// decoupled from the concrete cache type. The engine.Cache implements this
// interface implicitly via its InvalidateFile method.
//
// # Engine contract consumed by these helpers
//
//	ReadAt(ctx, payloadID, []BlockRef, dest, offset) (int, error)
//	WriteAt(ctx, payloadID, currentBlocks []BlockRef, data, offset) ([]BlockRef, error)
//	CopyPayload(ctx, srcPayloadID, dstPayloadID, srcBlocks []BlockRef) ([]BlockRef, error)
//	Flush(ctx, payloadID) (*block.FlushResult, error)
//
// Empty / nil []BlockRef on Read/Write triggers the dual-read shim.
// Non-empty triggers the CAS path with BLAKE3 verification (engine-internal).
package common

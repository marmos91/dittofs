// Package common provides shared helpers used by every protocol adapter
// (NFSv3, NFSv4, SMB2/3) so that block-store resolution, pooled read
// buffers, and metadata→protocol error mapping live in exactly one place.
//
// # Narrow interfaces, not *runtime.Runtime
//
// Helpers accept BlockStoreRegistry (and a narrow MetadataService interface
// added in later plans) instead of *runtime.Runtime. This keeps common/
// testable with trivial mocks and avoids a circular import with
// pkg/controlplane/runtime. The concrete *runtime.Runtime satisfies these
// interfaces implicitly — no runtime change is required.
//
// # Pool over-cap fallback (ADAPT-02 / D-11)
//
// ReadFromBlockStore allocates its response buffer via internal/adapter/pool.
// The pool has 4 KB / 64 KB / 1 MB tiers and falls through to a direct
// make([]byte, size) allocation when size exceeds LargeSize (pool.Get at
// bufpool.go:157-162). pool.Put silently ignores oversized/undersized
// buffers. As a result:
//
//   - Today every DittoFS read fits the 1 MB LargeSize tier (MaxReadSize
//     is 1 MB on both NFS and SMB), so over-cap fallback is dormant.
//   - If a future phase raises MaxReadSize toward the SMB 3.1.1 ceiling
//     (8 MB), the pool continues to work correctly via the direct-alloc
//     fallback; no handler code change is required.
//   - We deliberately do NOT bump LargeSize speculatively — sync.Pool is
//     per-P and bumping to 8 MB would pin ~128 MB of idle pool memory on a
//     16-core host for an optimization that does not fire at the current
//     negotiated cap. Revisit when a perf profile shows large reads
//     dominating.
//
// # Phase-12 seam for []BlockRef (ADAPT-04 / D-12)
//
// Adapter call sites today speak the current
// engine.BlockStore.ReadAt(ctx, payloadID, dest, offset) signature — the
// wire protocols (NFS3/4, SMB2/3) communicate (offset, length) and know
// nothing about blocks. Phase 12 (META-01 + API-01) reintroduces
// FileAttr.Blocks as []BlockRef and changes the engine signature to take
// resolved []BlockRef. The fetch-and-slice logic will land inside
// ReadFromBlockStore / WriteToBlockStore in exactly one place; protocol
// handlers remain untouched.
package common

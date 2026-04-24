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
// The helpers ReadFromBlockStore, WriteToBlockStore, and CommitBlockStore are
// the single place where Phase 12 (#423) will add []BlockRef plumbing. Today
// these helpers are thin passthroughs to engine.BlockStore.ReadAt /
// WriteAt / Flush, which accept (ctx, payloadID, buf, offset) — exactly what
// the wire protocols (NFS3 READ, NFS4 READ, SMB2 READ; NFS3 WRITE, NFS4
// WRITE, SMB2 WRITE; NFS3 COMMIT, NFS4 COMMIT, SMB2 CLOSE flush) hand us.
//
// In Phase 12:
//   - META-01 reintroduces FileAttr.Blocks as []BlockRef (sorted by offset,
//     populated at sync finalization).
//   - API-01 changes engine.BlockStore.ReadAt / WriteAt to accept []BlockRef
//     instead of (payloadID, offset). A binary search on []BlockRef resolves
//     the (offset, length) range to the chunks covering it.
//
// The Phase-12 change to ReadFromBlockStore and WriteToBlockStore will be,
// essentially:
//
//  1. fetch FileAttr.Blocks via the narrow MetadataService interface
//  2. slice the []BlockRef list to the range covering [offset, offset+len)
//  3. pass the resolved slice to the new engine.ReadAt/WriteAt signature
//
// Every protocol handler code path is UNCHANGED by Phase 12 because they all
// call common.ReadFromBlockStore / common.WriteToBlockStore /
// common.CommitBlockStore. Wire protocol fidelity is preserved — handlers
// continue to receive and emit (offset, length) on the wire. NFS and SMB
// have no concept of blocks; []BlockRef remains internal plumbing between
// the adapter and the engine.
//
// Phase 09 engine contract (unchanged in this phase):
//
//	ReadAt(ctx, payloadID string, data []byte, offset uint64) (int, error)
//	WriteAt(ctx, payloadID string, data []byte, offset uint64) error
//	Flush(ctx, payloadID string) (*blockstore.FlushResult, error)
//
// Note the asymmetry: ReadAt returns (int, error) because short reads are
// observable by the caller (EOF handling); WriteAt returns error only —
// successful writes are full-length by contract, and partial writes surface
// as an error. WriteToBlockStore mirrors this exactly: a nil return means
// the full data slice was persisted at offset.
package common

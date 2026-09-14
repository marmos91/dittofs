// Package journal is the local block cache for a share: it packs many files
// into a small set of shared, append-only segment files on disk and mediates
// between dirty client writes, fsync-durable checkpoints, flushing to a remote
// store, pressure-gated eviction, and garbage collection.
//
// It owns all persistent local-cache state. It imports only the standard
// library and golang.org/x/sys — TestNoForeignImports enforces that, so the
// claim cannot rot — with the Clock and Logger injected through [Config]. It
// knows nothing about namespaces, protocols, permissions, content hashing or
// the metadata store: callers resolve logical offsets to FileIDs and hand
// journal opaque byte ranges.
//
// The flush seam is deliberately content-agnostic. [Store.Flush] offers each
// contiguous dirty run to the caller's function as a [Run] and flips the
// fragments that function reports durable; what a block is, how bytes are
// chunked, hashed, deduped or uploaded is entirely the caller's business.
// That is why no chunking profile lives here.
//
// The unifying model: client writes (WriteAt) and cold-read hydration
// (Hydrate) both funnel through one internal append primitive, differing only
// in whether the record is born clean (already durable in the remote store) or
// dirty (must be flushed before it can be evicted).
//
// GC never touches the remote store: repack relocates local cache bytes
// between segments only (see reclaim.go). Remote-block refcount reclamation
// stays with the engine's block-GC sweep, whose per-remote serialization is
// what makes a decrement safe — journal must not drive one concurrently.
package journal

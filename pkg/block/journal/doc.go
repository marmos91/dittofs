// Package journal is the local block cache for a share: it packs many files
// into a small set of shared, append-only segment files on disk and mediates
// between dirty client writes, fsync-durable checkpoints, background carving to
// a remote store, pressure-gated eviction, and garbage collection.
//
// It owns all persistent local-cache state and depends only on the standard
// library plus a pair of narrow injected interfaces (RemoteStore, Clock). It
// knows nothing about namespaces, protocols, permissions, or the metadata
// store — callers resolve logical offsets to FileIDs and hand journal opaque
// byte ranges.
//
// The unifying model: client writes (WriteAt) and cold-read hydration
// (Hydrate) both funnel through one internal append primitive, differing only
// in whether the record is born clean (already durable in the remote store) or
// dirty (must be carved before it can be evicted).
//
// This package is built incrementally. The append/read/commit, recovery, carve
// and pressure-gated eviction paths are live; gc and Delete are stubbed and
// return errNotImplemented until their respective changes land.
package journal

// Package blockstoretest provides a unified conformance suite for the
// block.Store contract declared in pkg/block/blockstore.go.
//
// Two top-level entrypoints are exposed:
//
//   - BlockStoreConformance(t, factory) — runs the CAS-keyed contract
//     suite against any block.Store implementation. The s3 and memory
//     backends and the compression / encryption decorators call it.
//   - RemoteBlockStoreConformance(t, factory) — runs the block-keyed
//     (non-CAS) contract suite against any RemoteBlockStore
//     implementation. The memory and s3 backends both call this
//     entrypoint.
//
// There is no append-log conformance entrypoint. The local tier
// (*fs.FSStore) is not hash-keyed — it exposes the payload-keyed
// local.LocalStore surface (WriteAt / ReadAt / Hydrate / Commit) and so
// implements neither block.Store nor RemoteBlockStore. It is covered by
// its own package-local tests in pkg/block/local/fs and by the journal
// suites, not by anything here.
//
// Each scenario uses a factory that returns a fresh (store, cleanup)
// pair per subtest, so subtests do not share state and teardown is
// deterministic. See conformance.go and remoteblock.go for the factory
// type definitions.
package blockstoretest

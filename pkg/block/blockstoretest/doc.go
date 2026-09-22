// Package blockstoretest provides the conformance suite for the
// block-keyed remote store contract, pkg/block/remote.RemoteBlockStore.
//
// One entrypoint is exposed:
//
//   - RemoteBlockStoreConformance(t, factory) — runs the block-keyed
//     (non-CAS) contract suite. The memory and s3 backends both call it.
//
// There is no hash-keyed entrypoint. The CAS block.Store interface and its
// BlockStoreConformance suite were deleted; only orphan godoc for the
// interface survives in pkg/block/blockstore.go.
//
// There is no append-log entrypoint either. The local tier (*journal.Store)
// is payload-keyed — it exposes local.LocalStore (WriteAt / ReadAt / Hydrate
// / Commit) — and is covered by the journal suites and its own package tests.
//
// Each scenario uses a factory that returns a fresh (store, cleanup) pair per
// subtest, so subtests do not share state and teardown is deterministic. See
// remoteblock.go for the factory type definition.
package blockstoretest

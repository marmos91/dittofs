// Package blockstore defines the core types, interfaces, and errors for DittoFS
// block storage. It is the single source of truth for FileBlock, BlockState,
// ContentHash, and BlockSize -- shared across metadata stores, local stores,
// syncer, and remote block stores.
//
// Sub-packages:
//   - local: LocalStore interface for on-node storage (memory + disk)
//   - remote: RemoteStore interface for durable backend storage (S3, etc.)
//   - sync: Syncer for local-to-remote transfer orchestration
//   - engine: BlockStore engine composing local store, syncer, and metadata
//   - gc: Block garbage collection
//   - storetest: Conformance test suites for FileBlockStore implementations
package blockstore

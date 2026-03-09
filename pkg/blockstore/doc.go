// Package blockstore defines the core types, interfaces, and errors for DittoFS
// block storage. It is the single source of truth for FileBlock, BlockState,
// ContentHash, and BlockSize -- shared across metadata stores, cache layer,
// offloader, and remote block stores.
//
// Sub-packages:
//   - local: LocalStore interface for on-node cache (memory + disk)
//   - remote: RemoteStore interface for durable backend storage (S3, etc.)
//   - storetest: Conformance test suites for FileBlockStore implementations
package blockstore

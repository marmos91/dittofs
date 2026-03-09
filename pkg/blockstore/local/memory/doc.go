// Package memory provides a pure in-memory LocalStore implementation.
//
// MemoryStore stores all block data in memory with no disk persistence.
// It implements the full local.LocalStore interface for testing and ephemeral
// use cases where durability is not required.
//
// This store is useful for:
//   - Unit tests that need a fast, isolated LocalStore
//   - Running the conformance test suite (localtest)
//   - Ephemeral configurations where data loss on restart is acceptable
//
// Thread-safe: all operations are protected by a sync.RWMutex.
package memory

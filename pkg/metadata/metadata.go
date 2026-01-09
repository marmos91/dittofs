// Package metadata provides the core metadata types and operations for DittoFS.
//
// This package contains:
//   - Core types: File, FileAttr, FileHandle, ContentID
//   - Store interface: MetadataStore, Transaction, Transactor
//   - Business logic: RemoveFile, Move, Create, Lookup operations
//   - Permissions: Unix-style permission checking
//
// Store implementations are in subpackages:
//   - pkg/metadata/store/memory - In-memory store (for testing)
//   - pkg/metadata/store/badger - BadgerDB persistent store
//   - pkg/metadata/store/postgres - PostgreSQL distributed store
package metadata

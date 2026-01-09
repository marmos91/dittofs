package memory

import (
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// File Content Coordination
// ============================================================================
//
// These methods delegate to the shared operations in pkg/metadata/operations.go
// The business logic is centralized there; these are thin wrappers.

// PrepareWrite validates a write operation and returns a write intent.
//
// Delegates to metadata.PrepareWriteOp for centralized business logic.
func (s *MemoryMetadataStore) PrepareWrite(
	ctx *metadata.AuthContext,
	handle metadata.FileHandle,
	newSize uint64,
) (*metadata.WriteOperation, error) {
	return metadata.PrepareWriteOp(s, ctx, handle, newSize)
}

// CommitWrite applies metadata changes after a successful content write.
//
// Delegates to metadata.CommitWriteOp for centralized business logic.
func (s *MemoryMetadataStore) CommitWrite(
	ctx *metadata.AuthContext,
	intent *metadata.WriteOperation,
) (*metadata.File, error) {
	return metadata.CommitWriteOp(s, ctx, intent)
}

// PrepareRead validates a read operation and returns file metadata.
//
// Delegates to metadata.PrepareReadOp for centralized business logic.
func (s *MemoryMetadataStore) PrepareRead(
	ctx *metadata.AuthContext,
	handle metadata.FileHandle,
) (*metadata.ReadMetadata, error) {
	return metadata.PrepareReadOp(s, ctx, handle)
}

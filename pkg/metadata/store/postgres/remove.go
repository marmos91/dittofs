package postgres

import (
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// File/Directory Removal Operations
// ============================================================================
//
// These methods delegate to the shared operations in pkg/metadata/operations.go
// The business logic is centralized there; these are thin wrappers.

// RemoveFile removes a file's metadata from its parent directory.
//
// Delegates to metadata.RemoveFileOp for centralized business logic.
func (s *PostgresMetadataStore) RemoveFile(
	ctx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	name string,
) (*metadata.File, error) {
	return metadata.RemoveFileOp(s, ctx, parentHandle, name)
}

// RemoveDirectory removes an empty directory's metadata from its parent.
//
// Delegates to metadata.RemoveDirectoryOp for centralized business logic.
func (s *PostgresMetadataStore) RemoveDirectory(
	ctx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	name string,
) error {
	return metadata.RemoveDirectoryOp(s, ctx, parentHandle, name)
}

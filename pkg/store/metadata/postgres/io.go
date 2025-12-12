package postgres

import (
	"time"

	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// PrepareWrite validates a write operation and returns a write intent
func (s *PostgresMetadataStore) PrepareWrite(
	ctx *metadata.AuthContext,
	handle metadata.FileHandle,
	newSize uint64,
) (*metadata.WriteOperation, error) {
	// Decode handle
	shareName, fileID, err := decodeFileHandle(handle)
	if err != nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid file handle",
		}
	}

	// Get file
	file, err := s.getFileByID(ctx.Context, fileID, shareName)
	if err != nil {
		return nil, err
	}

	// Check write permission
	if err := s.checkAccess(file, ctx, metadata.PermissionWrite); err != nil {
		return nil, err
	}

	// Verify it's a regular file
	if file.Type != metadata.FileTypeRegular {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrIsDirectory,
			Message: "cannot write to non-regular file",
			Path:    file.Path,
		}
	}

	// Build WriteOperation
	writeOp := &metadata.WriteOperation{
		Handle:       handle,
		NewSize:      newSize,
		NewMtime:     time.Now(), // Compute mtime as current time
		ContentID:    file.ContentID,
		PreWriteAttr: &file.FileAttr,
	}

	return writeOp, nil
}

// CommitWrite applies metadata changes after a successful content write
func (s *PostgresMetadataStore) CommitWrite(
	ctx *metadata.AuthContext,
	intent *metadata.WriteOperation,
) (*metadata.File, error) {
	// Decode handle
	shareName, fileID, err := decodeFileHandle(intent.Handle)
	if err != nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid file handle",
		}
	}

	// Update file with new size and mtime
	// Use GREATEST to handle concurrent writes - only grow the file size, never shrink
	// This prevents race conditions where out-of-order write completions could
	// incorrectly reduce the file size
	now := time.Now()
	updateQuery := `
		UPDATE files
		SET size = GREATEST(size, $1), mtime = $2, ctime = $3
		WHERE id = $4 AND share_name = $5
	`

	_, err = s.pool.Exec(ctx.Context, updateQuery,
		int64(intent.NewSize),
		intent.NewMtime,
		now,
		fileID,
		shareName,
	)
	if err != nil {
		return nil, mapPgError(err, "CommitWrite", "")
	}

	// Invalidate stats cache
	s.statsCache.invalidate()

	// Get updated file
	file, err := s.getFileByID(ctx.Context, fileID, shareName)
	if err != nil {
		return nil, err
	}

	return file, nil
}

// PrepareRead validates a read operation and returns file metadata
func (s *PostgresMetadataStore) PrepareRead(
	ctx *metadata.AuthContext,
	handle metadata.FileHandle,
) (*metadata.ReadMetadata, error) {
	// Get file
	file, err := s.GetFile(ctx.Context, handle)
	if err != nil {
		return nil, err
	}

	// Check read permission
	if err := s.checkAccess(file, ctx, metadata.PermissionRead); err != nil {
		return nil, err
	}

	// Verify it's a regular file
	if file.Type != metadata.FileTypeRegular {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrIsDirectory,
			Message: "cannot read non-regular file",
			Path:    file.Path,
		}
	}

	// Return read metadata with ContentID
	readMeta := &metadata.ReadMetadata{
		Attr: &file.FileAttr,
	}

	return readMeta, nil
}

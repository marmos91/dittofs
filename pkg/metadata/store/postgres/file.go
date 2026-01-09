package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// GetFile retrieves a file by its handle
func (s *PostgresMetadataStore) GetFile(ctx context.Context, handle metadata.FileHandle) (*metadata.File, error) {
	// Decode the file handle
	shareName, id, err := decodeFileHandle(handle)
	if err != nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid file handle",
		}
	}

	// Query the file from the database, including link count
	query := `
		SELECT
			f.id, f.share_name, f.path,
			f.file_type, f.mode, f.uid, f.gid, f.size,
			f.atime, f.mtime, f.ctime, f.creation_time,
			f.content_id, f.link_target, f.device_major, f.device_minor,
			f.hidden, lc.link_count
		FROM files f
		LEFT JOIN link_counts lc ON f.id = lc.file_id
		WHERE f.id = $1 AND f.share_name = $2
	`

	row := s.pool.QueryRow(ctx, query, id, shareName)
	file, err := fileRowToFileWithNlink(row)
	if err != nil {
		return nil, mapPgError(err, "GetFile", "")
	}

	return file, nil
}

// GetFileByID retrieves a file by its UUID (internal use)
func (s *PostgresMetadataStore) getFileByID(ctx context.Context, id uuid.UUID, shareName string) (*metadata.File, error) {
	query := `
		SELECT
			f.id, f.share_name, f.path,
			f.file_type, f.mode, f.uid, f.gid, f.size,
			f.atime, f.mtime, f.ctime, f.creation_time,
			f.content_id, f.link_target, f.device_major, f.device_minor,
			f.hidden, lc.link_count
		FROM files f
		LEFT JOIN link_counts lc ON f.id = lc.file_id
		WHERE f.id = $1 AND f.share_name = $2
	`

	row := s.pool.QueryRow(ctx, query, id, shareName)
	file, err := fileRowToFileWithNlink(row)
	if err != nil {
		return nil, mapPgError(err, "getFileByID", "")
	}

	return file, nil
}

// GetFileByContentID retrieves a file by its content ID (used by cache flusher)
func (s *PostgresMetadataStore) GetFileByContentID(ctx context.Context, contentID metadata.ContentID) (*metadata.File, error) {
	if contentID == "" {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "content ID cannot be empty",
		}
	}

	query := `
		SELECT
			f.id, f.share_name, f.path,
			f.file_type, f.mode, f.uid, f.gid, f.size,
			f.atime, f.mtime, f.ctime, f.creation_time,
			f.content_id, f.link_target, f.device_major, f.device_minor,
			f.hidden, lc.link_count
		FROM files f
		LEFT JOIN link_counts lc ON f.id = lc.file_id
		WHERE f.content_id = $1
		LIMIT 1
	`

	row := s.pool.QueryRow(ctx, query, string(contentID))
	file, err := fileRowToFileWithNlink(row)
	if err != nil {
		return nil, mapPgError(err, "GetFileByContentID", string(contentID))
	}

	return file, nil
}

// Lookup finds a file by name in a parent directory
func (s *PostgresMetadataStore) Lookup(ctx *metadata.AuthContext, parentHandle metadata.FileHandle, name string) (*metadata.File, error) {
	// Validate input
	if name == "" {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "file name cannot be empty",
		}
	}

	if name == "." || name == ".." {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "lookup of '.' and '..' not supported",
		}
	}

	// Decode parent handle
	shareName, parentID, err := decodeFileHandle(parentHandle)
	if err != nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid parent handle",
		}
	}

	// Check if parent is a directory (with execute permission)
	parent, err := s.getFileByID(ctx.Context, parentID, shareName)
	if err != nil {
		return nil, err
	}

	if parent.Type != metadata.FileTypeDirectory {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrNotDirectory,
			Message: "parent is not a directory",
			Path:    parent.Path,
		}
	}

	// Check execute permission on parent directory
	if err := s.checkAccess(parent, ctx, metadata.PermissionExecute); err != nil {
		return nil, err
	}

	// Query child file with link count
	query := `
		SELECT
			f.id, f.share_name, f.path,
			f.file_type, f.mode, f.uid, f.gid, f.size,
			f.atime, f.mtime, f.ctime, f.creation_time,
			f.content_id, f.link_target, f.device_major, f.device_minor,
			f.hidden, lc.link_count
		FROM files f
		INNER JOIN parent_child_map pcm ON f.id = pcm.child_id
		LEFT JOIN link_counts lc ON f.id = lc.file_id
		WHERE pcm.parent_id = $1 AND pcm.child_name = $2
	`

	row := s.pool.QueryRow(ctx.Context, query, parentID, name)
	child, err := fileRowToFileWithNlink(row)
	if err != nil {
		return nil, mapPgError(err, "Lookup", fmt.Sprintf("%s/%s", parent.Path, name))
	}

	return child, nil
}

// GetShareNameForHandle returns the share name for a given file handle
func (s *PostgresMetadataStore) GetShareNameForHandle(ctx context.Context, handle metadata.FileHandle) (string, error) {
	shareName, _, err := decodeFileHandle(handle)
	if err != nil {
		return "", &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid file handle",
		}
	}

	return shareName, nil
}

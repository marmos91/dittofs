package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/store/metadata"
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

	// Query the file from the database
	query := `
		SELECT
			id, share_name, path,
			file_type, mode, uid, gid, size,
			atime, mtime, ctime,
			content_id, link_target, device_major, device_minor
		FROM files
		WHERE id = $1 AND share_name = $2
	`

	row := s.pool.QueryRow(ctx, query, id, shareName)
	file, err := fileRowToFile(row)
	if err != nil {
		return nil, mapPgError(err, "GetFile", "")
	}

	return file, nil
}

// GetFileByID retrieves a file by its UUID (internal use)
func (s *PostgresMetadataStore) getFileByID(ctx context.Context, id uuid.UUID, shareName string) (*metadata.File, error) {
	query := `
		SELECT
			id, share_name, path,
			file_type, mode, uid, gid, size,
			atime, mtime, ctime,
			content_id, link_target, device_major, device_minor
		FROM files
		WHERE id = $1 AND share_name = $2
	`

	row := s.pool.QueryRow(ctx, query, id, shareName)
	file, err := fileRowToFile(row)
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
			id, share_name, path,
			file_type, mode, uid, gid, size,
			atime, mtime, ctime,
			content_id, link_target, device_major, device_minor
		FROM files
		WHERE content_id = $1
		LIMIT 1
	`

	row := s.pool.QueryRow(ctx, query, string(contentID))
	file, err := fileRowToFile(row)
	if err != nil {
		return nil, mapPgError(err, "GetFileByContentID", string(contentID))
	}

	return file, nil
}

// Lookup finds a file by name in a parent directory
func (s *PostgresMetadataStore) Lookup(ctx context.Context, authCtx *metadata.AuthContext, parentHandle metadata.FileHandle, name string) (metadata.FileHandle, *metadata.FileAttr, error) {
	// Validate input
	if name == "" {
		return nil, nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "file name cannot be empty",
		}
	}

	if name == "." || name == ".." {
		return nil, nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "lookup of '.' and '..' not supported",
		}
	}

	// Decode parent handle
	shareName, parentID, err := decodeFileHandle(parentHandle)
	if err != nil {
		return nil, nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid parent handle",
		}
	}

	// Check if parent is a directory (with execute permission)
	parent, err := s.getFileByID(ctx, parentID, shareName)
	if err != nil {
		return nil, nil, err
	}

	if parent.Type != metadata.FileTypeDirectory {
		return nil, nil, &metadata.StoreError{
			Code:    metadata.ErrNotDirectory,
			Message: "parent is not a directory",
			Path:    parent.Path,
		}
	}

	// Check execute permission on parent directory
	if err := s.checkAccess(parent, authCtx, metadata.PermissionExecute); err != nil {
		return nil, nil, err
	}

	// Query child file
	query := `
		SELECT
			f.id, f.share_name, f.path,
			f.file_type, f.mode, f.uid, f.gid, f.size,
			f.atime, f.mtime, f.ctime,
			f.content_id, f.link_target, f.device_major, f.device_minor
		FROM files f
		INNER JOIN parent_child_map pcm ON f.id = pcm.child_id
		WHERE pcm.parent_id = $1 AND pcm.child_name = $2
	`

	row := s.pool.QueryRow(ctx, query, parentID, name)
	child, err := fileRowToFile(row)
	if err != nil {
		return nil, nil, mapPgError(err, "Lookup", fmt.Sprintf("%s/%s", parent.Path, name))
	}

	// Encode child handle
	childHandle, err := metadata.EncodeFileHandle(child)
	if err != nil {
		return nil, nil, &metadata.StoreError{
			Code:    metadata.ErrIOError,
			Message: "failed to encode child handle",
		}
	}

	return childHandle, &child.FileAttr, nil
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

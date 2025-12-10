package postgres

import (
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// RemoveFile removes a file
func (s *PostgresMetadataStore) RemoveFile(
	ctx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	name string,
) (*metadata.File, error) {
	if name == "" {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "file name cannot be empty",
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

	// Begin transaction
	tx, err := s.pool.Begin(ctx.Context)
	if err != nil {
		return nil, mapPgError(err, "RemoveFile", name)
	}
	defer tx.Rollback(ctx.Context)

	// Get and lock parent directory
	parent, err := s.getFileByIDTx(ctx.Context, tx, parentID, shareName)
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

	// Check write permission on parent
	if err := s.checkAccess(parent, ctx, metadata.PermissionWrite); err != nil {
		return nil, err
	}

	// Get child file
	lookupQuery := `
		SELECT child_id
		FROM parent_child_map
		WHERE parent_id = $1 AND child_name = $2
	`

	var childID uuid.UUID
	err = tx.QueryRow(ctx.Context, lookupQuery, parentID, name).Scan(&childID)
	if err != nil {
		return nil, mapPgError(err, "RemoveFile", name)
	}

	// Get child file details
	child, err := s.getFileByIDTx(ctx.Context, tx, childID, shareName)
	if err != nil {
		return nil, err
	}

	// Verify it's not a directory
	if child.Type == metadata.FileTypeDirectory {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrIsDirectory,
			Message: "cannot remove directory (use RemoveDirectory)",
			Path:    child.Path,
		}
	}

	// Delete parent_child_map entry
	deleteMapQuery := `
		DELETE FROM parent_child_map
		WHERE parent_id = $1 AND child_id = $2
	`

	_, err = tx.Exec(ctx.Context, deleteMapQuery, parentID, childID)
	if err != nil {
		return nil, mapPgError(err, "RemoveFile", child.Path)
	}

	// Decrement link count
	updateLinkCountQuery := `
		UPDATE link_counts
		SET link_count = link_count - 1
		WHERE file_id = $1
		RETURNING link_count
	`

	var linkCount int32
	err = tx.QueryRow(ctx.Context, updateLinkCountQuery, childID).Scan(&linkCount)
	if err != nil {
		return nil, mapPgError(err, "RemoveFile", child.Path)
	}

	// If link count reaches 0, delete the file
	if linkCount == 0 {
		// Delete from files table (CASCADE will delete link_counts entry)
		deleteFileQuery := `DELETE FROM files WHERE id = $1`
		_, err = tx.Exec(ctx.Context, deleteFileQuery, childID)
		if err != nil {
			return nil, mapPgError(err, "RemoveFile", child.Path)
		}
	}

	// Update parent directory mtime
	now := time.Now()
	updateParentQuery := `
		UPDATE files
		SET mtime = $1, ctime = $1
		WHERE id = $2
	`

	_, err = tx.Exec(ctx.Context, updateParentQuery, now, parentID)
	if err != nil {
		return nil, mapPgError(err, "RemoveFile", child.Path)
	}

	// Commit transaction
	if err := tx.Commit(ctx.Context); err != nil {
		return nil, mapPgError(err, "RemoveFile", child.Path)
	}

	// Invalidate stats cache
	s.statsCache.invalidate()

	return child, nil
}

// RemoveDirectory removes an empty directory
func (s *PostgresMetadataStore) RemoveDirectory(
	ctx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	name string,
) error {
	if name == "" {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "directory name cannot be empty",
		}
	}

	// Decode parent handle
	shareName, parentID, err := decodeFileHandle(parentHandle)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid parent handle",
		}
	}

	// Begin transaction
	tx, err := s.pool.Begin(ctx.Context)
	if err != nil {
		return mapPgError(err, "RemoveDirectory", name)
	}
	defer tx.Rollback(ctx.Context)

	// Get and lock parent directory
	parent, err := s.getFileByIDTx(ctx.Context, tx, parentID, shareName)
	if err != nil {
		return err
	}

	if parent.Type != metadata.FileTypeDirectory {
		return &metadata.StoreError{
			Code:    metadata.ErrNotDirectory,
			Message: "parent is not a directory",
			Path:    parent.Path,
		}
	}

	// Check write permission on parent
	if err := s.checkAccess(parent, ctx, metadata.PermissionWrite); err != nil {
		return err
	}

	// Get child directory
	lookupQuery := `
		SELECT child_id
		FROM parent_child_map
		WHERE parent_id = $1 AND child_name = $2
	`

	var childID uuid.UUID
	err = tx.QueryRow(ctx.Context, lookupQuery, parentID, name).Scan(&childID)
	if err != nil {
		return mapPgError(err, "RemoveDirectory", name)
	}

	// Get child directory details
	child, err := s.getFileByIDTx(ctx.Context, tx, childID, shareName)
	if err != nil {
		return err
	}

	// Verify it's a directory
	if child.Type != metadata.FileTypeDirectory {
		return &metadata.StoreError{
			Code:    metadata.ErrNotDirectory,
			Message: "not a directory",
			Path:    child.Path,
		}
	}

	// Check if directory is empty
	checkEmptyQuery := `
		SELECT EXISTS(
			SELECT 1 FROM parent_child_map
			WHERE parent_id = $1
			LIMIT 1
		)
	`

	var hasChildren bool
	err = tx.QueryRow(ctx.Context, checkEmptyQuery, childID).Scan(&hasChildren)
	if err != nil {
		return mapPgError(err, "RemoveDirectory", child.Path)
	}

	if hasChildren {
		return &metadata.StoreError{
			Code:    metadata.ErrNotEmpty,
			Message: "directory not empty",
			Path:    child.Path,
		}
	}

	// Delete parent_child_map entry
	deleteMapQuery := `
		DELETE FROM parent_child_map
		WHERE parent_id = $1 AND child_id = $2
	`

	_, err = tx.Exec(ctx.Context, deleteMapQuery, parentID, childID)
	if err != nil {
		return mapPgError(err, "RemoveDirectory", child.Path)
	}

	// Delete from files table (CASCADE will delete link_counts entry)
	deleteFileQuery := `DELETE FROM files WHERE id = $1`
	_, err = tx.Exec(ctx.Context, deleteFileQuery, childID)
	if err != nil {
		return mapPgError(err, "RemoveDirectory", child.Path)
	}

	// Update parent directory mtime
	now := time.Now()
	updateParentQuery := `
		UPDATE files
		SET mtime = $1, ctime = $1
		WHERE id = $2
	`

	_, err = tx.Exec(ctx.Context, updateParentQuery, now, parentID)
	if err != nil {
		return mapPgError(err, "RemoveDirectory", child.Path)
	}

	// Commit transaction
	if err := tx.Commit(ctx.Context); err != nil {
		return mapPgError(err, "RemoveDirectory", child.Path)
	}

	// Invalidate stats cache
	s.statsCache.invalidate()

	return nil
}

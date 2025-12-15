package postgres

import (
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// RemoveFile removes a file's metadata from its parent directory.
//
// This performs metadata cleanup including permission validation, type checking,
// and directory entry removal. The file's content data is NOT deleted by this
// method - the caller must coordinate content deletion with the content repository
// using the returned ContentID.
//
// Hard Links:
// If the file has multiple hard links (linkCount > 1), this removes only one link.
// The returned File will have an empty ContentID to signal that the caller should
// NOT delete the content (other hard links still reference it).
// When the last link is removed (linkCount reaches 0), the ContentID is returned
// so the caller can delete the content.
func (s *PostgresMetadataStore) RemoveFile(
	ctx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	name string,
) (*metadata.File, error) {
	if err := metadata.ValidateName(name); err != nil {
		return nil, err
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
	defer func() { _ = tx.Rollback(ctx.Context) }()

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
	// NOTE: We delete by parent_id AND child_name, NOT child_id
	// This is critical for hard link support - multiple entries can have the same child_id
	// but different child_names (hard links). We only want to remove the specific name.
	deleteMapQuery := `
		DELETE FROM parent_child_map
		WHERE parent_id = $1 AND child_name = $2
	`

	_, err = tx.Exec(ctx.Context, deleteMapQuery, parentID, name)
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

	// If link count > 0, other hard links still reference this content.
	// Clear ContentID to signal to the caller that content should NOT be deleted.
	// This matches the memory store behavior - empty ContentID means "don't delete content".
	if linkCount > 0 {
		child.ContentID = ""
	}

	return child, nil
}

// RemoveDirectory removes an empty directory
func (s *PostgresMetadataStore) RemoveDirectory(
	ctx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	name string,
) error {
	if err := metadata.ValidateName(name); err != nil {
		return err
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
	defer func() { _ = tx.Rollback(ctx.Context) }()

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

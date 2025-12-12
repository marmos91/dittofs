package postgres

import (
	"path"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// Move moves or renames a file or directory
func (s *PostgresMetadataStore) Move(
	ctx *metadata.AuthContext,
	srcParentHandle metadata.FileHandle,
	srcName string,
	dstParentHandle metadata.FileHandle,
	dstName string,
) error {
	if srcName == "" || dstName == "" {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "source and destination names cannot be empty",
		}
	}

	// Decode handles
	srcShareName, srcParentID, err := decodeFileHandle(srcParentHandle)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid source parent handle",
		}
	}

	dstShareName, dstParentID, err := decodeFileHandle(dstParentHandle)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid destination parent handle",
		}
	}

	// Cannot move across shares
	if srcShareName != dstShareName {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "cannot move across shares",
		}
	}

	// Begin transaction
	tx, err := s.pool.Begin(ctx.Context)
	if err != nil {
		return mapPgError(err, "Move", srcName)
	}
	defer tx.Rollback(ctx.Context)

	// Lock both parents in deterministic order (by UUID) to prevent deadlocks
	parentIDs := []uuid.UUID{srcParentID, dstParentID}
	if srcParentID.String() > dstParentID.String() {
		parentIDs = []uuid.UUID{dstParentID, srcParentID}
	}

	// Lock parents
	lockQuery := `
		SELECT id FROM files
		WHERE id = ANY($1::uuid[])
		ORDER BY id
		FOR UPDATE
	`

	_, err = tx.Exec(ctx.Context, lockQuery, parentIDs)
	if err != nil {
		return mapPgError(err, "Move", srcName)
	}

	// Get source parent
	srcParent, err := s.getFileByIDTx(ctx.Context, tx, srcParentID, srcShareName)
	if err != nil {
		return err
	}

	if srcParent.Type != metadata.FileTypeDirectory {
		return &metadata.StoreError{
			Code:    metadata.ErrNotDirectory,
			Message: "source parent is not a directory",
			Path:    srcParent.Path,
		}
	}

	// Check write permission on source parent
	if err := s.checkAccess(srcParent, ctx, metadata.PermissionWrite); err != nil {
		return err
	}

	// Get destination parent
	dstParent, err := s.getFileByIDTx(ctx.Context, tx, dstParentID, dstShareName)
	if err != nil {
		return err
	}

	if dstParent.Type != metadata.FileTypeDirectory {
		return &metadata.StoreError{
			Code:    metadata.ErrNotDirectory,
			Message: "destination parent is not a directory",
			Path:    dstParent.Path,
		}
	}

	// Check write permission on destination parent (if different from source)
	if srcParentID != dstParentID {
		if err := s.checkAccess(dstParent, ctx, metadata.PermissionWrite); err != nil {
			return err
		}
	}

	// Get source file/directory ID
	srcLookupQuery := `
		SELECT child_id
		FROM parent_child_map
		WHERE parent_id = $1 AND child_name = $2
	`

	var srcFileID uuid.UUID
	err = tx.QueryRow(ctx.Context, srcLookupQuery, srcParentID, srcName).Scan(&srcFileID)
	if err != nil {
		return mapPgError(err, "Move", srcName)
	}

	// Check if destination already exists
	dstCheckQuery := `
		SELECT child_id FROM parent_child_map
		WHERE parent_id = $1 AND child_name = $2
	`

	var dstFileID uuid.UUID
	err = tx.QueryRow(ctx.Context, dstCheckQuery, dstParentID, dstName).Scan(&dstFileID)
	dstExists := err == nil
	if err != nil && err.Error() != "no rows in result set" {
		return mapPgError(err, "Move", dstName)
	}

	now := time.Now()

	// If destination exists, handle replacement
	if dstExists {
		// Get destination file to check type compatibility
		dstFile, err := s.getFileByIDTx(ctx.Context, tx, dstFileID, dstShareName)
		if err != nil {
			return err
		}

		// Get source file for type checking
		srcFile, err := s.getFileByIDTx(ctx.Context, tx, srcFileID, srcShareName)
		if err != nil {
			return err
		}

		// Check type compatibility for replacement
		if srcFile.Type == metadata.FileTypeDirectory {
			// Moving directory - destination must be a directory too
			if dstFile.Type != metadata.FileTypeDirectory {
				return &metadata.StoreError{
					Code:    metadata.ErrNotDirectory,
					Message: "cannot replace non-directory with directory",
					Path:    path.Join(dstParent.Path, dstName),
				}
			}
			// Check if destination directory is empty
			emptyCheckQuery := `
				SELECT EXISTS(
					SELECT 1 FROM parent_child_map
					WHERE parent_id = $1
				)
			`
			var hasChildren bool
			err = tx.QueryRow(ctx.Context, emptyCheckQuery, dstFileID).Scan(&hasChildren)
			if err != nil {
				return mapPgError(err, "Move", dstName)
			}
			if hasChildren {
				return &metadata.StoreError{
					Code:    metadata.ErrNotEmpty,
					Message: "destination directory not empty",
					Path:    path.Join(dstParent.Path, dstName),
				}
			}
		} else {
			// Moving non-directory - destination must not be a directory
			if dstFile.Type == metadata.FileTypeDirectory {
				return &metadata.StoreError{
					Code:    metadata.ErrIsDirectory,
					Message: "cannot replace directory with non-directory",
					Path:    path.Join(dstParent.Path, dstName),
				}
			}
		}

		// Get link count for destination file
		var dstLinkCount int32
		linkCountQuery := `SELECT link_count FROM link_counts WHERE file_id = $1`
		err = tx.QueryRow(ctx.Context, linkCountQuery, dstFileID).Scan(&dstLinkCount)
		if err != nil {
			return mapPgError(err, "Move", dstName)
		}

		// Remove destination from parent_child_map
		deleteDstMapQuery := `
			DELETE FROM parent_child_map
			WHERE parent_id = $1 AND child_name = $2
		`
		_, err = tx.Exec(ctx.Context, deleteDstMapQuery, dstParentID, dstName)
		if err != nil {
			return mapPgError(err, "Move", dstName)
		}

		// Handle link count and potential file deletion
		if dstLinkCount > 1 {
			// Has other hard links, just decrement count
			decrementLinkQuery := `
				UPDATE link_counts
				SET link_count = link_count - 1
				WHERE file_id = $1
			`
			_, err = tx.Exec(ctx.Context, decrementLinkQuery, dstFileID)
			if err != nil {
				return mapPgError(err, "Move", dstName)
			}
		} else {
			// Last link - delete the file entirely
			// Delete from link_counts first (foreign key)
			deleteLinkCountQuery := `DELETE FROM link_counts WHERE file_id = $1`
			_, err = tx.Exec(ctx.Context, deleteLinkCountQuery, dstFileID)
			if err != nil {
				return mapPgError(err, "Move", dstName)
			}

			// Delete the file itself
			deleteFileQuery := `DELETE FROM files WHERE id = $1`
			_, err = tx.Exec(ctx.Context, deleteFileQuery, dstFileID)
			if err != nil {
				return mapPgError(err, "Move", dstName)
			}
		}
	}

	// Get source file to update its path
	srcFile, err := s.getFileByIDTx(ctx.Context, tx, srcFileID, srcShareName)
	if err != nil {
		return err
	}

	// Calculate new path
	newPath := path.Join(dstParent.Path, dstName)

	// If it's the same parent, just update the name
	if srcParentID == dstParentID {
		// Update parent_child_map (just change child_name)
		updateMapQuery := `
			UPDATE parent_child_map
			SET child_name = $1
			WHERE parent_id = $2 AND child_id = $3
		`

		_, err = tx.Exec(ctx.Context, updateMapQuery, dstName, srcParentID, srcFileID)
		if err != nil {
			return mapPgError(err, "Move", srcName)
		}

		// Update file path
		updatePathQuery := `
			UPDATE files
			SET path = $1, ctime = $2
			WHERE id = $3
		`

		_, err = tx.Exec(ctx.Context, updatePathQuery, newPath, now, srcFileID)
		if err != nil {
			return mapPgError(err, "Move", srcName)
		}

		// Update parent mtime
		updateParentQuery := `
			UPDATE files
			SET mtime = $1, ctime = $1
			WHERE id = $2
		`

		_, err = tx.Exec(ctx.Context, updateParentQuery, now, srcParentID)
		if err != nil {
			return mapPgError(err, "Move", srcName)
		}
	} else {
		// Cross-directory move: delete from source, insert into destination
		deleteMapQuery := `
			DELETE FROM parent_child_map
			WHERE parent_id = $1 AND child_id = $2
		`

		_, err = tx.Exec(ctx.Context, deleteMapQuery, srcParentID, srcFileID)
		if err != nil {
			return mapPgError(err, "Move", srcName)
		}

		insertMapQuery := `
			INSERT INTO parent_child_map (parent_id, child_id, child_name)
			VALUES ($1, $2, $3)
		`

		_, err = tx.Exec(ctx.Context, insertMapQuery, dstParentID, srcFileID, dstName)
		if err != nil {
			return mapPgError(err, "Move", dstName)
		}

		// Update file path
		updatePathQuery := `
			UPDATE files
			SET path = $1, ctime = $2
			WHERE id = $3
		`

		_, err = tx.Exec(ctx.Context, updatePathQuery, newPath, now, srcFileID)
		if err != nil {
			return mapPgError(err, "Move", srcName)
		}

		// Update both parent mtimes
		updateParentsQuery := `
			UPDATE files
			SET mtime = $1, ctime = $1
			WHERE id = ANY($2::uuid[])
		`

		_, err = tx.Exec(ctx.Context, updateParentsQuery, now, []uuid.UUID{srcParentID, dstParentID})
		if err != nil {
			return mapPgError(err, "Move", srcName)
		}
	}

	// If moving a directory, update all descendant paths
	if srcFile.Type == metadata.FileTypeDirectory {
		// This is a complex operation - we need to update all descendant paths
		// For now, we'll use a recursive approach
		// In production, you might want to use a more efficient method

		oldPathPrefix := srcFile.Path + "/"
		newPathPrefix := newPath + "/"

		updateDescendantsQuery := `
			UPDATE files
			SET path = $1 || SUBSTRING(path FROM $2::INTEGER),
			    ctime = $3
			WHERE path LIKE $4 AND share_name = $5
		`

		_, err = tx.Exec(ctx.Context, updateDescendantsQuery,
			newPathPrefix,
			len(oldPathPrefix)+1,
			now,
			oldPathPrefix+"%",
			srcShareName,
		)
		if err != nil {
			return mapPgError(err, "Move", srcName)
		}
	}

	// Commit transaction
	if err := tx.Commit(ctx.Context); err != nil {
		return mapPgError(err, "Move", srcName)
	}

	return nil
}

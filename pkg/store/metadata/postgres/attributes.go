package postgres

import (
	"context"
	"fmt"
	"path"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// SetFileAttributes updates file attributes
func (s *PostgresMetadataStore) SetFileAttributes(
	ctx *metadata.AuthContext,
	handle metadata.FileHandle,
	attrs *metadata.SetAttrs,
) error {
	// Decode handle
	shareName, fileID, err := decodeFileHandle(handle)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid file handle",
		}
	}

	// Begin transaction
	tx, err := s.pool.Begin(context.Background())
	if err != nil {
		return mapPgError(err, "SetFileAttributes", "")
	}
	defer tx.Rollback(context.Background())

	// Get and lock file
	file, err := s.getFileByIDTx(context.Background(), tx, fileID, shareName)
	if err != nil {
		return err
	}

	// Check permissions for different updates
	if attrs.Mode != nil || attrs.UID != nil || attrs.GID != nil {
		// Changing ownership or permissions requires ownership
		if err := s.checkAccess(file, ctx, metadata.PermissionChangeOwnership); err != nil {
			return err
		}
	}

	// Build update query dynamically
	query := "UPDATE files SET "
	params := []interface{}{}
	paramIndex := 1
	updates := []string{}

	now := time.Now()

	// Mode
	if attrs.Mode != nil {
		updates = append(updates, fmt.Sprintf("mode = $%d", paramIndex))
		params = append(params, int32(*attrs.Mode&0o7777))
		paramIndex++
	}

	// UID
	if attrs.UID != nil {
		updates = append(updates, fmt.Sprintf("uid = $%d", paramIndex))
		params = append(params, int32(*attrs.UID))
		paramIndex++
	}

	// GID
	if attrs.GID != nil {
		updates = append(updates, fmt.Sprintf("gid = $%d", paramIndex))
		params = append(params, int32(*attrs.GID))
		paramIndex++
	}

	// Size (truncate)
	if attrs.Size != nil {
		updates = append(updates, fmt.Sprintf("size = $%d", paramIndex))
		params = append(params, int64(*attrs.Size))
		paramIndex++
	}

	// Atime
	if attrs.Atime != nil {
		updates = append(updates, fmt.Sprintf("atime = $%d", paramIndex))
		params = append(params, *attrs.Atime)
		paramIndex++
	}

	// Mtime
	if attrs.Mtime != nil {
		updates = append(updates, fmt.Sprintf("mtime = $%d", paramIndex))
		params = append(params, *attrs.Mtime)
		paramIndex++
	}

	// Always update ctime when attributes change
	updates = append(updates, fmt.Sprintf("ctime = $%d", paramIndex))
	params = append(params, now)
	paramIndex++

	// Add WHERE clause
	query += joinStrings(updates, ", ")
	query += fmt.Sprintf(" WHERE id = $%d AND share_name = $%d", paramIndex, paramIndex+1)
	params = append(params, fileID, shareName)

	// Execute update
	_, err = tx.Exec(context.Background(), query, params...)
	if err != nil {
		return mapPgError(err, "SetFileAttributes", file.Path)
	}

	// Commit transaction
	if err := tx.Commit(context.Background()); err != nil {
		return mapPgError(err, "SetFileAttributes", file.Path)
	}

	return nil
}

// CreateSymlink creates a symbolic link
func (s *PostgresMetadataStore) CreateSymlink(
	ctx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	name string,
	target string,
	attr *metadata.FileAttr,
) (*metadata.File, error) {
	// Validate input
	if name == "" {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "symlink name cannot be empty",
		}
	}

	if target == "" {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "symlink target cannot be empty",
		}
	}

	// Apply defaults
	mode := attr.Mode
	if mode == 0 {
		mode = 0o777 // Symlinks typically have 0777 permissions
	}
	uid := attr.UID
	gid := attr.GID

	// Decode parent handle
	shareName, parentID, err := decodeFileHandle(parentHandle)
	if err != nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid parent handle",
		}
	}

	// Begin transaction
	tx, err := s.pool.Begin(context.Background())
	if err != nil {
		return nil, mapPgError(err, "CreateSymlink", name)
	}
	defer tx.Rollback(context.Background())

	// Get and lock parent directory
	parent, err := s.getFileByIDTx(context.Background(), tx, parentID, shareName)
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

	// Check if child already exists
	checkQuery := `
		SELECT EXISTS(
			SELECT 1 FROM parent_child_map
			WHERE parent_id = $1 AND child_name = $2
		)
	`
	var exists bool
	err = tx.QueryRow(context.Background(), checkQuery, parentID, name).Scan(&exists)
	if err != nil {
		return nil, mapPgError(err, "CreateSymlink", name)
	}

	if exists {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrAlreadyExists,
			Message: "symlink already exists",
			Path:    path.Join(parent.Path, name),
		}
	}

	// Generate new symlink ID
	symlinkID := uuid.New()
	symlinkPath := path.Join(parent.Path, name)
	now := time.Now()

	// Insert symlink
	insertQuery := `
		INSERT INTO files (
			id, share_name, path,
			file_type, mode, uid, gid, size,
			atime, mtime, ctime,
			content_id, link_target, device_major, device_minor
		) VALUES (
			$1, $2, $3,
			$4, $5, $6, $7, $8,
			$9, $10, $11,
			$12, $13, $14, $15
		)
	`

	_, err = tx.Exec(context.Background(), insertQuery,
		symlinkID,
		shareName,
		symlinkPath,
		int16(metadata.FileTypeSymlink),
		int32(mode & 0o7777),
		int32(uid),
		int32(gid),
		int64(len(target)), // size is length of target path
		now,                // atime
		now,                // mtime
		now,                // ctime
		nil,                // content_id (NULL for symlinks)
		target,             // link_target
		nil,                // device_major
		nil,                // device_minor
	)
	if err != nil {
		return nil, mapPgError(err, "CreateSymlink", symlinkPath)
	}

	// Insert into parent_child_map
	insertMapQuery := `
		INSERT INTO parent_child_map (parent_id, child_id, child_name)
		VALUES ($1, $2, $3)
	`

	_, err = tx.Exec(context.Background(), insertMapQuery, parentID, symlinkID, name)
	if err != nil {
		return nil, mapPgError(err, "CreateSymlink", symlinkPath)
	}

	// Insert into link_counts (symlinks have link count = 1)
	insertLinkCountQuery := `
		INSERT INTO link_counts (file_id, link_count)
		VALUES ($1, $2)
	`

	_, err = tx.Exec(context.Background(), insertLinkCountQuery, symlinkID, 1)
	if err != nil {
		return nil, mapPgError(err, "CreateSymlink", symlinkPath)
	}

	// Update parent directory mtime
	updateParentQuery := `
		UPDATE files
		SET mtime = $1, ctime = $1
		WHERE id = $2
	`

	_, err = tx.Exec(context.Background(), updateParentQuery, now, parentID)
	if err != nil {
		return nil, mapPgError(err, "CreateSymlink", symlinkPath)
	}

	// Commit transaction
	if err := tx.Commit(context.Background()); err != nil {
		return nil, mapPgError(err, "CreateSymlink", symlinkPath)
	}

	// Invalidate stats cache
	s.statsCache.invalidate()

	// Build File
	file := &metadata.File{
		ID:        symlinkID,
		ShareName: shareName,
		Path:      symlinkPath,
		FileAttr: metadata.FileAttr{
			Type:       metadata.FileTypeSymlink,
			Mode:       mode & 0o7777,
			UID:        uid,
			GID:        gid,
			Size:       uint64(len(target)),
			Atime:      now,
			Mtime:      now,
			Ctime:      now,
			LinkTarget: target,
		},
	}

	return file, nil
}

// ReadSymlink reads the target of a symbolic link
func (s *PostgresMetadataStore) ReadSymlink(ctx *metadata.AuthContext, handle metadata.FileHandle) (string, *metadata.File, error) {
	// Get file
	file, err := s.GetFile(context.Background(), handle)
	if err != nil {
		return "", nil, err
	}

	// Verify it's a symlink
	if file.Type != metadata.FileTypeSymlink {
		return "", nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "not a symbolic link",
			Path:    file.Path,
		}
	}

	return file.LinkTarget, file, nil
}

// CreateSpecialFile creates a special file (device, FIFO, socket)
func (s *PostgresMetadataStore) CreateSpecialFile(
	ctx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	name string,
	fileType metadata.FileType,
	attr *metadata.FileAttr,
	deviceMajor, deviceMinor uint32,
) (*metadata.File, error) {
	// Validate file type
	if fileType != metadata.FileTypeCharDevice &&
		fileType != metadata.FileTypeBlockDevice &&
		fileType != metadata.FileTypeFIFO &&
		fileType != metadata.FileTypeSocket {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "invalid special file type",
		}
	}

	// TODO: For now, return not supported
	// Full implementation would be similar to CreateFile but with device_major/device_minor
	return nil, &metadata.StoreError{
		Code:    metadata.ErrNotSupported,
		Message: "special files not yet supported",
	}
}

// CreateHardLink creates a hard link to an existing file
func (s *PostgresMetadataStore) CreateHardLink(
	ctx *metadata.AuthContext,
	dirHandle metadata.FileHandle,
	name string,
	targetHandle metadata.FileHandle,
) error {
	// Hard links not fully supported yet (return not supported like BadgerDB)
	return &metadata.StoreError{
		Code:    metadata.ErrNotSupported,
		Message: "hard links not yet supported",
	}
}

// Helper function to join strings
func joinStrings(strs []string, sep string) string {
	if len(strs) == 0 {
		return ""
	}
	result := strs[0]
	for i := 1; i < len(strs); i++ {
		result += sep + strs[i]
	}
	return result
}

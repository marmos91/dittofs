package postgres

import (
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
	tx, err := s.pool.Begin(ctx.Context)
	if err != nil {
		return mapPgError(err, "SetFileAttributes", "")
	}
	defer func() { _ = tx.Rollback(ctx.Context) }()

	// Get and lock file
	file, err := s.getFileByIDTx(ctx.Context, tx, fileID, shareName)
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
	_, err = tx.Exec(ctx.Context, query, params...)
	if err != nil {
		return mapPgError(err, "SetFileAttributes", file.Path)
	}

	// Commit transaction
	if err := tx.Commit(ctx.Context); err != nil {
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
	if err := metadata.ValidateName(name); err != nil {
		return nil, err
	}

	if err := metadata.ValidateSymlinkTarget(target); err != nil {
		return nil, err
	}

	// Apply defaults
	attr.Type = metadata.FileTypeSymlink
	metadata.ApplyCreateDefaults(attr, ctx, target)

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
		return nil, mapPgError(err, "CreateSymlink", name)
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

	// Check if child already exists
	checkQuery := `
		SELECT EXISTS(
			SELECT 1 FROM parent_child_map
			WHERE parent_id = $1 AND child_name = $2
		)
	`
	var exists bool
	err = tx.QueryRow(ctx.Context, checkQuery, parentID, name).Scan(&exists)
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

	_, err = tx.Exec(ctx.Context, insertQuery,
		symlinkID,
		shareName,
		symlinkPath,
		int16(metadata.FileTypeSymlink),
		int32(attr.Mode),
		int32(attr.UID),
		int32(attr.GID),
		int64(attr.Size), // size is length of target path (set by ApplyCreateDefaults)
		attr.Atime,
		attr.Mtime,
		attr.Ctime,
		nil,    // content_id (NULL for symlinks)
		target, // link_target
		nil,    // device_major
		nil,    // device_minor
	)
	if err != nil {
		return nil, mapPgError(err, "CreateSymlink", symlinkPath)
	}

	// Insert into parent_child_map
	insertMapQuery := `
		INSERT INTO parent_child_map (parent_id, child_id, child_name)
		VALUES ($1, $2, $3)
	`

	_, err = tx.Exec(ctx.Context, insertMapQuery, parentID, symlinkID, name)
	if err != nil {
		return nil, mapPgError(err, "CreateSymlink", symlinkPath)
	}

	// Insert into link_counts (symlinks have link count = 1)
	insertLinkCountQuery := `
		INSERT INTO link_counts (file_id, link_count)
		VALUES ($1, $2)
	`

	_, err = tx.Exec(ctx.Context, insertLinkCountQuery, symlinkID, 1)
	if err != nil {
		return nil, mapPgError(err, "CreateSymlink", symlinkPath)
	}

	// Update parent directory mtime
	updateParentQuery := `
		UPDATE files
		SET mtime = $1, ctime = $1
		WHERE id = $2
	`

	_, err = tx.Exec(ctx.Context, updateParentQuery, attr.Mtime, parentID)
	if err != nil {
		return nil, mapPgError(err, "CreateSymlink", symlinkPath)
	}

	// Commit transaction
	if err := tx.Commit(ctx.Context); err != nil {
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
			Mode:       attr.Mode,
			UID:        attr.UID,
			GID:        attr.GID,
			Size:       attr.Size,
			Atime:      attr.Atime,
			Mtime:      attr.Mtime,
			Ctime:      attr.Ctime,
			LinkTarget: target,
		},
	}

	return file, nil
}

// ReadSymlink reads the target of a symbolic link
func (s *PostgresMetadataStore) ReadSymlink(ctx *metadata.AuthContext, handle metadata.FileHandle) (string, *metadata.File, error) {
	// Get file
	file, err := s.GetFile(ctx.Context, handle)
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
	// Check context cancellation
	if err := ctx.Context.Err(); err != nil {
		return nil, err
	}

	// Validate file type
	if err := metadata.ValidateSpecialFileType(fileType); err != nil {
		return nil, err
	}

	// Validate name
	if err := metadata.ValidateName(name); err != nil {
		return nil, err
	}

	// Check if user is root (required for device files)
	if fileType == metadata.FileTypeBlockDevice || fileType == metadata.FileTypeCharDevice {
		if err := metadata.RequiresRoot(ctx); err != nil {
			return nil, err
		}
	}

	// Apply defaults
	attr.Type = fileType
	metadata.ApplyCreateDefaults(attr, ctx, "")

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
		return nil, mapPgError(err, "CreateSpecialFile", name)
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

	// Check if child already exists
	checkQuery := `
		SELECT EXISTS(
			SELECT 1 FROM parent_child_map
			WHERE parent_id = $1 AND child_name = $2
		)
	`
	var exists bool
	err = tx.QueryRow(ctx.Context, checkQuery, parentID, name).Scan(&exists)
	if err != nil {
		return nil, mapPgError(err, "CreateSpecialFile", name)
	}

	if exists {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrAlreadyExists,
			Message: "file already exists",
			Path:    path.Join(parent.Path, name),
		}
	}

	// Generate new file ID
	fileID := uuid.New()
	filePath := path.Join(parent.Path, name)

	// Insert special file
	// Note: Special files have no content_id, but may have device_major/device_minor
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

	// Set device numbers only for device files
	var devMajor, devMinor *int32
	if fileType == metadata.FileTypeBlockDevice || fileType == metadata.FileTypeCharDevice {
		major := int32(deviceMajor)
		minor := int32(deviceMinor)
		devMajor = &major
		devMinor = &minor
	}

	_, err = tx.Exec(ctx.Context, insertQuery,
		fileID,
		shareName,
		filePath,
		int16(fileType),
		int32(attr.Mode),
		int32(attr.UID),
		int32(attr.GID),
		int64(attr.Size), // size = 0 for special files (set by ApplyCreateDefaults)
		attr.Atime,
		attr.Mtime,
		attr.Ctime,
		nil,      // content_id (NULL for special files)
		nil,      // link_target
		devMajor, // device_major (NULL for non-device files)
		devMinor, // device_minor (NULL for non-device files)
	)
	if err != nil {
		return nil, mapPgError(err, "CreateSpecialFile", filePath)
	}

	// Insert into parent_child_map
	insertMapQuery := `
		INSERT INTO parent_child_map (parent_id, child_id, child_name)
		VALUES ($1, $2, $3)
	`

	_, err = tx.Exec(ctx.Context, insertMapQuery, parentID, fileID, name)
	if err != nil {
		return nil, mapPgError(err, "CreateSpecialFile", filePath)
	}

	// Insert into link_counts (special files start with link count = 1)
	insertLinkCountQuery := `
		INSERT INTO link_counts (file_id, link_count)
		VALUES ($1, $2)
	`

	_, err = tx.Exec(ctx.Context, insertLinkCountQuery, fileID, 1)
	if err != nil {
		return nil, mapPgError(err, "CreateSpecialFile", filePath)
	}

	// Update parent directory mtime
	updateParentQuery := `
		UPDATE files
		SET mtime = $1, ctime = $1
		WHERE id = $2
	`

	_, err = tx.Exec(ctx.Context, updateParentQuery, attr.Mtime, parentID)
	if err != nil {
		return nil, mapPgError(err, "CreateSpecialFile", filePath)
	}

	// Commit transaction
	if err := tx.Commit(ctx.Context); err != nil {
		return nil, mapPgError(err, "CreateSpecialFile", filePath)
	}

	// Invalidate stats cache
	s.statsCache.invalidate()

	// Build File
	file := &metadata.File{
		ID:        fileID,
		ShareName: shareName,
		Path:      filePath,
		FileAttr: metadata.FileAttr{
			Type:  fileType,
			Mode:  attr.Mode,
			UID:   attr.UID,
			GID:   attr.GID,
			Size:  attr.Size,
			Atime: attr.Atime,
			Mtime: attr.Mtime,
			Ctime: attr.Ctime,
		},
	}

	return file, nil
}

// CreateHardLink creates a hard link to an existing file
func (s *PostgresMetadataStore) CreateHardLink(
	ctx *metadata.AuthContext,
	dirHandle metadata.FileHandle,
	name string,
	targetHandle metadata.FileHandle,
) error {
	// Validate name
	if err := metadata.ValidateName(name); err != nil {
		return err
	}

	// Decode directory handle
	dirShareName, dirID, err := decodeFileHandle(dirHandle)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid directory handle",
		}
	}

	// Decode target handle
	targetShareName, targetID, err := decodeFileHandle(targetHandle)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid target handle",
		}
	}

	// Verify both are in the same share (hard links cannot cross filesystems)
	if dirShareName != targetShareName {
		return &metadata.StoreError{
			Code:    metadata.ErrNotSupported,
			Message: "hard links cannot cross filesystems",
		}
	}

	// Begin transaction
	tx, err := s.pool.Begin(ctx.Context)
	if err != nil {
		return mapPgError(err, "CreateHardLink", name)
	}
	defer func() { _ = tx.Rollback(ctx.Context) }()

	// Get and verify directory
	dirFile, err := s.getFileByIDTx(ctx.Context, tx, dirID, dirShareName)
	if err != nil {
		return err
	}

	if dirFile.Type != metadata.FileTypeDirectory {
		return &metadata.StoreError{
			Code:    metadata.ErrNotDirectory,
			Message: "not a directory",
			Path:    dirFile.Path,
		}
	}

	// Check write permission on directory
	if err := s.checkAccess(dirFile, ctx, metadata.PermissionWrite); err != nil {
		return err
	}

	// Get and verify target file
	targetFile, err := s.getFileByIDTx(ctx.Context, tx, targetID, targetShareName)
	if err != nil {
		return err
	}

	// Cannot hard link directories
	if targetFile.Type == metadata.FileTypeDirectory {
		return &metadata.StoreError{
			Code:    metadata.ErrIsDirectory,
			Message: "cannot create hard link to directory",
			Path:    targetFile.Path,
		}
	}

	// Check if name already exists in directory
	checkQuery := `
		SELECT EXISTS(
			SELECT 1 FROM parent_child_map
			WHERE parent_id = $1 AND child_name = $2
		)
	`
	var exists bool
	err = tx.QueryRow(ctx.Context, checkQuery, dirID, name).Scan(&exists)
	if err != nil {
		return mapPgError(err, "CreateHardLink", name)
	}

	if exists {
		return &metadata.StoreError{
			Code:    metadata.ErrAlreadyExists,
			Message: fmt.Sprintf("name already exists: %s", name),
			Path:    path.Join(dirFile.Path, name),
		}
	}

	// Get current link count
	var currentLinks int32
	linkCountQuery := `SELECT link_count FROM link_counts WHERE file_id = $1`
	err = tx.QueryRow(ctx.Context, linkCountQuery, targetID).Scan(&currentLinks)
	if err != nil {
		return mapPgError(err, "CreateHardLink", name)
	}

	// Check link count limit
	if uint32(currentLinks) >= s.capabilities.MaxHardLinkCount {
		return &metadata.StoreError{
			Code:    metadata.ErrNotSupported,
			Message: "maximum hard link count reached",
		}
	}

	// Add entry in parent_child_map (pointing to the same target file)
	insertMapQuery := `
		INSERT INTO parent_child_map (parent_id, child_id, child_name)
		VALUES ($1, $2, $3)
	`
	_, err = tx.Exec(ctx.Context, insertMapQuery, dirID, targetID, name)
	if err != nil {
		return mapPgError(err, "CreateHardLink", name)
	}

	// Increment link count
	updateLinkCountQuery := `
		UPDATE link_counts
		SET link_count = link_count + 1
		WHERE file_id = $1
	`
	_, err = tx.Exec(ctx.Context, updateLinkCountQuery, targetID)
	if err != nil {
		return mapPgError(err, "CreateHardLink", name)
	}

	// Update timestamps
	now := time.Now()

	// Target file's ctime changed (metadata changed)
	updateTargetQuery := `UPDATE files SET ctime = $1 WHERE id = $2`
	_, err = tx.Exec(ctx.Context, updateTargetQuery, now, targetID)
	if err != nil {
		return mapPgError(err, "CreateHardLink", name)
	}

	// Directory's mtime and ctime changed (contents changed)
	updateDirQuery := `UPDATE files SET mtime = $1, ctime = $1 WHERE id = $2`
	_, err = tx.Exec(ctx.Context, updateDirQuery, now, dirID)
	if err != nil {
		return mapPgError(err, "CreateHardLink", name)
	}

	// Commit transaction
	if err := tx.Commit(ctx.Context); err != nil {
		return mapPgError(err, "CreateHardLink", name)
	}

	// Invalidate stats cache
	s.statsCache.invalidate()

	return nil
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

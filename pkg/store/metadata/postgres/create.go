package postgres

import (
	"context"
	"path"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/marmos91/dittofs/pkg/store/metadata"
	"github.com/marmos91/dittofs/pkg/store/metadata/internal"
)

// Create creates a new file or directory
func (s *PostgresMetadataStore) Create(
	ctx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	name string,
	attr *metadata.FileAttr,
) (*metadata.File, error) {
	// Validate type
	if err := metadata.ValidateCreateType(attr.Type); err != nil {
		return nil, err
	}

	// Dispatch based on type
	if attr.Type == metadata.FileTypeRegular {
		return s.createFile(ctx, parentHandle, name, attr)
	}
	return s.createDirectory(ctx, parentHandle, name, attr)
}

// createFile creates a new regular file (internal implementation)
func (s *PostgresMetadataStore) createFile(
	ctx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	name string,
	attr *metadata.FileAttr,
) (*metadata.File, error) {
	// Validate input
	if err := metadata.ValidateName(name); err != nil {
		return nil, err
	}

	// Apply defaults for mode, UID/GID, timestamps, and size
	attr.Type = metadata.FileTypeRegular
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
		return nil, mapPgError(err, "createFile", name)
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
		return nil, mapPgError(err, "createFile", name)
	}

	if exists {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrAlreadyExists,
			Message: "file already exists",
			Path:    path.Join(parent.Path, name),
		}
	}

	// Generate new file ID and ContentID (path-based for S3 interoperability)
	fileID := uuid.New()
	filePath := path.Join(parent.Path, name)

	// Note: We don't validate PATH_MAX here because NFS uses file handles,
	// not paths. PATH_MAX only applies to paths passed to syscalls, but NFS
	// operations traverse component by component via LOOKUP.

	contentID := internal.BuildContentID(shareName, filePath)

	// Insert file
	insertQuery := `
		INSERT INTO files (
			id, share_name, path,
			file_type, mode, uid, gid, size,
			atime, mtime, ctime, creation_time,
			content_id, link_target, device_major, device_minor
		) VALUES (
			$1, $2, $3,
			$4, $5, $6, $7, $8,
			$9, $10, $11, $12,
			$13, $14, $15, $16
		)
	`

	_, err = tx.Exec(ctx.Context, insertQuery,
		fileID,
		shareName,
		filePath,
		int16(metadata.FileTypeRegular),
		int32(attr.Mode),
		int32(attr.UID),
		int32(attr.GID),
		int64(attr.Size),
		attr.Atime,
		attr.Mtime,
		attr.Ctime,
		attr.CreationTime,
		contentID,
		nil, // link_target (NULL for regular files)
		nil, // device_major
		nil, // device_minor
	)
	if err != nil {
		return nil, mapPgError(err, "createFile", filePath)
	}

	// Insert into parent_child_map
	insertMapQuery := `
		INSERT INTO parent_child_map (parent_id, child_id, child_name)
		VALUES ($1, $2, $3)
	`

	_, err = tx.Exec(ctx.Context, insertMapQuery, parentID, fileID, name)
	if err != nil {
		return nil, mapPgError(err, "createFile", filePath)
	}

	// Insert into link_counts (regular files start with link count = 1)
	insertLinkCountQuery := `
		INSERT INTO link_counts (file_id, link_count)
		VALUES ($1, $2)
	`

	_, err = tx.Exec(ctx.Context, insertLinkCountQuery, fileID, 1)
	if err != nil {
		return nil, mapPgError(err, "createFile", filePath)
	}

	// Update parent directory mtime
	updateParentQuery := `
		UPDATE files
		SET mtime = $1, ctime = $1
		WHERE id = $2
	`

	_, err = tx.Exec(ctx.Context, updateParentQuery, attr.Mtime, parentID)
	if err != nil {
		return nil, mapPgError(err, "createFile", filePath)
	}

	// Commit transaction
	if err := tx.Commit(ctx.Context); err != nil {
		return nil, mapPgError(err, "createFile", filePath)
	}

	// Invalidate stats cache
	s.statsCache.invalidate()

	// Build File
	file := &metadata.File{
		ID:        fileID,
		ShareName: shareName,
		Path:      filePath,
		FileAttr: metadata.FileAttr{
			Type:         metadata.FileTypeRegular,
			Mode:         attr.Mode,
			UID:          attr.UID,
			GID:          attr.GID,
			Size:         attr.Size,
			Atime:        attr.Atime,
			Mtime:        attr.Mtime,
			Ctime:        attr.Ctime,
			CreationTime: attr.CreationTime,
			ContentID:    metadata.ContentID(contentID),
		},
	}

	return file, nil
}

// createDirectory creates a new directory (internal implementation)
func (s *PostgresMetadataStore) createDirectory(
	ctx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	name string,
	attr *metadata.FileAttr,
) (*metadata.File, error) {
	// Validate input
	if err := metadata.ValidateName(name); err != nil {
		return nil, err
	}

	// Apply defaults for mode, UID/GID, timestamps, and size
	attr.Type = metadata.FileTypeDirectory
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
		return nil, mapPgError(err, "createDirectory", name)
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
		return nil, mapPgError(err, "createDirectory", name)
	}

	if exists {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrAlreadyExists,
			Message: "directory already exists",
			Path:    path.Join(parent.Path, name),
		}
	}

	// Generate new directory ID
	dirID := uuid.New()
	dirPath := path.Join(parent.Path, name)

	// Note: We don't validate PATH_MAX here because NFS uses file handles,
	// not paths. PATH_MAX only applies to paths passed to syscalls, but NFS
	// operations traverse component by component via LOOKUP.

	// Insert directory
	insertQuery := `
		INSERT INTO files (
			id, share_name, path,
			file_type, mode, uid, gid, size,
			atime, mtime, ctime, creation_time,
			content_id, link_target, device_major, device_minor
		) VALUES (
			$1, $2, $3,
			$4, $5, $6, $7, $8,
			$9, $10, $11, $12,
			$13, $14, $15, $16
		)
	`

	_, err = tx.Exec(ctx.Context, insertQuery,
		dirID,
		shareName,
		dirPath,
		int16(metadata.FileTypeDirectory),
		int32(attr.Mode),
		int32(attr.UID),
		int32(attr.GID),
		int64(attr.Size),
		attr.Atime,
		attr.Mtime,
		attr.Ctime,
		attr.CreationTime,
		nil, // content_id (NULL for directories)
		nil, // link_target
		nil, // device_major
		nil, // device_minor
	)
	if err != nil {
		return nil, mapPgError(err, "createDirectory", dirPath)
	}

	// Insert into parent_child_map
	insertMapQuery := `
		INSERT INTO parent_child_map (parent_id, child_id, child_name)
		VALUES ($1, $2, $3)
	`

	_, err = tx.Exec(ctx.Context, insertMapQuery, parentID, dirID, name)
	if err != nil {
		return nil, mapPgError(err, "createDirectory", dirPath)
	}

	// Insert into link_counts (directories start with link count = 2: . and parent)
	insertLinkCountQuery := `
		INSERT INTO link_counts (file_id, link_count)
		VALUES ($1, $2)
	`

	_, err = tx.Exec(ctx.Context, insertLinkCountQuery, dirID, 2)
	if err != nil {
		return nil, mapPgError(err, "createDirectory", dirPath)
	}

	// Increment parent's link count (new subdirectory adds ".." reference to parent)
	updateParentLinkCountQuery := `
		UPDATE link_counts
		SET link_count = link_count + 1
		WHERE file_id = $1
	`

	_, err = tx.Exec(ctx.Context, updateParentLinkCountQuery, parentID)
	if err != nil {
		return nil, mapPgError(err, "createDirectory", dirPath)
	}

	// Update parent directory mtime
	updateParentQuery := `
		UPDATE files
		SET mtime = $1, ctime = $1
		WHERE id = $2
	`

	_, err = tx.Exec(ctx.Context, updateParentQuery, attr.Mtime, parentID)
	if err != nil {
		return nil, mapPgError(err, "createDirectory", dirPath)
	}

	// Commit transaction
	if err := tx.Commit(ctx.Context); err != nil {
		return nil, mapPgError(err, "createDirectory", dirPath)
	}

	// Invalidate stats cache
	s.statsCache.invalidate()

	// Build File
	file := &metadata.File{
		ID:        dirID,
		ShareName: shareName,
		Path:      dirPath,
		FileAttr: metadata.FileAttr{
			Type:         metadata.FileTypeDirectory,
			Mode:         attr.Mode,
			UID:          attr.UID,
			GID:          attr.GID,
			Size:         attr.Size,
			Atime:        attr.Atime,
			Mtime:        attr.Mtime,
			Ctime:        attr.Ctime,
			CreationTime: attr.CreationTime,
		},
	}

	return file, nil
}

// CreateFile creates a new regular file
func (s *PostgresMetadataStore) CreateFile(
	ctx context.Context,
	authCtx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	name string,
	mode uint32,
) (metadata.FileHandle, *metadata.FileAttr, error) {
	// Validate input
	if err := metadata.ValidateName(name); err != nil {
		return nil, nil, err
	}

	// Decode parent handle
	shareName, parentID, err := decodeFileHandle(parentHandle)
	if err != nil {
		return nil, nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid parent handle",
		}
	}

	// Begin transaction
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, mapPgError(err, "CreateFile", name)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Get and lock parent directory
	parent, err := s.getFileByIDTx(ctx, tx, parentID, shareName)
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

	// Check write permission on parent
	if err := s.checkAccess(parent, authCtx, metadata.PermissionWrite); err != nil {
		return nil, nil, err
	}

	// Check if child already exists
	checkQuery := `
		SELECT EXISTS(
			SELECT 1 FROM parent_child_map
			WHERE parent_id = $1 AND child_name = $2
		)
	`
	var exists bool
	err = tx.QueryRow(ctx, checkQuery, parentID, name).Scan(&exists)
	if err != nil {
		return nil, nil, mapPgError(err, "CreateFile", name)
	}

	if exists {
		return nil, nil, &metadata.StoreError{
			Code:    metadata.ErrAlreadyExists,
			Message: "file already exists",
			Path:    path.Join(parent.Path, name),
		}
	}

	// Generate new file ID and handle
	fileID := uuid.New()
	filePath := path.Join(parent.Path, name)
	now := time.Now()

	// Note: We don't validate PATH_MAX here because NFS uses file handles,
	// not paths. PATH_MAX only applies to paths passed to syscalls, but NFS
	// operations traverse component by component via LOOKUP.

	// Get effective UID/GID from auth context
	uid := uint32(0)
	gid := uint32(0)
	if authCtx.Identity != nil {
		if authCtx.Identity.UID != nil {
			uid = *authCtx.Identity.UID
		}
		if authCtx.Identity.GID != nil {
			gid = *authCtx.Identity.GID
		}
	}

	// Insert file
	insertFileQuery := `
		INSERT INTO files (
			id, share_name, path,
			file_type, mode, uid, gid, size,
			atime, mtime, ctime, creation_time,
			content_id, link_target, device_major, device_minor
		) VALUES (
			$1, $2, $3,
			$4, $5, $6, $7, $8,
			$9, $10, $11, $12,
			$13, $14, $15, $16
		)
	`

	// Generate content ID for the file (path-based for S3 interoperability)
	contentID := metadata.ContentID(internal.BuildContentID(shareName, filePath))

	_, err = tx.Exec(ctx, insertFileQuery,
		fileID,
		shareName,
		filePath,
		int16(metadata.FileTypeRegular),
		int32(mode&0o7777),
		int32(uid),
		int32(gid),
		int64(0), // size starts at 0
		now,      // atime
		now,      // mtime
		now,      // ctime
		now,      // creation_time
		string(contentID),
		nil, // link_target
		nil, // device_major
		nil, // device_minor
	)
	if err != nil {
		return nil, nil, mapPgError(err, "CreateFile", filePath)
	}

	// Insert into parent_child_map
	insertMapQuery := `
		INSERT INTO parent_child_map (parent_id, child_id, child_name)
		VALUES ($1, $2, $3)
	`

	_, err = tx.Exec(ctx, insertMapQuery, parentID, fileID, name)
	if err != nil {
		return nil, nil, mapPgError(err, "CreateFile", filePath)
	}

	// Insert into link_counts (regular files start with link count = 1)
	insertLinkCountQuery := `
		INSERT INTO link_counts (file_id, link_count)
		VALUES ($1, $2)
	`

	_, err = tx.Exec(ctx, insertLinkCountQuery, fileID, 1)
	if err != nil {
		return nil, nil, mapPgError(err, "CreateFile", filePath)
	}

	// Update parent directory mtime
	updateParentQuery := `
		UPDATE files
		SET mtime = $1, ctime = $1
		WHERE id = $2
	`

	_, err = tx.Exec(ctx, updateParentQuery, now, parentID)
	if err != nil {
		return nil, nil, mapPgError(err, "CreateFile", filePath)
	}

	// Commit transaction
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, mapPgError(err, "CreateFile", filePath)
	}

	// Invalidate stats cache
	s.statsCache.invalidate()

	// Encode handle
	handle, err := metadata.EncodeShareHandle(shareName, fileID)
	if err != nil {
		return nil, nil, &metadata.StoreError{
			Code:    metadata.ErrIOError,
			Message: "failed to encode file handle",
		}
	}

	// Build FileAttr
	attr := &metadata.FileAttr{
		Type:         metadata.FileTypeRegular,
		Mode:         mode & 0o7777,
		UID:          uid,
		GID:          gid,
		Size:         0,
		Atime:        now,
		Mtime:        now,
		Ctime:        now,
		CreationTime: now,
		ContentID:    contentID,
	}

	return handle, attr, nil
}

// CreateDirectory creates a new directory
func (s *PostgresMetadataStore) CreateDirectory(
	ctx context.Context,
	authCtx *metadata.AuthContext,
	parentHandle metadata.FileHandle,
	name string,
	mode uint32,
) (metadata.FileHandle, *metadata.FileAttr, error) {
	// Validate input
	if err := metadata.ValidateName(name); err != nil {
		return nil, nil, err
	}

	// Decode parent handle
	shareName, parentID, err := decodeFileHandle(parentHandle)
	if err != nil {
		return nil, nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid parent handle",
		}
	}

	// Begin transaction
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, mapPgError(err, "CreateDirectory", name)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Get and lock parent directory
	parent, err := s.getFileByIDTx(ctx, tx, parentID, shareName)
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

	// Check write permission on parent
	if err := s.checkAccess(parent, authCtx, metadata.PermissionWrite); err != nil {
		return nil, nil, err
	}

	// Check if child already exists
	checkQuery := `
		SELECT EXISTS(
			SELECT 1 FROM parent_child_map
			WHERE parent_id = $1 AND child_name = $2
		)
	`
	var exists bool
	err = tx.QueryRow(ctx, checkQuery, parentID, name).Scan(&exists)
	if err != nil {
		return nil, nil, mapPgError(err, "CreateDirectory", name)
	}

	if exists {
		return nil, nil, &metadata.StoreError{
			Code:    metadata.ErrAlreadyExists,
			Message: "directory already exists",
			Path:    path.Join(parent.Path, name),
		}
	}

	// Generate new directory ID
	dirID := uuid.New()
	dirPath := path.Join(parent.Path, name)
	now := time.Now()

	// Note: We don't validate PATH_MAX here because NFS uses file handles,
	// not paths. PATH_MAX only applies to paths passed to syscalls, but NFS
	// operations traverse component by component via LOOKUP.

	// Get effective UID/GID from auth context
	uid := uint32(0)
	gid := uint32(0)
	if authCtx.Identity != nil {
		if authCtx.Identity.UID != nil {
			uid = *authCtx.Identity.UID
		}
		if authCtx.Identity.GID != nil {
			gid = *authCtx.Identity.GID
		}
	}

	// Insert directory
	insertDirQuery := `
		INSERT INTO files (
			id, share_name, path,
			file_type, mode, uid, gid, size,
			atime, mtime, ctime, creation_time,
			content_id, link_target, device_major, device_minor
		) VALUES (
			$1, $2, $3,
			$4, $5, $6, $7, $8,
			$9, $10, $11, $12,
			$13, $14, $15, $16
		)
	`

	_, err = tx.Exec(ctx, insertDirQuery,
		dirID,
		shareName,
		dirPath,
		int16(metadata.FileTypeDirectory),
		int32(mode&0o7777),
		int32(uid),
		int32(gid),
		int64(0), // size
		now,      // atime
		now,      // mtime
		now,      // ctime
		now,      // creation_time
		nil,      // content_id (NULL for directories)
		nil,      // link_target
		nil,      // device_major
		nil,      // device_minor
	)
	if err != nil {
		return nil, nil, mapPgError(err, "CreateDirectory", dirPath)
	}

	// Insert into parent_child_map
	insertMapQuery := `
		INSERT INTO parent_child_map (parent_id, child_id, child_name)
		VALUES ($1, $2, $3)
	`

	_, err = tx.Exec(ctx, insertMapQuery, parentID, dirID, name)
	if err != nil {
		return nil, nil, mapPgError(err, "CreateDirectory", dirPath)
	}

	// Insert into link_counts (directories start with link count = 2: "." and parent)
	insertLinkCountQuery := `
		INSERT INTO link_counts (file_id, link_count)
		VALUES ($1, $2)
	`

	_, err = tx.Exec(ctx, insertLinkCountQuery, dirID, 2)
	if err != nil {
		return nil, nil, mapPgError(err, "CreateDirectory", dirPath)
	}

	// Update parent directory mtime and link count (adding subdirectory increments parent link count)
	updateParentQuery := `
		UPDATE files
		SET mtime = $1, ctime = $1
		WHERE id = $2
	`

	_, err = tx.Exec(ctx, updateParentQuery, now, parentID)
	if err != nil {
		return nil, nil, mapPgError(err, "CreateDirectory", dirPath)
	}

	// Commit transaction
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, mapPgError(err, "CreateDirectory", dirPath)
	}

	// Invalidate stats cache
	s.statsCache.invalidate()

	// Encode handle
	handle, err := metadata.EncodeShareHandle(shareName, dirID)
	if err != nil {
		return nil, nil, &metadata.StoreError{
			Code:    metadata.ErrIOError,
			Message: "failed to encode directory handle",
		}
	}

	// Build FileAttr
	attr := &metadata.FileAttr{
		Type:         metadata.FileTypeDirectory,
		Mode:         mode & 0o7777,
		UID:          uid,
		GID:          gid,
		Size:         0,
		Atime:        now,
		Mtime:        now,
		Ctime:        now,
		CreationTime: now,
	}

	return handle, attr, nil
}

// getFileByIDTx retrieves a file by ID within a transaction (with FOR UPDATE lock)
func (s *PostgresMetadataStore) getFileByIDTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, shareName string) (*metadata.File, error) {
	query := `
		SELECT
			id, share_name, path,
			file_type, mode, uid, gid, size,
			atime, mtime, ctime, creation_time,
			content_id, link_target, device_major, device_minor,
			hidden
		FROM files
		WHERE id = $1 AND share_name = $2
		FOR UPDATE
	`

	row := tx.QueryRow(ctx, query, id, shareName)
	file, err := fileRowToFile(row)
	if err != nil {
		return nil, mapPgError(err, "getFileByIDTx", "")
	}

	return file, nil
}

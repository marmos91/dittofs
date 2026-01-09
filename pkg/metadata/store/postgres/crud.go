package postgres

import (
	"context"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// CRUD Operations
// ============================================================================
//
// These methods provide low-level data operations for metadata storage.
// They are thin wrappers around PostgreSQL queries with NO business logic.
// Business logic is handled by shared functions in the metadata package.

// GetEntry retrieves file metadata by handle.
// Returns ErrNotFound if handle doesn't exist.
func (s *PostgresMetadataStore) GetEntry(ctx context.Context, handle metadata.FileHandle) (*metadata.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

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
		return nil, mapPgError(err, "GetEntry", "")
	}

	return file, nil
}

// PutEntry stores or updates file metadata.
// Creates the entry if it doesn't exist.
func (s *PostgresMetadataStore) PutEntry(ctx context.Context, file *metadata.File) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// Upsert the file
	query := `
		INSERT INTO files (
			id, share_name, path, file_type, mode, uid, gid, size,
			atime, mtime, ctime, creation_time, content_id, link_target,
			device_major, device_minor, hidden
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17
		)
		ON CONFLICT (id) DO UPDATE SET
			share_name = EXCLUDED.share_name,
			path = EXCLUDED.path,
			file_type = EXCLUDED.file_type,
			mode = EXCLUDED.mode,
			uid = EXCLUDED.uid,
			gid = EXCLUDED.gid,
			size = EXCLUDED.size,
			atime = EXCLUDED.atime,
			mtime = EXCLUDED.mtime,
			ctime = EXCLUDED.ctime,
			creation_time = EXCLUDED.creation_time,
			content_id = EXCLUDED.content_id,
			link_target = EXCLUDED.link_target,
			device_major = EXCLUDED.device_major,
			device_minor = EXCLUDED.device_minor,
			hidden = EXCLUDED.hidden
	`

	// Extract device major/minor from Rdev
	var deviceMajor, deviceMinor *int32
	if file.Type == metadata.FileTypeBlockDevice || file.Type == metadata.FileTypeCharDevice {
		major := int32(metadata.RdevMajor(file.Rdev))
		minor := int32(metadata.RdevMinor(file.Rdev))
		deviceMajor = &major
		deviceMinor = &minor
	}

	// Convert content ID to nullable string
	var contentID *string
	if file.ContentID != "" {
		s := string(file.ContentID)
		contentID = &s
	}

	// Convert link target to nullable string
	var linkTarget *string
	if file.LinkTarget != "" {
		linkTarget = &file.LinkTarget
	}

	_, err := s.pool.Exec(ctx, query,
		file.ID,
		file.ShareName,
		file.Path,
		int16(file.Type),
		int32(file.Mode),
		int32(file.UID),
		int32(file.GID),
		int64(file.Size),
		file.Atime,
		file.Mtime,
		file.Ctime,
		file.CreationTime,
		contentID,
		linkTarget,
		deviceMajor,
		deviceMinor,
		file.Hidden,
	)
	if err != nil {
		return mapPgError(err, "PutEntry", file.Path)
	}

	// Upsert link count if not exists
	linkCountQuery := `
		INSERT INTO link_counts (file_id, link_count)
		VALUES ($1, $2)
		ON CONFLICT (file_id) DO NOTHING
	`
	initialCount := int32(1)
	if file.Type == metadata.FileTypeDirectory {
		initialCount = 2 // . and parent entry
	}
	_, err = s.pool.Exec(ctx, linkCountQuery, file.ID, initialCount)
	if err != nil {
		return mapPgError(err, "PutEntry:LinkCount", file.Path)
	}

	return nil
}

// DeleteEntry removes file metadata by handle.
// Returns ErrNotFound if handle doesn't exist.
func (s *PostgresMetadataStore) DeleteEntry(ctx context.Context, handle metadata.FileHandle) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	_, id, err := decodeFileHandle(handle)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid file handle",
		}
	}

	// Delete file and related entries (cascades should handle related tables)
	query := `DELETE FROM files WHERE id = $1`
	result, err := s.pool.Exec(ctx, query, id)
	if err != nil {
		return mapPgError(err, "DeleteEntry", "")
	}

	if result.RowsAffected() == 0 {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "file not found",
		}
	}

	return nil
}

// GetChild resolves a name in a directory to a file handle.
// Returns ErrNotFound if name doesn't exist.
func (s *PostgresMetadataStore) GetChild(ctx context.Context, dirHandle metadata.FileHandle, name string) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	shareName, parentID, err := decodeFileHandle(dirHandle)
	if err != nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid directory handle",
		}
	}

	query := `
		SELECT child_id
		FROM parent_child_map
		WHERE parent_id = $1 AND child_name = $2
	`

	var childID uuid.UUID
	err = s.pool.QueryRow(ctx, query, parentID, name).Scan(&childID)
	if err != nil {
		return nil, mapPgError(err, "GetChild", name)
	}

	// Encode the child handle
	return metadata.EncodeShareHandle(shareName, childID)
}

// SetChild adds or updates a child entry in a directory.
func (s *PostgresMetadataStore) SetChild(ctx context.Context, dirHandle metadata.FileHandle, name string, childHandle metadata.FileHandle) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	_, parentID, err := decodeFileHandle(dirHandle)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid directory handle",
		}
	}

	_, childID, err := decodeFileHandle(childHandle)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid child handle",
		}
	}

	query := `
		INSERT INTO parent_child_map (parent_id, child_name, child_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (parent_id, child_name) DO UPDATE SET
			child_id = EXCLUDED.child_id
	`

	_, err = s.pool.Exec(ctx, query, parentID, name, childID)
	if err != nil {
		return mapPgError(err, "SetChild", name)
	}

	return nil
}

// DeleteChild removes a child entry from a directory.
// Returns ErrNotFound if name doesn't exist.
func (s *PostgresMetadataStore) DeleteChild(ctx context.Context, dirHandle metadata.FileHandle, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	_, parentID, err := decodeFileHandle(dirHandle)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid directory handle",
		}
	}

	query := `DELETE FROM parent_child_map WHERE parent_id = $1 AND child_name = $2`
	result, err := s.pool.Exec(ctx, query, parentID, name)
	if err != nil {
		return mapPgError(err, "DeleteChild", name)
	}

	if result.RowsAffected() == 0 {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "child not found",
		}
	}

	return nil
}

// GetParent returns the parent handle for a file/directory.
// Returns ErrNotFound for root directories (no parent).
func (s *PostgresMetadataStore) GetParent(ctx context.Context, handle metadata.FileHandle) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	shareName, id, err := decodeFileHandle(handle)
	if err != nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid file handle",
		}
	}

	query := `SELECT parent_id FROM file_parents WHERE file_id = $1`

	var parentID uuid.UUID
	err = s.pool.QueryRow(ctx, query, id).Scan(&parentID)
	if err != nil {
		return nil, mapPgError(err, "GetParent", "")
	}

	return metadata.EncodeShareHandle(shareName, parentID)
}

// SetParent sets the parent handle for a file/directory.
func (s *PostgresMetadataStore) SetParent(ctx context.Context, handle metadata.FileHandle, parentHandle metadata.FileHandle) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	_, id, err := decodeFileHandle(handle)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid file handle",
		}
	}

	_, parentID, err := decodeFileHandle(parentHandle)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid parent handle",
		}
	}

	query := `
		INSERT INTO file_parents (file_id, parent_id)
		VALUES ($1, $2)
		ON CONFLICT (file_id) DO UPDATE SET
			parent_id = EXCLUDED.parent_id
	`

	_, err = s.pool.Exec(ctx, query, id, parentID)
	if err != nil {
		return mapPgError(err, "SetParent", "")
	}

	return nil
}

// GetLinkCount returns the hard link count for a file.
// Returns 0 if the file doesn't track link counts or doesn't exist.
func (s *PostgresMetadataStore) GetLinkCount(ctx context.Context, handle metadata.FileHandle) (uint32, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	_, id, err := decodeFileHandle(handle)
	if err != nil {
		return 0, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid file handle",
		}
	}

	query := `SELECT link_count FROM link_counts WHERE file_id = $1`

	var count int32
	err = s.pool.QueryRow(ctx, query, id).Scan(&count)
	if err != nil {
		// If not found, return 0 (not an error for link counts)
		if isNotFound(err) {
			return 0, nil
		}
		return 0, mapPgError(err, "GetLinkCount", "")
	}

	return uint32(count), nil
}

// SetLinkCount sets the hard link count for a file.
func (s *PostgresMetadataStore) SetLinkCount(ctx context.Context, handle metadata.FileHandle, count uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	_, id, err := decodeFileHandle(handle)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid file handle",
		}
	}

	query := `
		INSERT INTO link_counts (file_id, link_count)
		VALUES ($1, $2)
		ON CONFLICT (file_id) DO UPDATE SET
			link_count = EXCLUDED.link_count
	`

	_, err = s.pool.Exec(ctx, query, id, int32(count))
	if err != nil {
		return mapPgError(err, "SetLinkCount", "")
	}

	return nil
}

// GenerateHandle creates a new unique file handle for a path in a share.
func (s *PostgresMetadataStore) GenerateHandle(ctx context.Context, shareName string, path string) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// PostgreSQL uses UUID-based handles, path is stored in File struct
	return metadata.GenerateNewHandle(shareName)
}

// GetRootHandle returns the root handle for a share.
// Returns ErrNotFound if the share doesn't exist.
func (s *PostgresMetadataStore) GetRootHandle(ctx context.Context, shareName string) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	query := `SELECT root_dir_id FROM shares WHERE name = $1`

	var rootID uuid.UUID
	err := s.pool.QueryRow(ctx, query, shareName).Scan(&rootID)
	if err != nil {
		return nil, mapPgError(err, "GetRootHandle", shareName)
	}

	return metadata.EncodeShareHandle(shareName, rootID)
}

// GetShareOptions returns the share configuration options.
// Returns ErrNotFound if the share doesn't exist.
func (s *PostgresMetadataStore) GetShareOptions(ctx context.Context, shareName string) (*metadata.ShareOptions, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	query := `
		SELECT
			read_only,
			require_auth,
			allowed_clients,
			denied_clients,
			allowed_auth_methods
		FROM shares
		WHERE name = $1
	`

	var (
		readOnly           bool
		requireAuth        bool
		allowedClients     []string
		deniedClients      []string
		allowedAuthMethods []string
	)

	err := s.pool.QueryRow(ctx, query, shareName).Scan(
		&readOnly,
		&requireAuth,
		&allowedClients,
		&deniedClients,
		&allowedAuthMethods,
	)
	if err != nil {
		return nil, mapPgError(err, "GetShareOptions", shareName)
	}

	return &metadata.ShareOptions{
		ReadOnly:           readOnly,
		RequireAuth:        requireAuth,
		AllowedClients:     allowedClients,
		DeniedClients:      deniedClients,
		AllowedAuthMethods: allowedAuthMethods,
	}, nil
}

// isNotFound checks if the error indicates a not-found condition
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	// Check for pgx no rows error
	return err.Error() == "no rows in result set"
}

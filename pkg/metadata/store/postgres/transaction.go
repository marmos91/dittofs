package postgres

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Transaction Support
// ============================================================================

// postgresTransaction wraps a PostgreSQL transaction for the Transaction interface.
type postgresTransaction struct {
	store *PostgresMetadataStore
	tx    pgx.Tx
}

// WithTransaction executes fn within a PostgreSQL transaction.
//
// If fn returns an error, the transaction is rolled back.
// If fn returns nil, the transaction is committed.
func (s *PostgresMetadataStore) WithTransaction(ctx context.Context, fn func(tx metadata.Transaction) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) // No-op if committed

	ptx := &postgresTransaction{store: s, tx: tx}
	if err := fn(ptx); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// ============================================================================
// Transaction CRUD Operations
// ============================================================================

func (tx *postgresTransaction) GetEntry(ctx context.Context, handle metadata.FileHandle) (*metadata.File, error) {
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

	row := tx.tx.QueryRow(ctx, query, id, shareName)
	file, err := fileRowToFileWithNlink(row)
	if err != nil {
		return nil, mapPgError(err, "GetEntry", "")
	}

	return file, nil
}

func (tx *postgresTransaction) PutEntry(ctx context.Context, file *metadata.File) error {
	if err := ctx.Err(); err != nil {
		return err
	}

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

	var deviceMajor, deviceMinor *int32
	if file.Type == metadata.FileTypeBlockDevice || file.Type == metadata.FileTypeCharDevice {
		major := int32(metadata.RdevMajor(file.Rdev))
		minor := int32(metadata.RdevMinor(file.Rdev))
		deviceMajor = &major
		deviceMinor = &minor
	}

	var contentIDPtr *string
	if file.ContentID != "" {
		str := string(file.ContentID)
		contentIDPtr = &str
	}

	var linkTargetPtr *string
	if file.LinkTarget != "" {
		linkTargetPtr = &file.LinkTarget
	}

	_, err := tx.tx.Exec(ctx, query,
		file.ID, file.ShareName, file.Path,
		file.Type, file.Mode, file.UID, file.GID, file.Size,
		file.Atime, file.Mtime, file.Ctime, file.CreationTime,
		contentIDPtr, linkTargetPtr, deviceMajor, deviceMinor,
		file.Hidden,
	)
	if err != nil {
		return mapPgError(err, "PutEntry", "")
	}

	return nil
}

func (tx *postgresTransaction) DeleteEntry(ctx context.Context, handle metadata.FileHandle) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	shareName, id, err := decodeFileHandle(handle)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid file handle",
		}
	}

	// Delete related records first
	_, _ = tx.tx.Exec(ctx, `DELETE FROM link_counts WHERE file_id = $1`, id)
	_, _ = tx.tx.Exec(ctx, `DELETE FROM directory_children WHERE child_id = $1`, id)
	_, _ = tx.tx.Exec(ctx, `DELETE FROM directory_children WHERE parent_id = $1`, id)

	// Delete the file
	result, err := tx.tx.Exec(ctx, `DELETE FROM files WHERE id = $1 AND share_name = $2`, id, shareName)
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

func (tx *postgresTransaction) GetChild(ctx context.Context, dirHandle metadata.FileHandle, name string) (metadata.FileHandle, error) {
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
		SELECT dc.child_id FROM directory_children dc
		WHERE dc.parent_id = $1 AND dc.child_name = $2
	`

	var childID string
	err = tx.tx.QueryRow(ctx, query, parentID, name).Scan(&childID)
	if err != nil {
		return nil, mapPgError(err, "GetChild", name)
	}

	return encodeFileHandle(shareName, childID)
}

func (tx *postgresTransaction) SetChild(ctx context.Context, dirHandle metadata.FileHandle, name string, childHandle metadata.FileHandle) error {
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
		INSERT INTO directory_children (parent_id, child_name, child_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (parent_id, child_name) DO UPDATE SET child_id = EXCLUDED.child_id
	`

	_, err = tx.tx.Exec(ctx, query, parentID, name, childID)
	if err != nil {
		return mapPgError(err, "SetChild", name)
	}

	return nil
}

func (tx *postgresTransaction) DeleteChild(ctx context.Context, dirHandle metadata.FileHandle, name string) error {
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

	result, err := tx.tx.Exec(ctx, `DELETE FROM directory_children WHERE parent_id = $1 AND child_name = $2`, parentID, name)
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

func (tx *postgresTransaction) ListChildren(ctx context.Context, dirHandle metadata.FileHandle, cursor string, limit int) ([]metadata.DirEntry, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}

	shareName, parentID, err := decodeFileHandle(dirHandle)
	if err != nil {
		return nil, "", &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid directory handle",
		}
	}

	if limit <= 0 {
		limit = 1000
	}

	query := `
		SELECT dc.child_name, dc.child_id, f.file_type, f.mode, f.uid, f.gid, f.size, f.atime, f.mtime, f.ctime
		FROM directory_children dc
		LEFT JOIN files f ON dc.child_id = f.id
		WHERE dc.parent_id = $1 AND dc.child_name > $2
		ORDER BY dc.child_name
		LIMIT $3
	`

	rows, err := tx.tx.Query(ctx, query, parentID, cursor, limit+1)
	if err != nil {
		return nil, "", mapPgError(err, "ListChildren", "")
	}
	defer rows.Close()

	var entries []metadata.DirEntry
	for rows.Next() && len(entries) < limit {
		var name, childIDStr string
		var fileType metadata.FileType
		var mode, uid, gid uint32
		var size uint64
		var atime, mtime, ctime interface{}

		err := rows.Scan(&name, &childIDStr, &fileType, &mode, &uid, &gid, &size, &atime, &mtime, &ctime)
		if err != nil {
			return nil, "", err
		}

		childHandle, err := encodeFileHandle(shareName, childIDStr)
		if err != nil {
			return nil, "", err
		}

		entry := metadata.DirEntry{
			ID:     metadata.HandleToINode(childHandle),
			Name:   name,
			Handle: childHandle,
		}

		entries = append(entries, entry)
	}

	nextCursor := ""
	if len(entries) >= limit {
		nextCursor = entries[len(entries)-1].Name
	}

	return entries, nextCursor, nil
}

func (tx *postgresTransaction) GetParent(ctx context.Context, handle metadata.FileHandle) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	shareName, childID, err := decodeFileHandle(handle)
	if err != nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid file handle",
		}
	}

	query := `SELECT parent_id FROM directory_children WHERE child_id = $1 LIMIT 1`

	var parentIDStr string
	err = tx.tx.QueryRow(ctx, query, childID).Scan(&parentIDStr)
	if err != nil {
		return nil, mapPgError(err, "GetParent", "")
	}

	return encodeFileHandle(shareName, parentIDStr)
}

func (tx *postgresTransaction) SetParent(ctx context.Context, handle metadata.FileHandle, parentHandle metadata.FileHandle) error {
	// In PostgreSQL, parent is tracked via directory_children table
	// This is already handled by SetChild
	return nil
}

func (tx *postgresTransaction) GetLinkCount(ctx context.Context, handle metadata.FileHandle) (uint32, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	_, fileID, err := decodeFileHandle(handle)
	if err != nil {
		return 0, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid file handle",
		}
	}

	var count uint32
	err = tx.tx.QueryRow(ctx, `SELECT link_count FROM link_counts WHERE file_id = $1`, fileID).Scan(&count)
	if err != nil {
		// Not found means count is 0
		return 0, nil
	}

	return count, nil
}

func (tx *postgresTransaction) SetLinkCount(ctx context.Context, handle metadata.FileHandle, count uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	_, fileID, err := decodeFileHandle(handle)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid file handle",
		}
	}

	query := `
		INSERT INTO link_counts (file_id, link_count)
		VALUES ($1, $2)
		ON CONFLICT (file_id) DO UPDATE SET link_count = EXCLUDED.link_count
	`

	_, err = tx.tx.Exec(ctx, query, fileID, count)
	if err != nil {
		return mapPgError(err, "SetLinkCount", "")
	}

	return nil
}

func (tx *postgresTransaction) GetFilesystemMeta(ctx context.Context, shareName string) (*metadata.FilesystemMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	query := `SELECT meta FROM filesystem_meta WHERE share_name = $1`

	var data []byte
	err := tx.tx.QueryRow(ctx, query, shareName).Scan(&data)
	if err != nil {
		// Return defaults if not found
		return &metadata.FilesystemMeta{
			Capabilities: tx.store.capabilities,
		}, nil
	}

	var meta metadata.FilesystemMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}

	return &meta, nil
}

func (tx *postgresTransaction) PutFilesystemMeta(ctx context.Context, shareName string, meta *metadata.FilesystemMeta) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}

	query := `
		INSERT INTO filesystem_meta (share_name, meta)
		VALUES ($1, $2)
		ON CONFLICT (share_name) DO UPDATE SET meta = EXCLUDED.meta
	`

	_, err = tx.tx.Exec(ctx, query, shareName, data)
	if err != nil {
		return mapPgError(err, "PutFilesystemMeta", shareName)
	}

	return nil
}

// ============================================================================
// Additional Store Methods
// ============================================================================

// ListChildren implements the Transaction interface for non-transactional calls.
func (s *PostgresMetadataStore) ListChildren(ctx context.Context, dirHandle metadata.FileHandle, cursor string, limit int) ([]metadata.DirEntry, string, error) {
	var entries []metadata.DirEntry
	var nextCursor string

	err := s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		var err error
		entries, nextCursor, err = tx.ListChildren(ctx, dirHandle, cursor, limit)
		return err
	})

	return entries, nextCursor, err
}

// GetFilesystemMeta retrieves filesystem metadata for a share.
func (s *PostgresMetadataStore) GetFilesystemMeta(ctx context.Context, shareName string) (*metadata.FilesystemMeta, error) {
	var meta *metadata.FilesystemMeta

	err := s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		var err error
		meta, err = tx.GetFilesystemMeta(ctx, shareName)
		return err
	})

	return meta, err
}

// PutFilesystemMeta stores filesystem metadata for a share.
func (s *PostgresMetadataStore) PutFilesystemMeta(ctx context.Context, shareName string, meta *metadata.FilesystemMeta) error {
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.PutFilesystemMeta(ctx, shareName, meta)
	})
}

// CreateShare creates a new share with the given configuration.
func (s *PostgresMetadataStore) CreateShare(ctx context.Context, share *metadata.Share) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	query := `
		INSERT INTO shares (name, options)
		VALUES ($1, $2)
	`

	optionsData, err := json.Marshal(share.Options)
	if err != nil {
		return err
	}

	_, err = s.pool.Exec(ctx, query, share.Name, optionsData)
	if err != nil {
		return mapPgError(err, "CreateShare", share.Name)
	}

	return nil
}

// DeleteShare removes a share and all its metadata.
func (s *PostgresMetadataStore) DeleteShare(ctx context.Context, shareName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	result, err := s.pool.Exec(ctx, `DELETE FROM shares WHERE name = $1`, shareName)
	if err != nil {
		return mapPgError(err, "DeleteShare", shareName)
	}

	if result.RowsAffected() == 0 {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "share not found",
			Path:    shareName,
		}
	}

	return nil
}

// ListShares returns the names of all shares.
func (s *PostgresMetadataStore) ListShares(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx, `SELECT name FROM shares`)
	if err != nil {
		return nil, mapPgError(err, "ListShares", "")
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}

	return names, nil
}

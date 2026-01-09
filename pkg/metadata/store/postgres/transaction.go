package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
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
	defer func() { _ = tx.Rollback(ctx) }() // No-op if committed

	ptx := &postgresTransaction{store: s, tx: tx}
	if err := fn(ptx); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// ============================================================================
// Transaction CRUD Operations
// ============================================================================

func (tx *postgresTransaction) GetFile(ctx context.Context, handle metadata.FileHandle) (*metadata.File, error) {
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
		return nil, mapPgError(err, "GetFile", "")
	}

	return file, nil
}

func (tx *postgresTransaction) PutFile(ctx context.Context, file *metadata.File) error {
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
		return mapPgError(err, "PutFile", "")
	}

	return nil
}

func (tx *postgresTransaction) DeleteFile(ctx context.Context, handle metadata.FileHandle) error {
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
	_, _ = tx.tx.Exec(ctx, `DELETE FROM parent_child_map WHERE child_id = $1`, id)
	_, _ = tx.tx.Exec(ctx, `DELETE FROM parent_child_map WHERE parent_id = $1`, id)

	// Delete the file
	result, err := tx.tx.Exec(ctx, `DELETE FROM files WHERE id = $1 AND share_name = $2`, id, shareName)
	if err != nil {
		return mapPgError(err, "DeleteFile", "")
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
		SELECT dc.child_id FROM parent_child_map dc
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
		INSERT INTO parent_child_map (parent_id, child_name, child_id)
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

	result, err := tx.tx.Exec(ctx, `DELETE FROM parent_child_map WHERE parent_id = $1 AND child_name = $2`, parentID, name)
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
		FROM parent_child_map dc
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

	query := `SELECT parent_id FROM parent_child_map WHERE child_id = $1 LIMIT 1`

	var parentIDStr string
	err = tx.tx.QueryRow(ctx, query, childID).Scan(&parentIDStr)
	if err != nil {
		return nil, mapPgError(err, "GetParent", "")
	}

	return encodeFileHandle(shareName, parentIDStr)
}

func (tx *postgresTransaction) SetParent(ctx context.Context, handle metadata.FileHandle, parentHandle metadata.FileHandle) error {
	// In PostgreSQL, parent is tracked via parent_child_map table
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

func (tx *postgresTransaction) GenerateHandle(ctx context.Context, shareName string, path string) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// PostgreSQL uses UUID-based handles, path is stored in File struct
	return metadata.GenerateNewHandle(shareName)
}

// ============================================================================
// Transaction Shares Operations
// ============================================================================

func (tx *postgresTransaction) GetRootHandle(ctx context.Context, shareName string) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	query := `SELECT root_file_id FROM shares WHERE share_name = $1`

	var rootID uuid.UUID
	err := tx.tx.QueryRow(ctx, query, shareName).Scan(&rootID)
	if err != nil {
		return nil, mapPgError(err, "GetRootHandle", shareName)
	}

	return metadata.EncodeShareHandle(shareName, rootID)
}

func (tx *postgresTransaction) GetShareOptions(ctx context.Context, shareName string) (*metadata.ShareOptions, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	query := `SELECT options FROM shares WHERE share_name = $1`

	var optionsJSON []byte
	err := tx.tx.QueryRow(ctx, query, shareName).Scan(&optionsJSON)
	if err != nil {
		return nil, mapPgError(err, "GetShareOptions", shareName)
	}

	var options metadata.ShareOptions
	if len(optionsJSON) > 0 {
		if err := json.Unmarshal(optionsJSON, &options); err != nil {
			return nil, mapPgError(err, "GetShareOptions", shareName)
		}
	}

	return &options, nil
}

func (tx *postgresTransaction) CreateShare(ctx context.Context, share *metadata.Share) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	optionsData, err := json.Marshal(share.Options)
	if err != nil {
		return err
	}

	// Update options for existing share (created by CreateRootDirectory)
	query := `UPDATE shares SET options = $1 WHERE share_name = $2`
	_, err = tx.tx.Exec(ctx, query, optionsData, share.Name)
	if err != nil {
		return mapPgError(err, "CreateShare", share.Name)
	}

	return nil
}

func (tx *postgresTransaction) UpdateShareOptions(ctx context.Context, shareName string, options *metadata.ShareOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	optionsData, err := json.Marshal(options)
	if err != nil {
		return err
	}

	query := `UPDATE shares SET options = $1 WHERE share_name = $2`
	result, err := tx.tx.Exec(ctx, query, optionsData, shareName)
	if err != nil {
		return mapPgError(err, "UpdateShareOptions", shareName)
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

func (tx *postgresTransaction) DeleteShare(ctx context.Context, shareName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	result, err := tx.tx.Exec(ctx, `DELETE FROM shares WHERE share_name = $1`, shareName)
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

func (tx *postgresTransaction) ListShares(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	rows, err := tx.tx.Query(ctx, `SELECT share_name FROM shares`)
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

func (tx *postgresTransaction) CreateRootDirectory(ctx context.Context, shareName string, attr *metadata.FileAttr) (*metadata.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if shareName == "" {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "share name cannot be empty",
		}
	}

	// Apply defaults
	uid := attr.UID
	gid := attr.GID
	mode := attr.Mode
	if mode == 0 {
		mode = 0o755
	}

	// Check if root directory already exists (idempotent behavior)
	checkQuery := `
		SELECT f.id, f.file_type, f.mode, f.uid, f.gid, f.size,
			   f.atime, f.mtime, f.ctime, f.creation_time, f.hidden
		FROM files f
		WHERE f.share_name = $1 AND f.path = '/'
	`

	var (
		id           string
		fileType     int16
		existingMode int32
		existingUID  int32
		existingGID  int32
		size         int64
		atime        time.Time
		mtime        time.Time
		ctime        time.Time
		creationTime time.Time
		hidden       bool
	)

	err := tx.tx.QueryRow(ctx, checkQuery, shareName).Scan(
		&id, &fileType, &existingMode, &existingUID, &existingGID, &size,
		&atime, &mtime, &ctime, &creationTime, &hidden,
	)

	if err == nil {
		// Root exists - return it
		return &metadata.File{
			ID:        uuid.MustParse(id),
			ShareName: shareName,
			Path:      "/",
			FileAttr: metadata.FileAttr{
				Type:         metadata.FileType(fileType),
				Mode:         uint32(existingMode),
				UID:          uint32(existingUID),
				GID:          uint32(existingGID),
				Size:         uint64(size),
				Atime:        atime,
				Mtime:        mtime,
				Ctime:        ctime,
				CreationTime: creationTime,
				Hidden:       hidden,
			},
		}, nil
	}
	if err != pgx.ErrNoRows {
		return nil, mapPgError(err, "CreateRootDirectory", shareName)
	}

	// Create new root directory
	rootID := uuid.New()
	now := time.Now()

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

	_, err = tx.tx.Exec(ctx, insertFileQuery,
		rootID,                            // id
		shareName,                         // share_name
		"/",                               // path (root)
		int16(metadata.FileTypeDirectory), // file_type
		int32(mode),                       // mode
		int32(uid),                        // uid
		int32(gid),                        // gid
		int64(0),                          // size
		now,                               // atime
		now,                               // mtime
		now,                               // ctime
		now,                               // creation_time
		nil,                               // content_id (NULL for directories)
		nil,                               // link_target (NULL)
		nil,                               // device_major (NULL)
		nil,                               // device_minor (NULL)
	)
	if err != nil {
		return nil, mapPgError(err, "CreateRootDirectory", shareName)
	}

	// Insert into link_counts
	insertLinkCountQuery := `
		INSERT INTO link_counts (file_id, link_count)
		VALUES ($1, $2)
	`

	_, err = tx.tx.Exec(ctx, insertLinkCountQuery, rootID, 2)
	if err != nil {
		return nil, mapPgError(err, "CreateRootDirectory", shareName)
	}

	// Insert into shares table
	insertShareQuery := `
		INSERT INTO shares (share_name, root_file_id)
		VALUES ($1, $2)
		ON CONFLICT (share_name) DO UPDATE
		SET root_file_id = EXCLUDED.root_file_id
	`

	_, err = tx.tx.Exec(ctx, insertShareQuery, shareName, rootID)
	if err != nil {
		return nil, mapPgError(err, "CreateRootDirectory", shareName)
	}

	return &metadata.File{
		ID:        rootID,
		ShareName: shareName,
		Path:      "/",
		FileAttr: metadata.FileAttr{
			Type:         metadata.FileTypeDirectory,
			Mode:         mode,
			UID:          uid,
			GID:          gid,
			Size:         0,
			Atime:        now,
			Mtime:        now,
			Ctime:        now,
			CreationTime: now,
		},
	}, nil
}

// ============================================================================
// Transaction ServerConfig Operations
// ============================================================================

func (tx *postgresTransaction) SetServerConfig(ctx context.Context, config metadata.MetadataServerConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	query := `
		INSERT INTO server_config (id, config)
		VALUES (1, $1)
		ON CONFLICT (id) DO UPDATE
		SET config = EXCLUDED.config, updated_at = NOW()
	`

	_, err := tx.tx.Exec(ctx, query, config.CustomSettings)
	if err != nil {
		return mapPgError(err, "SetServerConfig", "")
	}

	return nil
}

func (tx *postgresTransaction) GetServerConfig(ctx context.Context) (metadata.MetadataServerConfig, error) {
	if err := ctx.Err(); err != nil {
		return metadata.MetadataServerConfig{}, err
	}

	query := `SELECT config FROM server_config WHERE id = 1`

	var customSettings map[string]any
	err := tx.tx.QueryRow(ctx, query).Scan(&customSettings)
	if err != nil {
		return metadata.MetadataServerConfig{}, mapPgError(err, "GetServerConfig", "")
	}

	return metadata.MetadataServerConfig{
		CustomSettings: customSettings,
	}, nil
}

func (tx *postgresTransaction) GetFilesystemCapabilities(ctx context.Context, handle metadata.FileHandle) (*metadata.FilesystemCapabilities, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Return cached capabilities
	caps := tx.store.capabilities
	return &caps, nil
}

func (tx *postgresTransaction) SetFilesystemCapabilities(capabilities metadata.FilesystemCapabilities) {
	tx.store.capabilities = capabilities
}

func (tx *postgresTransaction) GetFilesystemStatistics(ctx context.Context, handle metadata.FileHandle) (*metadata.FilesystemStatistics, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	query := `
		SELECT
			COALESCE(SUM(size), 0) AS total_bytes_used,
			COUNT(*) AS total_files_used
		FROM files
	`

	var bytesUsed, filesUsed int64
	err := tx.tx.QueryRow(ctx, query).Scan(&bytesUsed, &filesUsed)
	if err != nil {
		return nil, mapPgError(err, "GetFilesystemStatistics", "")
	}

	stats := metadata.FilesystemStatistics{
		TotalBytes:     1 << 50, // 1 PB (effectively unlimited)
		AvailableBytes: (1 << 50) - uint64(bytesUsed),
		UsedBytes:      uint64(bytesUsed),
		TotalFiles:     1 << 32, // 4 billion files
		AvailableFiles: (1 << 32) - uint64(filesUsed),
		UsedFiles:      uint64(filesUsed),
	}

	return &stats, nil
}

// ============================================================================
// Transaction Files Operations (additional)
// ============================================================================

func (tx *postgresTransaction) GetFileByContentID(ctx context.Context, contentID metadata.ContentID) (*metadata.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

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

	row := tx.tx.QueryRow(ctx, query, string(contentID))
	file, err := fileRowToFileWithNlink(row)
	if err != nil {
		return nil, mapPgError(err, "GetFileByContentID", string(contentID))
	}

	return file, nil
}

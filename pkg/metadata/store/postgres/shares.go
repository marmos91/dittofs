package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Handle/Share Operations
// ============================================================================

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

// ============================================================================
// Share Lifecycle Operations
// ============================================================================

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

// UpdateShareOptions updates the share configuration options.
func (s *PostgresMetadataStore) UpdateShareOptions(ctx context.Context, shareName string, options *metadata.ShareOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	query := `
		UPDATE shares
		SET read_only = $1,
		    require_auth = $2,
		    allowed_clients = $3,
		    denied_clients = $4,
		    allowed_auth_methods = $5
		WHERE name = $6
	`

	result, err := s.pool.Exec(ctx, query,
		options.ReadOnly,
		options.RequireAuth,
		options.AllowedClients,
		options.DeniedClients,
		options.AllowedAuthMethods,
		shareName,
	)
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

// ============================================================================
// Root Directory Operations
// ============================================================================

// CreateRootDirectory creates the root directory for a share
func (s *PostgresMetadataStore) CreateRootDirectory(
	ctx context.Context,
	shareName string,
	attr *metadata.FileAttr,
) (*metadata.File, error) {
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

	s.logger.Info("Creating root directory",
		"share", shareName,
		"uid", uid,
		"gid", gid,
	)

	// Check if root directory already exists (idempotent behavior)
	existingRoot, err := s.getExistingRootDirectory(ctx, shareName)
	if err == nil && existingRoot != nil {
		// Check if root directory attributes need to be updated from config
		// This handles the case where the config changed since the share was first created
		needsUpdate := false
		if mode != 0 && existingRoot.Mode != mode {
			s.logger.Info("Updating root directory mode from config",
				"share", shareName,
				"oldMode", fmt.Sprintf("%o", existingRoot.Mode),
				"newMode", fmt.Sprintf("%o", mode))
			existingRoot.Mode = mode
			needsUpdate = true
		}
		if existingRoot.UID != uid {
			s.logger.Info("Updating root directory UID from config",
				"share", shareName,
				"oldUID", existingRoot.UID,
				"newUID", uid)
			existingRoot.UID = uid
			needsUpdate = true
		}
		if existingRoot.GID != gid {
			s.logger.Info("Updating root directory GID from config",
				"share", shareName,
				"oldGID", existingRoot.GID,
				"newGID", gid)
			existingRoot.GID = gid
			needsUpdate = true
		}

		if needsUpdate {
			now := time.Now()
			updateQuery := `
				UPDATE files
				SET mode = $1, uid = $2, gid = $3, ctime = $4
				WHERE id = $5
			`
			_, err := s.pool.Exec(ctx, updateQuery,
				int32(existingRoot.Mode),
				int32(existingRoot.UID),
				int32(existingRoot.GID),
				now,
				existingRoot.ID,
			)
			if err != nil {
				return nil, mapPgError(err, "UpdateRootDirectory", shareName)
			}
			existingRoot.Ctime = now
			s.logger.Info("Root directory attributes updated from config",
				"share", shareName,
				"root_id", existingRoot.ID)
		} else {
			s.logger.Info("Root directory already exists, returning existing",
				"share", shareName,
				"root_id", existingRoot.ID,
			)
		}
		return existingRoot, nil
	}

	// Begin transaction
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, mapPgError(err, "CreateRootDirectory", shareName)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Generate UUID for root directory
	rootID := uuid.New()

	now := time.Now()

	// Insert root directory file
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

	_, err = tx.Exec(ctx, insertFileQuery,
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

	// Insert into link_counts (directories start with link count = 2: "." and parent reference)
	insertLinkCountQuery := `
		INSERT INTO link_counts (file_id, link_count)
		VALUES ($1, $2)
	`

	_, err = tx.Exec(ctx, insertLinkCountQuery, rootID, 2)
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

	_, err = tx.Exec(ctx, insertShareQuery, shareName, rootID)
	if err != nil {
		return nil, mapPgError(err, "CreateRootDirectory", shareName)
	}

	// Commit transaction
	if err := tx.Commit(ctx); err != nil {
		return nil, mapPgError(err, "CreateRootDirectory", shareName)
	}

	s.logger.Info("Root directory created successfully",
		"share", shareName,
		"root_id", rootID,
	)

	// Build File
	file := &metadata.File{
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
	}

	return file, nil
}

// getExistingRootDirectory checks if a root directory already exists for the share
// and returns it if found. Returns nil, nil if not found.
func (s *PostgresMetadataStore) getExistingRootDirectory(ctx context.Context, shareName string) (*metadata.File, error) {
	query := `
		SELECT f.id, f.file_type, f.mode, f.uid, f.gid, f.size,
			   f.atime, f.mtime, f.ctime, f.creation_time, f.hidden
		FROM files f
		WHERE f.share_name = $1 AND f.path = '/'
	`

	var (
		id           uuid.UUID
		fileType     int16
		mode         int32
		uid          int32
		gid          int32
		size         int64
		atime        time.Time
		mtime        time.Time
		ctime        time.Time
		creationTime time.Time
		hidden       bool
	)

	err := s.pool.QueryRow(ctx, query, shareName).Scan(
		&id,
		&fileType,
		&mode,
		&uid,
		&gid,
		&size,
		&atime,
		&mtime,
		&ctime,
		&creationTime,
		&hidden,
	)

	if err == pgx.ErrNoRows {
		return nil, nil // Not found, not an error
	}
	if err != nil {
		return nil, err
	}

	return &metadata.File{
		ID:        id,
		ShareName: shareName,
		Path:      "/",
		FileAttr: metadata.FileAttr{
			Type:         metadata.FileType(fileType),
			Mode:         uint32(mode),
			UID:          uint32(uid),
			GID:          uint32(gid),
			Size:         uint64(size),
			Atime:        atime,
			Mtime:        mtime,
			Ctime:        ctime,
			CreationTime: creationTime,
			Hidden:       hidden,
		},
	}, nil
}


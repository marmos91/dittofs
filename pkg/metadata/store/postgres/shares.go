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

	query := `SELECT root_file_id FROM shares WHERE share_name = $1`

	var rootID uuid.UUID
	err := s.queryRow(ctx, query, shareName).Scan(&rootID)
	if err != nil {
		return nil, mapPgError(err, "GetRootHandle", shareName)
	}

	return metadata.EncodeShareHandle(shareName, rootID)
}

// GetShareOptions returns the share configuration options.
// Returns ErrNotFound if the share doesn't exist.
//
// The block_layout column is read alongside the legacy options JSON
// blob and overrides whatever the JSON happens to contain — the
// dedicated column is the authoritative source per
// (D-A6). Empty / NULL values coerce to legacy via
// ParseBlockLayout for forward-compat with pre-migration rows.
func (s *PostgresMetadataStore) GetShareOptions(ctx context.Context, shareName string) (*metadata.ShareOptions, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	query := `SELECT options, block_layout FROM shares WHERE share_name = $1`

	var (
		optionsJSON     []byte
		blockLayoutText string
	)
	err := s.queryRow(ctx, query, shareName).Scan(&optionsJSON, &blockLayoutText)
	if err != nil {
		return nil, mapPgError(err, "GetShareOptions", shareName)
	}

	var options metadata.ShareOptions
	if len(optionsJSON) > 0 {
		if err := json.Unmarshal(optionsJSON, &options); err != nil {
			return nil, fmt.Errorf("failed to unmarshal share options: %w", err)
		}
	}

	// Authoritative: the dedicated block_layout column overrides any
	// stale value embedded in the JSON blob. ParseBlockLayout coerces
	// the empty-string default (DEFAULT 'legacy' in the schema, but
	// also any pre-migration row with NULL→"" via the COALESCE-like
	// behavior of TEXT NOT NULL DEFAULT) into BlockLayoutLegacy.
	// Unknown values surface ErrInvalidBlockLayout.
	layout, err := metadata.ParseBlockLayout(blockLayoutText)
	if err != nil {
		return nil, fmt.Errorf("share %q: %w", shareName, err)
	}
	options.BlockLayout = layout

	return &options, nil
}

// ============================================================================
// Share Lifecycle Operations
// ============================================================================

// CreateShare creates a new share with the given configuration.
//
// The block_layout column is populated from share.Options.BlockLayout;
// an unset / zero-value field is normalized through ParseBlockLayout
// (so it stores as 'legacy', matching the schema DEFAULT and D-A6
// safe-default semantics).
func (s *PostgresMetadataStore) CreateShare(ctx context.Context, share *metadata.Share) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// The shares.root_file_id column is NOT NULL with an FK to files(id), so
	// a share row cannot exist before its root inode — a bare
	// INSERT INTO shares (share_name, options, ...) is structurally
	// impossible (it raises a not_null_violation on root_file_id) and never
	// once succeeded. Honour the documented contract ("Also creates the root
	// directory for the share", matching the memory/badger backends) by
	// materializing a default root directory, which inserts the shares row
	// via CreateRootDirectory's ON CONFLICT upsert, then persisting the
	// caller's options. Callers wanting specific root attrs invoke
	// CreateRootDirectory afterward; it is idempotent and updates the
	// existing root in place (no orphaned inode).

	// Validate the block layout BEFORE creating any rows so an invalid value
	// can't leave a half-created share (root inode materialized, options
	// rejected). UpdateShareOptions re-parses it; this is the early guard.
	if _, err := metadata.ParseBlockLayout(string(share.Options.BlockLayout)); err != nil {
		return fmt.Errorf("create share %q: %w", share.Name, err)
	}

	// Duplicate detection: a share is "created" once its root inode exists.
	// This read is the common-case fast path; it is not the integrity
	// authority. Two creators racing the same name both pass this check and
	// reach CreateRootDirectory, but the partial unique index
	// unique_share_path_hash_active on files(share_name, path_hash) admits
	// only one root inode at path "/" — the loser's insert fails rather than
	// silently orphaning an inode. (Production also serializes share creation
	// upstream in the control plane.)
	existing, err := s.getExistingRootDirectory(ctx, share.Name)
	if err != nil {
		return fmt.Errorf("create share %q: check existing: %w", share.Name, err)
	}
	if existing != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrAlreadyExists,
			Message: "share already exists",
			Path:    share.Name,
		}
	}

	rootAttr := &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o755,
	}
	if _, err := s.CreateRootDirectory(ctx, share.Name, rootAttr); err != nil {
		return fmt.Errorf("create share %q root directory: %w", share.Name, err)
	}

	// Persist the requested options + block layout on the freshly-inserted
	// row (CreateRootDirectory seeds only share_name + root_file_id, leaving
	// options/block_layout at their column defaults). UpdateShareOptions
	// applies the same ParseBlockLayout normalization the old INSERT did.
	if err := s.UpdateShareOptions(ctx, share.Name, &share.Options); err != nil {
		return fmt.Errorf("create share %q options: %w", share.Name, err)
	}

	return nil
}

// UpdateShareOptions updates the share configuration options.
//
// The block_layout column is updated alongside the JSON options blob.
// This is how `dfsctl blockstore migrate` flips a share from `legacy`
// to `cas-only` once the integrity check passes (D-A7); the operation
// is a single SQL UPDATE so the flip is atomic with whatever other
// option changes the migration tool wants to bundle.
func (s *PostgresMetadataStore) UpdateShareOptions(ctx context.Context, shareName string, options *metadata.ShareOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	optionsData, err := json.Marshal(options)
	if err != nil {
		return fmt.Errorf("failed to marshal share options: %w", err)
	}

	layout, err := metadata.ParseBlockLayout(string(options.BlockLayout))
	if err != nil {
		return fmt.Errorf("share %q: %w", shareName, err)
	}

	query := `UPDATE shares SET options = $1, block_layout = $2 WHERE share_name = $3`
	result, err := s.exec(ctx, query, optionsData, string(layout), shareName)
	if err != nil {
		return err
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

// DeleteShare removes a share and all its metadata. Runs inside a
// transaction so the share row and its file rows are dropped atomically
// (see the tx-path for the cascade rationale).
func (s *PostgresMetadataStore) DeleteShare(ctx context.Context, shareName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		return tx.DeleteShare(ctx, shareName)
	})
}

// ListShares returns the names of all shares.
func (s *PostgresMetadataStore) ListShares(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	rows, err := s.query(ctx, `SELECT share_name FROM shares`)
	if err != nil {
		return nil, err
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
			_, err := s.exec(ctx, updateQuery,
				int32(existingRoot.Mode),
				int32(existingRoot.UID),
				int32(existingRoot.GID),
				now,
				existingRoot.ID,
			)
			if err != nil {
				return nil, err
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

	// Begin transaction with connection acquire timeout
	tx, err := s.beginTx(ctx)
	if err != nil {
		return nil, err
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

	err := s.queryRow(ctx, query, shareName).Scan(
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

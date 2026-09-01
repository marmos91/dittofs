package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/internal/sharecache"
	"github.com/marmos91/dittofs/pkg/metadata/store/internal/sqlcodec"
)

// ============================================================================
// Handle/Share Operations
// ============================================================================

// GetShareOptions returns the share configuration options, reporting
// ErrNotFound if the share does not exist.
//
// This shadows the promoted Core.GetShareOptions to put the share cache in
// front of it: every permission check funnels through this read, so the SELECT
// and decode are worth skipping. The returned value is always a deep copy, so
// a caller can never reach the shared cache entry.
func (s *PostgresMetadataStore) GetShareOptions(ctx context.Context, shareName string) (*metadata.ShareOptions, error) {
	// Ahead of the cache lookup, not just inside the backing read: a hit must
	// not report success for a request whose context has already given out.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if cached, ok := s.shareCache.Get(shareName); ok {
		return sharecache.Clone(cached), nil
	}

	// Snapshot the invalidation generation BEFORE the backing read so a write
	// that races this read cannot leave a stale value cached (Store checks it).
	gen := s.shareCache.Generation()

	options, err := s.Core.GetShareOptions(ctx, shareName)
	if err != nil {
		return nil, err
	}

	s.shareCache.Store(shareName, options, gen)
	return sharecache.Clone(options), nil
}

// ============================================================================
// Share Lifecycle Operations
// ============================================================================

// CreateShare creates a new share with the given configuration.
func (s *PostgresMetadataStore) CreateShare(ctx context.Context, share *metadata.Share) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// The shares.root_file_id column is NOT NULL with an FK to inodes(id), so
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

	// Duplicate detection: a share is "created" once its root inode exists.
	// This read is the common-case fast path; it is not the integrity
	// authority. The shares table PRIMARY KEY(share_name) is the authority —
	// two creators racing the same name both pass this check and reach
	// CreateRootDirectory, but the second INSERT INTO shares conflicts on the
	// primary key (the ON CONFLICT upsert just re-points root_file_id, leaving
	// at most one root). (Production also serializes share creation upstream in
	// the control plane.)
	existing, err := s.getExistingRootDirectory(ctx, s.queryRow, share.Name)
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

	// Root-inode insert and options write run in ONE transaction so the share
	// can never be left half-created (root materialized but options stuck at
	// their column defaults) if the second step fails or the process crashes
	// between them. CreateRootDirectory seeds only share_name + root_file_id,
	// so the options UPDATE finishes the row.
	return s.WithTransaction(ctx, func(tx metadata.Transaction) error {
		if _, err := tx.CreateRootDirectory(ctx, share.Name, rootAttr); err != nil {
			return fmt.Errorf("create share %q root directory: %w", share.Name, err)
		}
		if err := tx.UpdateShareOptions(ctx, share.Name, &share.Options); err != nil {
			return fmt.Errorf("create share %q options: %w", share.Name, err)
		}
		return nil
	})
}

// UpdateShareOptions updates the share configuration options.
func (s *PostgresMetadataStore) UpdateShareOptions(ctx context.Context, shareName string, options *metadata.ShareOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	optionsData, err := json.Marshal(options)
	if err != nil {
		return fmt.Errorf("failed to marshal share options: %w", err)
	}

	query := `UPDATE shares SET options = $1 WHERE share_name = $2`
	result, err := s.exec(ctx, query, optionsData, shareName)
	// Drop the cached options AFTER the write lands, whatever it reported: an
	// extra invalidation costs a re-read, a missed one is a stale permission.
	s.shareCache.InvalidateAll()
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

// ============================================================================
// Root Directory Operations
// ============================================================================

// CreateRootDirectory creates the root directory for a share.
//
// The whole probe-then-create sequence runs in one transaction through
// WithTransaction, so a serialization failure or deadlock retries under the
// package's backoff and a concurrent caller cannot slip between the probe and
// the insert and leave an orphaned root inode behind.
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

	var root *metadata.File
	err := s.WithTransaction(ctx, func(mtx metadata.Transaction) error {
		ptx := mtx.(*postgresTransaction)
		// The body rewrites the shares row without going through the tx method
		// that would flag it, so flag it here.
		ptx.sharesDirty = true
		var txErr error
		root, txErr = s.createRootDirectoryTx(ctx, ptx.tx, shareName, attr)
		return txErr
	})
	if err != nil {
		return nil, err
	}
	return root, nil
}

// createRootDirectoryTx holds the probe/reconcile/create body, run against the
// caller's transaction.
func (s *PostgresMetadataStore) createRootDirectoryTx(
	ctx context.Context,
	tx pgx.Tx,
	shareName string,
	attr *metadata.FileAttr,
) (*metadata.File, error) {
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
	existingRoot, err := s.getExistingRootDirectory(ctx, tx.QueryRow, shareName)
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
				UPDATE inodes
				SET mode = $1, uid = $2, gid = $3, ctime = $4
				WHERE id = $5
			`
			_, err := tx.Exec(ctx, updateQuery,
				int32(existingRoot.Mode),
				int32(existingRoot.UID),
				int32(existingRoot.GID),
				sqlcodec.TimeToFiletime(now),
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

	// Generate UUID for root directory
	rootID := uuid.New()

	now := time.Now()

	// Insert root directory inode. Directories start with nlink = 2 ("." and the
	// parent's entry). nlink is the sole source of truth for the hard-link count
	// (#1166).
	insertFileQuery := `
		INSERT INTO inodes (
			id, share_name,
			file_type, mode, uid, gid, size,
			atime, mtime, ctime, creation_time,
			content_id, link_target, device_major, device_minor, nlink
		) VALUES (
			$1, $2,
			$3, $4, $5, $6, $7,
			$8, $9, $10, $11,
			$12, $13, $14, $15, 2
		)
	`

	_, err = tx.Exec(ctx, insertFileQuery,
		rootID,                            // id
		shareName,                         // share_name
		int16(metadata.FileTypeDirectory), // file_type
		int32(mode),                       // mode
		int32(uid),                        // uid
		int32(gid),                        // gid
		int64(0),                          // size
		sqlcodec.TimeToFiletime(now),      // atime
		sqlcodec.TimeToFiletime(now),      // mtime
		sqlcodec.TimeToFiletime(now),      // ctime
		sqlcodec.TimeToFiletime(now),      // creation_time
		nil,                               // content_id (NULL for directories)
		nil,                               // link_target (NULL)
		nil,                               // device_major (NULL)
		nil,                               // device_minor (NULL)
	)
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
			Nlink:        2, // Root directories have 2 links ("." and parent's entry)
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
func (s *PostgresMetadataStore) getExistingRootDirectory(ctx context.Context, queryRow rowQuerier, shareName string) (*metadata.File, error) {
	// Resolve the root inode via shares.root_file_id (the share row is the
	// authoritative pointer to its root) now that the path column is gone (#1166).
	query := `
		SELECT f.id, f.file_type, f.mode, f.uid, f.gid, f.size,
			   f.atime, f.mtime, f.ctime, f.creation_time, f.hidden, f.nlink
		FROM inodes f
		WHERE f.id = (SELECT root_file_id FROM shares WHERE share_name = $1)
	`

	var (
		id           uuid.UUID
		fileType     int16
		mode         int32
		uid          int32
		gid          int32
		size         int64
		atime        int64
		mtime        int64
		ctime        int64
		creationTime int64
		hidden       bool
		nlink        int32
	)

	err := queryRow(ctx, query, shareName).Scan(
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
		&nlink,
	)

	if errors.Is(err, pgx.ErrNoRows) {
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
			Nlink:        uint32(nlink),
			UID:          uint32(uid),
			GID:          uint32(gid),
			Size:         uint64(size),
			Atime:        sqlcodec.FiletimeToTime(atime),
			Mtime:        sqlcodec.FiletimeToTime(mtime),
			Ctime:        sqlcodec.FiletimeToTime(ctime),
			CreationTime: sqlcodec.FiletimeToTime(creationTime),
			Hidden:       hidden,
		},
	}, nil
}

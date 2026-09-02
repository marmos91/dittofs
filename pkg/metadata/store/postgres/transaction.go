package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/basestore"
	"github.com/marmos91/dittofs/pkg/metadata/store/internal/sqlcodec"
	"github.com/marmos91/dittofs/pkg/metadata/store/internal/txretry"

	storesql "github.com/marmos91/dittofs/pkg/metadata/store/sql"
)

// Transaction retry policy (#1769). Under write contention DittoFS must
// backpressure — block-and-retry until a real budget elapses — not surface EIO
// to the caller after a fixed handful of attempts. Only the already-classified
// transient conflicts (40001 serialization_failure / 40P01 deadlock_detected)
// are retried; non-transient errors return immediately. The deadline and
// jittered backoff are shared with the sqlite backend in internal/txretry.

// ============================================================================
// Transaction Support
// ============================================================================

// postgresTransaction wraps a PostgreSQL transaction for the Transaction interface.
//
// Mutations accumulate their usage side effects on the transaction rather than
// applying them inline, and WithTransaction applies them exactly once after a
// successful Commit. WithTransaction retries the closure on a 40P01/40001 serialization failure;
// accumulating per-attempt and applying post-commit prevents double-counting
// across retries.
type postgresTransaction struct {
	// Core runs the shared SQL bodies on THIS transaction, not the pool. A
	// body reached through the pool would run on a separate connection and
	// survive this transaction's rollback.
	*storesql.Core

	store *PostgresMetadataStore
	tx    pgx.Tx
	// quota accumulates usage changes (bytes + file count) keyed by share and
	// owner identity. Applied to the store's quota cache exactly once after a
	// successful commit, so a serialization/deadlock retry never double-counts.
	quota basestore.QuotaDelta
	// sharesDirty records that this transaction wrote a share record, so the
	// store's ShareOptions cache is dropped after the commit. A stale entry is
	// a wrong permission decision, and shares are few enough that clearing the
	// whole cache costs one re-read each.
	sharesDirty bool
}

// isRetryableError checks if a PostgreSQL error is retryable (deadlock or serialization failure)
func isRetryableError(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "40P01": // deadlock_detected
			return true
		case "40001": // serialization_failure
			return true
		}
	}
	return false
}

// WithTransaction executes fn within a PostgreSQL transaction.
//
// If fn returns an error, the transaction is rolled back.
// If fn returns nil, the transaction is committed.
// Retries automatically on deadlock or serialization failures.
//
// Connection acquisition has a timeout to prevent indefinite blocking when
// the pool is exhausted under high concurrent load.
func (s *PostgresMetadataStore) WithTransaction(ctx context.Context, fn func(tx metadata.Transaction) error) error {
	return s.withTransaction(ctx, fn, false)
}

// WithTransactionRelaxed runs fn like WithTransaction but, when the store is
// configured for relaxed durability, sets synchronous_commit=off for the
// transaction so its commit does not wait for a WAL fsync (#1573 Wall 1). Use
// ONLY for pure-namespace/attr writes not paired with block data. Data-paired
// writes must use WithTransaction so their WAL is flushed synchronously (#588).
// In strict mode this is identical to WithTransaction.
func (s *PostgresMetadataStore) WithTransactionRelaxed(ctx context.Context, fn func(tx metadata.Transaction) error) error {
	return s.withTransaction(ctx, fn, true)
}

func (s *PostgresMetadataStore) withTransaction(ctx context.Context, fn func(tx metadata.Transaction) error, relaxed bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// synchronous_commit=off is honored only for relaxed namespace writes AND
	// only when the operator opted in. SET LOCAL is transaction-scoped, so it
	// resets at COMMIT and never leaks across pooled connections.
	relaxCommit := relaxed && s.config.RelaxedDurability

	// Backpressure deadline (#1769): retry transient conflicts until this budget
	// elapses rather than EIOing after a fixed attempt count.
	deadline := txretry.Deadline(ctx)

	var lastErr error
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		// Use a timeout for connection acquisition to prevent indefinite blocking
		// when the pool is exhausted. This is critical under high concurrent load
		// (e.g., POSIX compliance tests) where all connections might be in use.
		acquireCtx, cancel := context.WithTimeout(ctx, poolConnectionAcquireTimeout)
		tx, err := s.pool.Begin(acquireCtx)
		cancel() // Release timer resources immediately after Begin returns

		if err != nil {
			// Check if it was a timeout acquiring connection
			if errors.Is(err, context.DeadlineExceeded) {
				stats := s.pool.Stat()
				s.logger.Warn("Connection pool exhausted, timed out acquiring connection",
					"timeout", poolConnectionAcquireTimeout,
					"attempt", attempt+1,
					"max_conns", s.config.MaxConns,
					"total_conns", stats.TotalConns(),
					"acquired_conns", stats.AcquiredConns(),
					"idle_conns", stats.IdleConns(),
					"constructing_conns", stats.ConstructingConns())
			}
			return err
		}

		// Relaxed namespace write: drop synchronous_commit for this transaction
		// only. SET LOCAL is scoped to the tx and reset at COMMIT/ROLLBACK, so
		// pooled connections are never left in an off state. A failure here is
		// non-fatal — fall through to a fully-durable commit rather than abort.
		if relaxCommit {
			if _, serr := tx.Exec(ctx, "SET LOCAL synchronous_commit = off"); serr != nil {
				s.logger.Warn("relaxed durability: SET LOCAL synchronous_commit failed; committing synchronously",
					"error", serr)
			}
		}

		ptx := &postgresTransaction{store: s, tx: tx}
		ptx.Core = &storesql.Core{X: txExecer{tx: tx}, D: pgDialect, Caps: s.currentCapabilities}
		if err := fn(ptx); err != nil {
			// Apply timeout to rollback to prevent indefinite blocking
			rollbackCtx, rollbackCancel := context.WithTimeout(ctx, poolConnectionAcquireTimeout)
			_ = tx.Rollback(rollbackCtx)
			rollbackCancel()
			if isRetryableError(err) {
				lastErr = err
				if txretry.Backoff(ctx, deadline, attempt) {
					continue
				}
				break
			}
			return err
		}

		// Apply timeout to commit to prevent indefinite blocking if PostgreSQL is slow
		// (e.g., during checkpoint or WAL flush)
		commitCtx, commitCancel := context.WithTimeout(ctx, poolConnectionAcquireTimeout)
		if err := tx.Commit(commitCtx); err != nil {
			commitCancel()
			if isRetryableError(err) {
				lastErr = err
				if txretry.Backoff(ctx, deadline, attempt) {
					continue
				}
				break
			}
			return err
		}
		commitCancel()

		// Drop the ShareOptions cache once, after the commit, so a reader that
		// saw the pre-commit value cannot leave it cached (its generation-guarded
		// populate loses).
		if ptx.sharesDirty {
			s.shareCache.InvalidateAll()
		}
		// Apply the accumulated usage deltas exactly once, after commit.
		s.applyQuotaDelta(ptx.quota.Map())
		return nil // Success
	}

	// Budget exhausted (or ctx done) while backing off on a transient conflict.
	if err := ctx.Err(); err != nil {
		return err
	}
	return mapPgError(lastErr, "WithTransaction", "")
}

// ============================================================================
// Transaction CRUD Operations
// ============================================================================

// UpdateAttrs persists the file's attributes and leaves the stored
// file_block_refs manifest untouched.
func (tx *postgresTransaction) UpdateAttrs(ctx context.Context, file *metadata.File) error {
	return tx.putFile(ctx, file, false)
}

// SetManifest persists the file's attributes and rewrites the stored
// file_block_refs manifest from file.Blocks.
func (tx *postgresTransaction) SetManifest(ctx context.Context, file *metadata.File) error {
	return tx.putFile(ctx, file, true)
}

func (tx *postgresTransaction) putFile(ctx context.Context, file *metadata.File, writeManifest bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// For existing files (updates), use UPDATE directly to avoid unique constraint issues.
	// This is more efficient and handles concurrent updates properly.
	//
	// Namespace uniqueness lives in parent_child_map(parent_id, child_name); the
	// inodes row no longer carries a path/path_hash column (#1166), so a Move is
	// just a parent_child_map re-link (SetChild/DeleteChild) — there is nothing
	// path-related to update on the inode row here. content_id is still set:
	// it keys file_blocks and is consumed by GetFileByPayloadID.
	//
	// The leading CTE reads the pre-update size under FOR UPDATE and the UPDATE
	// joins against it, so a single round-trip both locks the row and returns
	// its old size for usedBytes delta tracking. This replaces a separate
	// SELECT-then-UPDATE that left a serialization window between the two
	// statements (the read size could be stale by the time the UPDATE ran).
	// RETURNING old.size yields exactly one row when the file existed and zero
	// rows when it did not — the same not-found signal the previous
	// RowsAffected()==0 check relied on.
	updateQuery := `
		WITH old AS (
			SELECT id, share_name, size, uid, gid, file_type
			FROM inodes
			WHERE id = $21 AND share_name = $22
			FOR UPDATE
		)
		UPDATE inodes SET
			file_type = $1,
			mode = $2,
			uid = $3,
			gid = $4,
			size = $5,
			atime = $6,
			mtime = $7,
			ctime = $8,
			creation_time = $9,
			content_id = $10,
			link_target = $11,
			device_major = $12,
			device_minor = $13,
			hidden = $14,
			acl = $15,
			eas = $16,
			object_id = $17,
			deleted_at = $18,
			original_path = $19,
			deleted_by = $20
		FROM old
		WHERE inodes.id = old.id AND inodes.share_name = old.share_name
		RETURNING old.size, old.uid, old.gid, old.file_type
	`

	var deviceMajor, deviceMinor *int32
	if file.Type == metadata.FileTypeBlockDevice || file.Type == metadata.FileTypeCharDevice {
		major := int32(metadata.RdevMajor(file.Rdev))
		minor := int32(metadata.RdevMinor(file.Rdev))
		deviceMajor = &major
		deviceMinor = &minor
	}

	var payloadIDPtr *string
	if file.PayloadID != "" {
		str := string(file.PayloadID)
		payloadIDPtr = &str
	}

	var linkTargetPtr *string
	if file.LinkTarget != "" {
		linkTargetPtr = &file.LinkTarget
	}

	// Marshal ACL to JSON for JSONB storage
	var aclJSON []byte
	if file.ACL != nil {
		var marshalErr error
		aclJSON, marshalErr = json.Marshal(file.ACL)
		if marshalErr != nil {
			return mapPgError(marshalErr, "UpdateAttrs", "marshal ACL")
		}
	}

	// Marshal extended attributes to JSON for JSONB storage. Empty/nil EAs
	// write SQL NULL so a file that never had EAs stores nothing.
	var easJSON []byte
	if len(file.EAs) > 0 {
		var marshalErr error
		easJSON, marshalErr = json.Marshal(file.EAs)
		if marshalErr != nil {
			return mapPgError(marshalErr, "UpdateAttrs", "marshal EAs")
		}
	}

	// object_id BYTEA argument.
	// Zero-valued ObjectID writes SQL NULL so the partial unique index
	// (files_object_id_idx WHERE object_id IS NOT NULL) skips the row —
	// legacy / never-quiesced / partially-flushed files never collide on
	// the all-zero sentinel.
	var objectIDArg interface{}
	if !file.ObjectID.IsZero() {
		objectIDArg = file.ObjectID[:]
	}

	// deleted_at is a BIGINT Windows-FILETIME column (#190), nullable: NULL marks
	// a live node, a value records the recycle instant losslessly (same encoding
	// as the other file timestamps — must use sqlcodec.TimeToFiletime, not UnixNano, so it
	// decodes back correctly via sqlcodec.FiletimeToTime). Pass *int64 so a nil DeletedAt
	// writes SQL NULL.
	var deletedAtArg *int64
	if file.DeletedAt != nil {
		n := sqlcodec.TimeToFiletime(*file.DeletedAt)
		deletedAtArg = &n
	}

	// Try UPDATE first (most common case for existing files). The CTE locks the
	// row and RETURNING old.size hands back the pre-update size in the same
	// round-trip, so there is no separate SELECT and no window between read and
	// write. A returned row means the file existed (and we have its old size);
	// pgx.ErrNoRows means it did not, so we fall through to INSERT.
	var oldSizeVal sql.NullInt64
	var oldUIDVal, oldGIDVal, oldTypeVal sql.NullInt64
	// A caller that knows the inode is new skips the probe entirely: on a
	// create the round-trip can only ever report "no such row", and a stale
	// claim surfaces as the INSERT's duplicate-key error.
	updated := !file.NewInode
	if updated {
		scanErr := tx.tx.QueryRow(ctx, updateQuery,
			file.Type, file.Mode, file.UID, file.GID, file.Size,
			sqlcodec.TimeToFiletime(file.Atime), sqlcodec.TimeToFiletime(file.Mtime),
			sqlcodec.TimeToFiletime(file.Ctime), sqlcodec.TimeToFiletime(file.CreationTime),
			payloadIDPtr, linkTargetPtr, deviceMajor, deviceMinor,
			file.Hidden, aclJSON, easJSON, objectIDArg,
			deletedAtArg, file.OriginalPath, file.DeletedBy,
			file.ID, file.ShareName,
		).Scan(&oldSizeVal, &oldUIDVal, &oldGIDVal, &oldTypeVal)
		switch {
		case scanErr == nil:
			// Row existed and was updated; oldSizeVal holds the pre-update size.
		case errors.Is(scanErr, pgx.ErrNoRows):
			updated = false
		default:
			return mapPgError(scanErr, "UpdateAttrs", "")
		}
	}

	// Track size delta for regular files after a successful update.
	// Accumulated on the tx and applied once after a successful commit so a
	// serialization/deadlock retry never double-counts.
	if updated && file.Type == metadata.FileTypeRegular {
		var oldSize uint64
		if oldSizeVal.Valid {
			oldSize = uint64(oldSizeVal.Int64)
		}

		// Per-identity usage. The previous row may not have been a regular file
		// (type change), in which case it contributed nothing before.
		oldWasRegular := oldTypeVal.Valid && metadata.FileType(oldTypeVal.Int64) == metadata.FileTypeRegular
		oldUID := uint32(oldUIDVal.Int64)
		oldGID := uint32(oldGIDVal.Int64)
		switch {
		case !oldWasRegular:
			tx.quota.Add(file.ShareName, file.UID, file.GID, int64(file.Size), 1)
		case oldUID == file.UID && oldGID == file.GID:
			tx.quota.Add(file.ShareName, file.UID, file.GID, int64(file.Size)-int64(oldSize), 0)
		default:
			// Chown: move bytes + inode from old owner to new owner.
			tx.quota.Add(file.ShareName, oldUID, oldGID, -int64(oldSize), -1)
			tx.quota.Add(file.ShareName, file.UID, file.GID, int64(file.Size), 1)
		}
	}

	// If no rows were updated, the file doesn't exist - do an INSERT
	if !updated {
		insertQuery := `
			INSERT INTO inodes (
				id, share_name, file_type, mode, uid, gid, size,
				atime, mtime, ctime, creation_time, content_id, link_target,
				device_major, device_minor, hidden, acl, eas, object_id,
				deleted_at, original_path, deleted_by
			) VALUES (
				$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18,
				$19, $20, $21, $22
			)
		`

		if _, err := tx.tx.Exec(ctx, insertQuery,
			file.ID, file.ShareName,
			file.Type, file.Mode, file.UID, file.GID, file.Size,
			sqlcodec.TimeToFiletime(file.Atime), sqlcodec.TimeToFiletime(file.Mtime),
			sqlcodec.TimeToFiletime(file.Ctime), sqlcodec.TimeToFiletime(file.CreationTime),
			payloadIDPtr, linkTargetPtr, deviceMajor, deviceMinor,
			file.Hidden, aclJSON, easJSON, objectIDArg,
			deletedAtArg, file.OriginalPath, file.DeletedBy,
		); err != nil {
			return mapPgError(err, "UpdateAttrs", "")
		}

		// Charge the new regular file to its share and owner.
		if file.Type == metadata.FileTypeRegular {
			tx.quota.Add(file.ShareName, file.UID, file.GID, int64(file.Size), 1)
		}

		// Debug logging for new file inserts, gated so the id formatting and
		// variadic boxing are skipped when Debug is off.
		if tx.store.logger.Enabled(ctx, slog.LevelDebug) {
			tx.store.logger.Debug("UpdateAttrs inserted",
				"id", file.ID.String(),
				"share", file.ShareName,
				"path", file.Path,
				"file_type", int(file.Type),
				"size", file.Size)
		}
	}

	// persist FileAttr.Blocks into file_block_refs — but ONLY on the
	// SetManifest path. Attr-only writes (chmod/utimes/close/rename/xattr/…)
	// come through UpdateAttrs and skip the DELETE+INSERT entirely instead of
	// rewriting the whole chunk list on every write. Only regular files carry
	// ChunkRef payloads; empty/nil Blocks on a SetManifest performs a
	// DELETE-only pass so no stale rows survive a drop.
	if file.Type == metadata.FileTypeRegular && writeManifest {
		// Apply only the rows that actually changed. An in-place overwrite that
		// reuses the same chunk boundaries frequently projects an identical
		// manifest, so putFileChunkRefs writes nothing and reports wrote=false.
		// Freshly-inserted rows (!updated) have no prior refs, so every ref is
		// a plain insert. The counter tracks manifests that truly changed.
		wrote, scanned, err := putFileChunkRefs(ctx, tx.tx, file.ID, file.Blocks, updated, file.ManifestDirtyOffsets)
		tx.store.manifestRowsScanned.Add(int64(scanned))
		if err != nil {
			return mapPgError(err, "SetManifest", "blocks")
		}
		if wrote {
			tx.store.manifestWrites.Add(1)
		}
	}

	return nil
}

func (tx *postgresTransaction) DeleteFile(ctx context.Context, handle metadata.FileHandle) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	shareName, id, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid file handle",
		}
	}

	// Read size + owner before deletion for counter tracking.
	var fileType int
	var fileSize int64
	var fileUID, fileGID int64
	_ = tx.tx.QueryRow(ctx,
		`SELECT file_type, size, uid, gid FROM inodes WHERE id = $1 AND share_name = $2`,
		id, shareName,
	).Scan(&fileType, &fileSize, &fileUID, &fileGID)

	// Delete the file. parent_child_map.parent_id declares ON DELETE CASCADE
	// against inodes(id), so deleting the inode row reaps (if it is a directory)
	// its child-map rows automatically. The hard-link count lives on the inode
	// row itself (inodes.nlink), so it is removed with the row. We do NOT delete
	// this file from its parent's children
	// map here — that is the responsibility of DeleteChild, which the service
	// layer calls separately. This matches the memory and badger stores.
	result, err := tx.tx.Exec(ctx, `DELETE FROM inodes WHERE id = $1 AND share_name = $2`, id, shareName)
	if err != nil {
		return mapPgError(err, "DeleteFile", "")
	}

	if result.RowsAffected() == 0 {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "file not found",
		}
	}

	// Subtract size from the pending delta + per-identity usage for regular files.
	if metadata.FileType(fileType) == metadata.FileTypeRegular {
		tx.quota.Add(shareName, uint32(fileUID), uint32(fileGID), -fileSize, -1)
	}

	return nil
}

// ============================================================================
// Transaction Shares Operations
// ============================================================================
//
// UpdateShareOptions and DeleteShare shadow their promoted Core namesakes for
// one reason: sharesDirty. It tells the commit path to drop the store's share
// cache, it lives on the transaction, and Core has no way to reach it — so the
// flag is set here and the statement runs there. Delete a shadow and the build
// still passes, since the promoted method satisfies the interface, but the
// cache then serves the options the transaction just overwrote.
//
// CreateShare keeps its own body: it runs the same UPDATE, but the memory and
// badger transactions reject a name that already exists where these two
// silently overwrite it, and collapsing the SQL pair onto Core would fix that
// divergence in place rather than decide it.

func (tx *postgresTransaction) CreateShare(ctx context.Context, share *metadata.Share) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tx.sharesDirty = true

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
	tx.sharesDirty = true
	return tx.Core.UpdateShareOptions(ctx, shareName, options)
}

func (tx *postgresTransaction) DeleteShare(ctx context.Context, shareName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tx.sharesDirty = true

	// Capture per-identity usage about to be removed (uid + gid) so the
	// in-memory usage cache stays accurate. Aggregated before the rows are
	// deleted; applied to the tx quota delta (post-commit) below.
	if err := tx.collectShareQuotaFreed(ctx, shareName, "uid", metadata.QuotaScopeUser); err != nil {
		return err
	}
	if err := tx.collectShareQuotaFreed(ctx, shareName, "gid", metadata.QuotaScopeGroup); err != nil {
		return err
	}

	// Drop the share row first: shares.root_file_id references inodes(id)
	// WITHOUT ON DELETE CASCADE, so the inode rows cannot be removed while
	// the share still points at the root inode.
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

	// Delete all inode rows for the share. The store.go:161 contract is
	// "removes a share and all its metadata"; dropping only the share row
	// orphans every inodes/parent_child_map/file_block_refs row.
	// parent_child_map and file_block_refs both cascade from inodes(id).
	if _, err := tx.tx.Exec(ctx, `DELETE FROM inodes WHERE share_name = $1`, shareName); err != nil {
		return mapPgError(err, "DeleteShare", shareName)
	}

	return nil
}

// collectShareQuotaFreed aggregates per-identity usage for the regular files of
// a share being deleted (grouped by uid or gid) and records the negative delta
// on the tx so the in-memory usage cache is decremented post-commit. The column
// is a fixed internal constant, never user input.
func (tx *postgresTransaction) collectShareQuotaFreed(ctx context.Context, shareName, col string, scope metadata.QuotaScope) error {
	query := fmt.Sprintf(
		`SELECT %s, COALESCE(SUM(size), 0), COUNT(*) FROM inodes
		 WHERE share_name = $1 AND file_type = $2 GROUP BY %s`,
		col, col,
	)
	rows, err := tx.tx.Query(ctx, query, shareName, int(metadata.FileTypeRegular))
	if err != nil {
		return mapPgError(err, "DeleteShare", shareName)
	}
	defer rows.Close()
	for rows.Next() {
		var id, bytes, files int64
		if err := rows.Scan(&id, &bytes, &files); err != nil {
			return mapPgError(err, "DeleteShare", shareName)
		}
		tx.quota.AddKeyed(basestore.QuotaKey{Share: shareName, Scope: scope, ID: uint32(id)}, metadata.UsageStat{Bytes: -bytes, Files: -files})
	}
	if err := rows.Err(); err != nil {
		return mapPgError(err, "DeleteShare", shareName)
	}
	return nil
}

func (tx *postgresTransaction) CreateRootDirectory(ctx context.Context, shareName string, attr *metadata.FileAttr) (*metadata.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tx.sharesDirty = true

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

	// Check if root directory already exists (idempotent behavior). The root is
	// resolved via shares.root_file_id — with the path column gone (#1166), the
	// share row is the authoritative pointer to its root inode.
	checkQuery := `
		SELECT f.id, f.file_type, f.mode, f.uid, f.gid, f.size,
			   f.atime, f.mtime, f.ctime, f.creation_time, f.hidden, f.nlink
		FROM inodes f
		WHERE f.id = (SELECT root_file_id FROM shares WHERE share_name = $1)
	`

	var (
		id           string
		fileType     int16
		existingMode int32
		existingUID  int32
		existingGID  int32
		size         int64
		atime        int64
		mtime        int64
		ctime        int64
		creationTime int64
		hidden       bool
		nlink        int32
	)

	err := tx.tx.QueryRow(ctx, checkQuery, shareName).Scan(
		&id, &fileType, &existingMode, &existingUID, &existingGID, &size,
		&atime, &mtime, &ctime, &creationTime, &hidden, &nlink,
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
				Nlink:        uint32(nlink),
				UID:          uint32(existingUID),
				GID:          uint32(existingGID),
				Size:         uint64(size),
				Atime:        sqlcodec.FiletimeToTime(atime),
				Mtime:        sqlcodec.FiletimeToTime(mtime),
				Ctime:        sqlcodec.FiletimeToTime(ctime),
				CreationTime: sqlcodec.FiletimeToTime(creationTime),
				Hidden:       hidden,
			},
		}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, mapPgError(err, "CreateRootDirectory", shareName)
	}

	// Create new root directory
	rootID := uuid.New()
	now := time.Now()

	// Directories start with nlink = 2 ("." and the parent's entry). nlink is
	// the sole source of truth for the hard-link count (#1166).
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

	_, err = tx.tx.Exec(ctx, insertFileQuery,
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
			Nlink:        2, // Root directories have 2 links (. and parent's entry)
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
	if errors.Is(err, pgx.ErrNoRows) {
		// Match the pool-path and the memory/badger backends: a missing
		// config row is an empty (non-nil) config, not a not-found error.
		return metadata.MetadataServerConfig{CustomSettings: map[string]any{}}, nil
	}
	if err != nil {
		return metadata.MetadataServerConfig{}, mapPgError(err, "GetServerConfig", "")
	}

	if customSettings == nil {
		customSettings = map[string]any{}
	}
	return metadata.MetadataServerConfig{
		CustomSettings: customSettings,
	}, nil
}

func (tx *postgresTransaction) SetFilesystemCapabilities(capabilities metadata.FilesystemCapabilities) {
	tx.store.capabilities = capabilities
}

// ============================================================================
// Transaction Files Operations (additional)
// ============================================================================

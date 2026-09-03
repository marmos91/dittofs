package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/basestore"
	"github.com/marmos91/dittofs/pkg/metadata/store/internal/sqlcodec"
	"github.com/marmos91/dittofs/pkg/metadata/store/internal/txretry"

	storesql "github.com/marmos91/dittofs/pkg/metadata/store/sql"
)

// Transaction retry policy (#1769). Under write contention DittoFS must
// backpressure — block-and-retry until a real budget elapses — not surface EIO
// to the caller after a fixed handful of attempts. Only the already-classified
// transient conflicts (sqlite BUSY/LOCKED) are retried; non-transient errors
// return immediately. The deadline and jittered backoff are shared with the
// postgres backend in internal/txretry.

// ============================================================================
// Transaction Support
// ============================================================================

// sqliteTransaction wraps a SQLite transaction for the Transaction interface.
//
// Mutations accumulate their usage side effects on the transaction rather than
// applying them inline, and WithTransaction applies them exactly once after a
// successful Commit. WithTransaction retries the closure on a busy/locked condition;
// accumulating per-attempt and applying post-commit prevents double-counting
// across retries.
//
// tx is the pgx-shaped executor (QueryRow/Query/Exec with (ctx, sql, args...))
// over the underlying *sql.Tx, so the ported query bodies use it unchanged.
type sqliteTransaction struct {
	// Core runs the shared SQL bodies on THIS transaction, not the pool. A
	// body reached through the pool would run on a separate connection and
	// survive this transaction's rollback.
	*storesql.Core

	store *SQLiteMetadataStore
	tx    execer
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

// WithTransaction executes fn within a SQLite transaction.
//
// If fn returns an error, the transaction is rolled back. If fn returns nil,
// the transaction is committed. Retries automatically on a busy/locked
// condition. The accumulated usedBytes / per-identity quota deltas are applied
// exactly once after a successful commit so a retry never double-counts.
func (s *SQLiteMetadataStore) WithTransaction(ctx context.Context, fn func(tx metadata.Transaction) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// Backpressure deadline (#1769): retry transient conflicts until this budget
	// elapses rather than EIOing after a fixed attempt count.
	deadline := txretry.Deadline(ctx)

	var lastErr error
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		rawTx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			if isBusyError(err) {
				lastErr = err
				if txretry.Backoff(ctx, deadline, attempt) {
					continue
				}
				break
			}
			// A non-busy error may be the ctx's own cancellation/deadline
			// surfacing through BeginTx; surface that verbatim rather than
			// masking it as an I/O error.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return mapDBError(err, "WithTransaction", "")
		}

		ptx := &sqliteTransaction{store: s, tx: execer{e: rawTx, op: "tx"}}
		ptx.Core = &storesql.Core{X: ptx.tx, D: sqliteDialect, Caps: s.currentCapabilities, Quota: &ptx.quota}
		if err := fn(ptx); err != nil {
			_ = rawTx.Rollback()
			if isBusyError(err) {
				lastErr = err
				if txretry.Backoff(ctx, deadline, attempt) {
					continue
				}
				break
			}
			return err
		}

		if err := rawTx.Commit(); err != nil {
			if isBusyError(err) {
				lastErr = err
				if txretry.Backoff(ctx, deadline, attempt) {
					continue
				}
				break
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return mapDBError(err, "WithTransaction", "")
		}

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
	return mapDBError(lastErr, "WithTransaction", "")
}

// ============================================================================
// Transaction CRUD Operations
// ============================================================================

// UpdateAttrs persists the file's attributes and leaves the stored
// file_block_refs manifest untouched.
func (tx *sqliteTransaction) UpdateAttrs(ctx context.Context, file *metadata.File) error {
	return tx.putFile(ctx, file, false)
}

// SetManifest persists the file's attributes and rewrites the stored
// file_block_refs manifest from file.Blocks.
func (tx *sqliteTransaction) SetManifest(ctx context.Context, file *metadata.File) error {
	return tx.putFile(ctx, file, true)
}

func (tx *sqliteTransaction) putFile(ctx context.Context, file *metadata.File, writeManifest bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	values, err := storesql.InodeValues(file)
	if err != nil {
		return mapDBError(err, "UpdateAttrs", "marshal attributes")
	}

	// SQLite is a single-writer engine: the transaction holds an exclusive
	// write lock for its duration, so a SELECT and the UPDATE that follows it
	// cannot interleave with another writer. That is what lets the pre-update
	// row be read in a separate statement here, where postgres has to lock and
	// return it from the UPDATE itself. A zero-row read means the file does
	// not exist, and the write falls through to the INSERT.
	//
	// Namespace uniqueness lives in parent_child_map(parent_id, child_name);
	// the inodes row carries no path column, so a Move is a re-link there and
	// touches nothing here. content_id is still written: it keys the
	// file_blocks GetFileByPayloadID consumes.
	const selectOldQuery = `
		SELECT size, uid, gid, file_type, nlink
		FROM inodes
		WHERE id = ?1 AND share_name = ?2
	`

	const updateQuery = `
		UPDATE inodes SET
			file_type = ?1,
			mode = ?2,
			uid = ?3,
			gid = ?4,
			size = ?5,
			atime = ?6,
			mtime = ?7,
			ctime = ?8,
			creation_time = ?9,
			content_id = ?10,
			link_target = ?11,
			device_major = ?12,
			device_minor = ?13,
			hidden = ?14,
			acl = ?15,
			eas = ?16,
			object_id = ?17,
			deleted_at = ?18,
			original_path = ?19,
			deleted_by = ?20
		WHERE id = ?21 AND share_name = ?22
	`

	var old storesql.OldInode

	// A caller that knows the inode is new skips the probe entirely: on a
	// create the round-trip could only ever report "no such row", and a stale
	// claim surfaces as the INSERT's duplicate-key error.
	updated := !file.NewInode
	if updated {
		scanErr := tx.tx.QueryRow(ctx, selectOldQuery, file.ID, file.ShareName).
			Scan(&old.Size, &old.UID, &old.GID, &old.Type, &old.Nlink)
		switch {
		case scanErr == nil:
			if _, err := tx.tx.Exec(ctx, updateQuery, append(values, file.ID, file.ShareName)...); err != nil {
				return mapDBError(err, "UpdateAttrs", "")
			}
		case errors.Is(scanErr, sql.ErrNoRows):
			updated = false
		default:
			return mapDBError(scanErr, "UpdateAttrs", "")
		}
	}

	// The usage delta is accumulated on the transaction and applied once after
	// a successful commit, so a serialization or deadlock retry never
	// double-counts it.
	if updated {
		storesql.ApplyPutQuota(&tx.quota, file, old)
	}

	// If no rows were updated, the file doesn't exist - do an INSERT
	if !updated {
		const insertQuery = `
			INSERT INTO inodes (
				id, share_name, file_type, mode, uid, gid, size,
				atime, mtime, ctime, creation_time, content_id, link_target,
				device_major, device_minor, hidden, acl, eas, object_id,
				deleted_at, original_path, deleted_by
			) VALUES (
				?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14, ?15, ?16, ?17, ?18,
				?19, ?20, ?21, ?22
			)
		`

		if _, err := tx.tx.Exec(ctx, insertQuery,
			append([]any{file.ID, file.ShareName}, values...)...,
		); err != nil {
			return mapDBError(err, "UpdateAttrs", "")
		}

		// Charge the new regular file to the share owner.
		if file.Type == metadata.FileTypeRegular {
			tx.quota.Add(file.ShareName, file.UID, file.GID, int64(file.Size), 1)
		}

		// Debug logging for new file inserts, gated so the id formatting is
		// skipped when Debug is off.
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
			return mapDBError(err, "SetManifest", "blocks")
		}
		if wrote {
			tx.store.manifestWrites.Add(1)
		}
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

func (tx *sqliteTransaction) CreateShare(ctx context.Context, share *metadata.Share) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tx.sharesDirty = true

	optionsData, err := json.Marshal(share.Options)
	if err != nil {
		return err
	}

	// Update options for existing share (created by CreateRootDirectory)
	query := `UPDATE shares SET options = ?1 WHERE share_name = ?2`
	_, err = tx.tx.Exec(ctx, query, optionsData, share.Name)
	if err != nil {
		return mapDBError(err, "CreateShare", share.Name)
	}

	return nil
}

func (tx *sqliteTransaction) UpdateShareOptions(ctx context.Context, shareName string, options *metadata.ShareOptions) error {
	tx.sharesDirty = true
	return tx.Core.UpdateShareOptions(ctx, shareName, options)
}

func (tx *sqliteTransaction) DeleteShare(ctx context.Context, shareName string) error {
	tx.sharesDirty = true
	return tx.Core.DeleteShare(ctx, shareName)
}

func (tx *sqliteTransaction) CreateRootDirectory(ctx context.Context, shareName string, attr *metadata.FileAttr) (*metadata.File, error) {
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
		WHERE f.id = (SELECT root_file_id FROM shares WHERE share_name = ?1)
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
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, mapDBError(err, "CreateRootDirectory", shareName)
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
			?1, ?2,
			?3, ?4, ?5, ?6, ?7,
			?8, ?9, ?10, ?11,
			?12, ?13, ?14, ?15, 2
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
		return nil, mapDBError(err, "CreateRootDirectory", shareName)
	}

	// Insert into shares table
	insertShareQuery := `
		INSERT INTO shares (share_name, root_file_id)
		VALUES (?1, ?2)
		ON CONFLICT (share_name) DO UPDATE
		SET root_file_id = EXCLUDED.root_file_id
	`

	_, err = tx.tx.Exec(ctx, insertShareQuery, shareName, rootID)
	if err != nil {
		return nil, mapDBError(err, "CreateRootDirectory", shareName)
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

func (tx *sqliteTransaction) SetFilesystemCapabilities(capabilities metadata.FilesystemCapabilities) {
	tx.store.capabilities = capabilities
}

// ============================================================================
// Transaction Files Operations (additional)
// ============================================================================

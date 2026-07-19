package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
	"github.com/marmos91/dittofs/pkg/metadata/store/internal/quota"
	"github.com/marmos91/dittofs/pkg/metadata/store/internal/sqlcodec"
)

// Transaction retry policy (#1769). Under write contention DittoFS must
// backpressure — block-and-retry until a real budget elapses — not surface EIO
// to the caller after a fixed handful of attempts. Every competitor
// (rclone/juicefs) goes slow under the same pressure but never errors; the old
// 3-attempt / 10-20-30ms budget was routinely exceeded on hot rows (usedBytes
// counter, parent-dir mtime, quota) under 8 concurrent writers, turning
// contention into NFS3ErrIO. Only the already-classified transient conflicts
// (sqlite BUSY/LOCKED) are retried; non-transient errors return immediately.
const (
	// txRetryBudget bounds how long WithTransaction backpressures on a transient
	// conflict before giving up and returning the mapped error. Kept in line with
	// the sqlite busy_timeout (config default 5s) so a genuinely stuck conflict
	// still eventually surfaces — after a real budget, not 60ms.
	txRetryBudget = 5 * time.Second
	// txRetryBaseBackoff / txRetryMaxBackoff bound the jittered exponential
	// backoff between attempts.
	txRetryBaseBackoff = 5 * time.Millisecond
	txRetryMaxBackoff  = 200 * time.Millisecond
)

// txBackoff waits a jittered exponential backoff before the next transaction
// attempt, bounded by deadline and ctx. It returns true if the caller should
// retry, or false when the retry budget is exhausted or ctx is done.
func txBackoff(ctx context.Context, deadline time.Time, attempt int) bool {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false
	}
	// Full-jitter exponential backoff: base<<attempt capped at max, then a
	// uniform random in (0, d]. Spreads retries so contending writers don't
	// resynchronize into a thundering herd.
	d := txRetryMaxBackoff
	if attempt < 16 {
		if s := txRetryBaseBackoff << uint(attempt); s > 0 && s < txRetryMaxBackoff {
			d = s
		}
	}
	if d > remaining {
		d = remaining
	}
	wait := time.Duration(rand.Int64N(int64(d)) + 1)
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// ============================================================================
// Transaction Support
// ============================================================================

// sqliteTransaction wraps a SQLite transaction for the Transaction interface.
//
// pendingDelta accumulates the usedBytes change made by mutations inside the
// closure. It is applied to the store's atomic counter exactly once after a
// successful Commit (see WithTransaction). WithTransaction retries the closure
// on a busy/locked condition; accumulating per-attempt and applying post-commit
// prevents the counter from double-counting across retries.
//
// tx is the pgx-shaped executor (QueryRow/Query/Exec with (ctx, sql, args...))
// over the underlying *sql.Tx, so the ported query bodies use it unchanged.
type sqliteTransaction struct {
	store        *SQLiteMetadataStore
	tx           execer
	pendingDelta int64
	// quota accumulates per-identity usage changes (bytes + file count) keyed by
	// (scope, id). Applied to the store's quota cache exactly once after a
	// successful commit, identical to pendingDelta (so a serialization/deadlock
	// retry never double-counts).
	quota quota.Delta
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
	// elapses rather than EIOing after a fixed attempt count. Honor an earlier
	// caller deadline if the ctx carries one.
	deadline := time.Now().Add(txRetryBudget)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}

	var lastErr error
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		rawTx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			if isBusyError(err) {
				lastErr = err
				if txBackoff(ctx, deadline, attempt) {
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
		if err := fn(ptx); err != nil {
			_ = rawTx.Rollback()
			if isBusyError(err) {
				lastErr = err
				if txBackoff(ctx, deadline, attempt) {
					continue
				}
				break
			}
			return err
		}

		if err := rawTx.Commit(); err != nil {
			if isBusyError(err) {
				lastErr = err
				if txBackoff(ctx, deadline, attempt) {
					continue
				}
				break
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return mapDBError(err, "WithTransaction", "")
		}

		// Apply the accumulated usedBytes delta exactly once, after commit.
		if ptx.pendingDelta != 0 {
			s.usedBytes.Add(ptx.pendingDelta)
		}
		// Apply per-identity usage deltas once, after commit.
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

func (tx *sqliteTransaction) GetFile(ctx context.Context, handle metadata.FileHandle) (*metadata.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	shareName, id, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid file handle",
		}
	}

	// Fold FileAttr.Blocks into the metadata read (#1176): one round-trip
	// instead of the SELECT plus a separate getFileChunkRefs. See GetFile
	// (pool path) and blockRefsAggExpr for the equivalence rationale.
	query := `
		SELECT
			f.id, f.share_name, ` + inodePathExpr + `,
			f.file_type, f.mode, f.uid, f.gid, f.size,
			f.atime, f.mtime, f.ctime, f.creation_time,
			f.content_id, f.link_target, f.device_major, f.device_minor,
			f.hidden, f.acl, f.eas, f.object_id,
			f.deleted_at, f.original_path, f.deleted_by, f.nlink,
			` + blockRefsAggExpr + `
		FROM inodes f
		WHERE f.id = ?1 AND f.share_name = ?2
	`

	row := tx.tx.QueryRow(ctx, query, id, shareName)
	file, err := sqlcodec.FileRowToFileWithNlinkAndBlocks(row, true)
	if err != nil {
		return nil, mapDBError(err, "GetFile", "")
	}

	// Debug logging to trace file type issues
	tx.store.logger.Debug("GetFile retrieved",
		"id", id.String(),
		"share", shareName,
		"path", file.Path,
		"file_type", int(file.Type),
		"size", file.Size)

	return file, nil
}

func (tx *sqliteTransaction) PutFile(ctx context.Context, file *metadata.File) error {
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
	// SQLite is a single-writer engine: a transaction holds an exclusive write
	// lock for its duration, so a SELECT-then-UPDATE inside the same tx has no
	// interleaving window (unlike the multi-writer Postgres case the FROM/CTE
	// FOR UPDATE form guarded against). Read the pre-update size/owner/type
	// first, then UPDATE; a zero-row read means the file does not exist and we
	// fall through to INSERT — the same not-found signal as before.
	const selectOldQuery = `
		SELECT size, uid, gid, file_type
		FROM inodes
		WHERE id = ?1 AND share_name = ?2
	`
	updateQuery := `
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
			return mapDBError(marshalErr, "PutFile", "marshal ACL")
		}
	}

	// Marshal extended attributes to JSON for JSONB storage. Empty/nil EAs
	// write SQL NULL so a file that never had EAs stores nothing.
	var easJSON []byte
	if len(file.EAs) > 0 {
		var marshalErr error
		easJSON, marshalErr = json.Marshal(file.EAs)
		if marshalErr != nil {
			return mapDBError(marshalErr, "PutFile", "marshal EAs")
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

	// Read the pre-update size/owner/type, then UPDATE. Under SQLite's single
	// writer there is no interleaving between the two statements inside this
	// transaction. A missing row (sql.ErrNoRows) means the file does not exist,
	// so we fall through to INSERT.
	var oldSizeVal sql.NullInt64
	var oldUIDVal, oldGIDVal, oldTypeVal sql.NullInt64
	updated := true
	scanErr := tx.tx.QueryRow(ctx, selectOldQuery, file.ID, file.ShareName).
		Scan(&oldSizeVal, &oldUIDVal, &oldGIDVal, &oldTypeVal)
	switch {
	case scanErr == nil:
		// Row exists; update it in place.
		if _, err := tx.tx.Exec(ctx, updateQuery,
			file.Type, file.Mode, file.UID, file.GID, file.Size,
			sqlcodec.TimeToFiletime(file.Atime), sqlcodec.TimeToFiletime(file.Mtime),
			sqlcodec.TimeToFiletime(file.Ctime), sqlcodec.TimeToFiletime(file.CreationTime),
			payloadIDPtr, linkTargetPtr, deviceMajor, deviceMinor,
			file.Hidden, aclJSON, easJSON, objectIDArg,
			deletedAtArg, file.OriginalPath, file.DeletedBy,
			file.ID, file.ShareName,
		); err != nil {
			return mapDBError(err, "PutFile", "")
		}
	case errors.Is(scanErr, sql.ErrNoRows):
		updated = false
	default:
		return mapDBError(scanErr, "PutFile", "")
	}

	// Track size delta for regular files after a successful update.
	// Accumulated on the tx and applied once after a successful commit so a
	// serialization/deadlock retry never double-counts.
	if updated && file.Type == metadata.FileTypeRegular {
		var oldSize uint64
		if oldSizeVal.Valid {
			oldSize = uint64(oldSizeVal.Int64)
		}
		tx.pendingDelta += int64(file.Size) - int64(oldSize)

		// Per-identity usage. The previous row may not have been a regular file
		// (type change), in which case it contributed nothing before.
		oldWasRegular := oldTypeVal.Valid && metadata.FileType(oldTypeVal.Int64) == metadata.FileTypeRegular
		oldUID := uint32(oldUIDVal.Int64)
		oldGID := uint32(oldGIDVal.Int64)
		switch {
		case !oldWasRegular:
			tx.quota.Add(file.UID, file.GID, int64(file.Size), 1)
		case oldUID == file.UID && oldGID == file.GID:
			tx.quota.Add(file.UID, file.GID, int64(file.Size)-int64(oldSize), 0)
		default:
			// Chown: move bytes + inode from old owner to new owner.
			tx.quota.Add(oldUID, oldGID, -int64(oldSize), -1)
			tx.quota.Add(file.UID, file.GID, int64(file.Size), 1)
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
				?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14, ?15, ?16, ?17, ?18,
				?19, ?20, ?21, ?22
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
			return mapDBError(err, "PutFile", "")
		}

		// Track new regular file size.
		if file.Type == metadata.FileTypeRegular {
			if file.Size > 0 {
				tx.pendingDelta += int64(file.Size)
			}
			tx.quota.Add(file.UID, file.GID, int64(file.Size), 1)
		}

		// Debug logging for new file inserts
		tx.store.logger.Debug("PutFile inserted",
			"id", file.ID.String(),
			"share", file.ShareName,
			"path", file.Path,
			"file_type", int(file.Type),
			"size", file.Size)
	}

	// persist FileAttr.Blocks into file_block_refs — but ONLY when the caller
	// signalled the manifest legitimately changed (BlocksDirty). Attr-only
	// writes (chmod/utimes/close/rename/xattr/…) leave BlocksDirty false, so
	// they skip the DELETE+INSERT entirely instead of rewriting the whole
	// chunk list on every write (#1715 #8 write-amplification fix). Only
	// regular files carry ChunkRef payloads; empty/nil Blocks under a dirty
	// flag performs a DELETE-only pass so no stale rows survive a drop.
	if file.Type == metadata.FileTypeRegular && file.BlocksDirty {
		tx.store.manifestWrites.Add(1)
		if err := putFileChunkRefs(ctx, tx.tx, file.ID, file.Blocks); err != nil {
			return mapDBError(err, "PutFile", "blocks")
		}
	}

	return nil
}

func (tx *sqliteTransaction) DeleteFile(ctx context.Context, handle metadata.FileHandle) error {
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
		`SELECT file_type, size, uid, gid FROM inodes WHERE id = ?1 AND share_name = ?2`,
		id, shareName,
	).Scan(&fileType, &fileSize, &fileUID, &fileGID)

	// Delete the file. parent_child_map.parent_id declares ON DELETE CASCADE
	// against inodes(id), so deleting the inode row reaps (if it is a directory)
	// its child-map rows automatically. The hard-link count lives on the inode
	// row itself (inodes.nlink), so it is removed with the row. We do NOT delete
	// this file from its parent's children
	// map here — that is the responsibility of DeleteChild, which the service
	// layer calls separately. This matches the memory and badger stores.
	result, err := tx.tx.Exec(ctx, `DELETE FROM inodes WHERE id = ?1 AND share_name = ?2`, id, shareName)
	if err != nil {
		return mapDBError(err, "DeleteFile", "")
	}

	if result.RowsAffected() == 0 {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: "file not found",
		}
	}

	// Subtract size from the pending delta + per-identity usage for regular files.
	if metadata.FileType(fileType) == metadata.FileTypeRegular {
		if fileSize > 0 {
			tx.pendingDelta -= fileSize
		}
		tx.quota.Add(uint32(fileUID), uint32(fileGID), -fileSize, -1)
	}

	return nil
}

func (tx *sqliteTransaction) GetChild(ctx context.Context, dirHandle metadata.FileHandle, name string) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	shareName, parentID, err := metadata.DecodeFileHandle(dirHandle)
	if err != nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid directory handle",
		}
	}

	query := `
		SELECT dc.child_id FROM parent_child_map dc
		WHERE dc.parent_id = ?1 AND dc.child_name = ?2
	`

	var childID string
	err = tx.tx.QueryRow(ctx, query, parentID, name).Scan(&childID)
	if err != nil {
		return nil, mapDBError(err, "GetChild", name)
	}

	// Debug logging to trace child lookup
	tx.store.logger.Debug("GetChild found",
		"parent_id", parentID.String(),
		"child_name", name,
		"child_id", childID,
		"share", shareName)

	return encodeFileHandle(shareName, childID)
}

func (tx *sqliteTransaction) SetChild(ctx context.Context, dirHandle metadata.FileHandle, name string, childHandle metadata.FileHandle) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	_, parentID, err := metadata.DecodeFileHandle(dirHandle)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid directory handle",
		}
	}

	_, childID, err := metadata.DecodeFileHandle(childHandle)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid child handle",
		}
	}

	query := `
		INSERT INTO parent_child_map (parent_id, child_name, child_id)
		VALUES (?1, ?2, ?3)
		ON CONFLICT (parent_id, child_name) DO UPDATE SET child_id = EXCLUDED.child_id
	`

	_, err = tx.tx.Exec(ctx, query, parentID, name, childID)
	if err != nil {
		return mapDBError(err, "SetChild", name)
	}

	return nil
}

func (tx *sqliteTransaction) DeleteChild(ctx context.Context, dirHandle metadata.FileHandle, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	_, parentID, err := metadata.DecodeFileHandle(dirHandle)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid directory handle",
		}
	}

	_, err = tx.tx.Exec(ctx, `DELETE FROM parent_child_map WHERE parent_id = ?1 AND child_name = ?2`, parentID, name)
	if err != nil {
		return mapDBError(err, "DeleteChild", name)
	}

	// Note: We don't check RowsAffected() here because the entry may have already
	// been deleted by the CASCADE DELETE on the child_id foreign key when DeleteFile
	// deleted the file from the files table. The desired outcome (child mapping
	// no longer exists) is achieved either way.

	return nil
}

func (tx *sqliteTransaction) ListChildren(ctx context.Context, dirHandle metadata.FileHandle, cursor string, limit int) ([]metadata.DirEntry, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}

	shareName, parentID, err := metadata.DecodeFileHandle(dirHandle)
	if err != nil {
		return nil, "", &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid directory handle",
		}
	}

	if limit <= 0 {
		limit = 1000
	}

	// Refs #532 (PR #536 review): hydrate f.acl — keep parity with the
	// pool-query ListChildren above. See files.go for rationale.
	query := `
		SELECT dc.child_name, dc.child_id, f.file_type, f.mode, f.uid, f.gid, f.size,
		       f.atime, f.mtime, f.ctime, f.creation_time, f.hidden, f.acl, f.eas, f.object_id,
		       f.deleted_at, f.original_path, f.deleted_by, f.nlink
		FROM parent_child_map dc
		LEFT JOIN inodes f ON dc.child_id = f.id
		WHERE dc.parent_id = ?1 AND dc.child_name > ?2
		ORDER BY dc.child_name
		LIMIT ?3
	`

	rows, err := tx.tx.Query(ctx, query, parentID, cursor, limit+1)
	if err != nil {
		return nil, "", mapDBError(err, "ListChildren", "")
	}
	defer rows.Close()

	var entries []metadata.DirEntry
	for rows.Next() && len(entries) < limit {
		var name, childIDStr string
		var fileType int16
		var mode, uid, gid int32
		var size int64
		var atime, mtime, ctime, creationTime int64
		var hidden bool
		var aclJSON []byte
		var easJSON []byte
		var objectIDRaw []byte
		var deletedAt sql.NullInt64
		var originalPath string
		var deletedBy string
		var linkCount sql.NullInt32

		err := rows.Scan(&name, &childIDStr, &fileType, &mode, &uid, &gid, &size,
			&atime, &mtime, &ctime, &creationTime, &hidden, &aclJSON, &easJSON, &objectIDRaw,
			&deletedAt, &originalPath, &deletedBy, &linkCount)
		if err != nil {
			return nil, "", err
		}

		childHandle, err := encodeFileHandle(shareName, childIDStr)
		if err != nil {
			return nil, "", err
		}

		// Determine Nlink value
		var nlink uint32
		if linkCount.Valid {
			nlink = uint32(linkCount.Int32)
		} else {
			// Default based on file type
			if metadata.FileType(fileType) == metadata.FileTypeDirectory {
				nlink = 2
			} else {
				nlink = 1
			}
		}

		// hydrate ObjectID for directory entries so the
		// shape matches GetFile. NULL/empty -> zero (sentinel).
		attr := &metadata.FileAttr{
			Type:         metadata.FileType(fileType),
			Mode:         uint32(mode),
			Nlink:        nlink,
			UID:          uint32(uid),
			GID:          uint32(gid),
			Size:         uint64(size),
			Atime:        sqlcodec.FiletimeToTime(atime),
			Mtime:        sqlcodec.FiletimeToTime(mtime),
			Ctime:        sqlcodec.FiletimeToTime(ctime),
			CreationTime: sqlcodec.FiletimeToTime(creationTime),
			Hidden:       hidden,
		}
		if len(objectIDRaw) > 0 {
			if len(objectIDRaw) != block.HashSize {
				return nil, "", fmt.Errorf(
					"ListChildren: object_id has invalid length %d (want %d)",
					len(objectIDRaw), block.HashSize,
				)
			}
			copy(attr.ObjectID[:], objectIDRaw)
		}

		// Recycle-bin metadata (#190): carried on DirEntry.Attr so trash
		// enumeration via listing reflects recycle state without a re-read,
		// matching the pool-query path. deleted_at is BIGINT unix-nanoseconds;
		// decode via sqlcodec.FiletimeToTime.
		if deletedAt.Valid {
			t := sqlcodec.FiletimeToTime(deletedAt.Int64)
			attr.DeletedAt = &t
		}
		attr.OriginalPath = originalPath
		attr.DeletedBy = deletedBy

		// Refs #532 (PR #536 review): mirror sqlcodec.FileRowToFileWithNlink — soft
		// failure on malformed ACL JSON, same as the pool-query path.
		if len(aclJSON) > 0 {
			var fileACL acl.ACL
			if err := json.Unmarshal(aclJSON, &fileACL); err == nil {
				attr.ACL = &fileACL
			}
		}

		// Hydrate EAs for directory entries (same lenient unmarshal as ACL).
		if len(easJSON) > 0 {
			var eas map[string][]byte
			if err := json.Unmarshal(easJSON, &eas); err == nil && len(eas) > 0 {
				attr.EAs = eas
			}
		}

		entry := metadata.DirEntry{
			ID:     metadata.HandleToINode(childHandle),
			Name:   name,
			Handle: childHandle,
			Attr:   attr,
		}

		entries = append(entries, entry)
	}

	// Surface any error that terminated the iteration early (e.g. a network
	// drop mid-stream). Without this check a partial result would be returned
	// as a complete, successful listing.
	if err := rows.Err(); err != nil {
		return nil, "", mapDBError(err, "ListChildren", "")
	}

	nextCursor := ""
	if len(entries) >= limit {
		nextCursor = entries[len(entries)-1].Name
	}

	return entries, nextCursor, nil
}

func (tx *sqliteTransaction) GetParent(ctx context.Context, handle metadata.FileHandle) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	shareName, childID, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid file handle",
		}
	}

	query := `SELECT parent_id FROM parent_child_map WHERE child_id = ?1 LIMIT 1`

	var parentIDStr string
	err = tx.tx.QueryRow(ctx, query, childID).Scan(&parentIDStr)
	if err != nil {
		return nil, mapDBError(err, "GetParent", "")
	}

	return encodeFileHandle(shareName, parentIDStr)
}

func (tx *sqliteTransaction) SetParent(ctx context.Context, handle metadata.FileHandle, parentHandle metadata.FileHandle) error {
	// Parent is tracked via the parent_child_map table, already handled by SetChild.
	return nil
}

func (tx *sqliteTransaction) GetLinkCount(ctx context.Context, handle metadata.FileHandle) (uint32, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	_, fileID, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return 0, &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid file handle",
		}
	}

	var count uint32
	err = tx.tx.QueryRow(ctx, `SELECT nlink FROM inodes WHERE id = ?1`, fileID).Scan(&count)
	if err != nil {
		// Not found means count is 0
		return 0, nil
	}

	return count, nil
}

func (tx *sqliteTransaction) SetLinkCount(ctx context.Context, handle metadata.FileHandle, count uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	_, fileID, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidHandle,
			Message: "invalid file handle",
		}
	}

	// inodes.nlink is the sole source of truth for the hard-link count (#1166);
	// GETATTR reads it straight off the inode row without a join.
	_, err = tx.tx.Exec(ctx, `UPDATE inodes SET nlink = ?1 WHERE id = ?2`, count, fileID)
	if err != nil {
		return mapDBError(err, "SetLinkCount", "")
	}

	return nil
}

func (tx *sqliteTransaction) GetFilesystemMeta(ctx context.Context, shareName string) (*metadata.FilesystemMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	query := `SELECT meta FROM filesystem_meta WHERE share_name = ?1`

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

func (tx *sqliteTransaction) PutFilesystemMeta(ctx context.Context, shareName string, meta *metadata.FilesystemMeta) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}

	query := `
		INSERT INTO filesystem_meta (share_name, meta)
		VALUES (?1, ?2)
		ON CONFLICT (share_name) DO UPDATE SET meta = EXCLUDED.meta
	`

	_, err = tx.tx.Exec(ctx, query, shareName, data)
	if err != nil {
		return mapDBError(err, "PutFilesystemMeta", shareName)
	}

	return nil
}

func (tx *sqliteTransaction) GenerateHandle(ctx context.Context, shareName string, path string) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Handles are UUID-based; the path is stored in the File struct.
	return metadata.GenerateNewHandle(shareName)
}

// ============================================================================
// Transaction Shares Operations
// ============================================================================

func (tx *sqliteTransaction) GetRootHandle(ctx context.Context, shareName string) (metadata.FileHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	query := `SELECT root_file_id FROM shares WHERE share_name = ?1`

	var rootID uuid.UUID
	err := tx.tx.QueryRow(ctx, query, shareName).Scan(&rootID)
	if err != nil {
		return nil, mapDBError(err, "GetRootHandle", shareName)
	}

	return metadata.EncodeShareHandle(shareName, rootID)
}

func (tx *sqliteTransaction) GetShareOptions(ctx context.Context, shareName string) (*metadata.ShareOptions, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	query := `SELECT options FROM shares WHERE share_name = ?1`

	var optionsJSON []byte
	err := tx.tx.QueryRow(ctx, query, shareName).Scan(&optionsJSON)
	if err != nil {
		return nil, mapDBError(err, "GetShareOptions", shareName)
	}

	var options metadata.ShareOptions
	if len(optionsJSON) > 0 {
		if err := json.Unmarshal(optionsJSON, &options); err != nil {
			return nil, mapDBError(err, "GetShareOptions", shareName)
		}
	}

	return &options, nil
}

func (tx *sqliteTransaction) CreateShare(ctx context.Context, share *metadata.Share) error {
	if err := ctx.Err(); err != nil {
		return err
	}

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
	if err := ctx.Err(); err != nil {
		return err
	}

	optionsData, err := json.Marshal(options)
	if err != nil {
		return err
	}

	query := `UPDATE shares SET options = ?1 WHERE share_name = ?2`
	result, err := tx.tx.Exec(ctx, query, optionsData, shareName)
	if err != nil {
		return mapDBError(err, "UpdateShareOptions", shareName)
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

func (tx *sqliteTransaction) DeleteShare(ctx context.Context, shareName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// Sum the regular-file bytes about to be removed so the usedBytes
	// counter stays accurate without a full recompute. A failed Scan must not
	// be swallowed: silently proceeding with freedBytes=0 would delete the
	// files but never decrement the counter, drifting statfs for the process
	// lifetime.
	var freedBytes int64
	if err := tx.tx.QueryRow(ctx,
		`SELECT COALESCE(SUM(size), 0) FROM inodes
		 WHERE share_name = ?1 AND file_type = ?2 AND size > 0`,
		shareName, int(metadata.FileTypeRegular),
	).Scan(&freedBytes); err != nil {
		return mapDBError(err, "DeleteShare", shareName)
	}

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
	result, err := tx.tx.Exec(ctx, `DELETE FROM shares WHERE share_name = ?1`, shareName)
	if err != nil {
		return mapDBError(err, "DeleteShare", shareName)
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
	if _, err := tx.tx.Exec(ctx, `DELETE FROM inodes WHERE share_name = ?1`, shareName); err != nil {
		return mapDBError(err, "DeleteShare", shareName)
	}

	// Accumulate the decrement on the tx and apply it once after a successful
	// commit so a serialization/deadlock retry never double-counts. The
	// counter is statfs-only, not quota-enforcing.
	if freedBytes > 0 {
		tx.pendingDelta -= freedBytes
	}

	return nil
}

// collectShareQuotaFreed aggregates per-identity usage for the regular files of
// a share being deleted (grouped by uid or gid) and records the negative delta
// on the tx so the in-memory usage cache is decremented post-commit. The column
// is a fixed internal constant, never user input.
func (tx *sqliteTransaction) collectShareQuotaFreed(ctx context.Context, shareName, col string, scope metadata.QuotaScope) error {
	query := fmt.Sprintf(
		`SELECT %s, COALESCE(SUM(size), 0), COUNT(*) FROM inodes
		 WHERE share_name = ?1 AND file_type = ?2 GROUP BY %s`,
		col, col,
	)
	rows, err := tx.tx.Query(ctx, query, shareName, int(metadata.FileTypeRegular))
	if err != nil {
		return mapDBError(err, "DeleteShare", shareName)
	}
	defer rows.Close()
	for rows.Next() {
		var id, bytes, files int64
		if err := rows.Scan(&id, &bytes, &files); err != nil {
			return mapDBError(err, "DeleteShare", shareName)
		}
		tx.quota.AddKeyed(quota.Key{Scope: scope, ID: uint32(id)}, metadata.UsageStat{Bytes: -bytes, Files: -files})
	}
	if err := rows.Err(); err != nil {
		return mapDBError(err, "DeleteShare", shareName)
	}
	return nil
}

func (tx *sqliteTransaction) ListShares(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	rows, err := tx.tx.Query(ctx, `SELECT share_name FROM shares`)
	if err != nil {
		return nil, mapDBError(err, "ListShares", "")
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

	// Surface any error that terminated the iteration early so a partial
	// share list is not returned as if it were complete.
	if err := rows.Err(); err != nil {
		return nil, mapDBError(err, "ListShares", "")
	}

	return names, nil
}

func (tx *sqliteTransaction) CreateRootDirectory(ctx context.Context, shareName string, attr *metadata.FileAttr) (*metadata.File, error) {
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

func (tx *sqliteTransaction) SetServerConfig(ctx context.Context, config metadata.MetadataServerConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	query := `
		INSERT INTO server_config (id, config)
		VALUES (1, ?1)
		ON CONFLICT (id) DO UPDATE
		SET config = EXCLUDED.config, updated_at = CURRENT_TIMESTAMP
	`

	// config is a JSON TEXT column; marshal explicitly (SQLite, unlike Postgres
	// JSONB, does not serialize a Go map bind value).
	settings := config.CustomSettings
	if settings == nil {
		settings = map[string]any{}
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		return mapDBError(err, "SetServerConfig", "")
	}
	if _, err := tx.tx.Exec(ctx, query, raw); err != nil {
		return mapDBError(err, "SetServerConfig", "")
	}

	return nil
}

func (tx *sqliteTransaction) GetServerConfig(ctx context.Context) (metadata.MetadataServerConfig, error) {
	if err := ctx.Err(); err != nil {
		return metadata.MetadataServerConfig{}, err
	}

	query := `SELECT config FROM server_config WHERE id = 1`

	// config is a JSON TEXT column; scan raw bytes and unmarshal (parity with
	// the pool-path GetServerConfig).
	var raw []byte
	err := tx.tx.QueryRow(ctx, query).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		// Match the pool-path and the memory/badger backends: a missing
		// config row is an empty (non-nil) config, not a not-found error.
		return metadata.MetadataServerConfig{CustomSettings: map[string]any{}}, nil
	}
	if err != nil {
		return metadata.MetadataServerConfig{}, mapDBError(err, "GetServerConfig", "")
	}

	customSettings := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &customSettings); err != nil {
			return metadata.MetadataServerConfig{}, mapDBError(err, "GetServerConfig", "")
		}
	}
	return metadata.MetadataServerConfig{
		CustomSettings: customSettings,
	}, nil
}

func (tx *sqliteTransaction) GetFilesystemCapabilities(ctx context.Context, handle metadata.FileHandle) (*metadata.FilesystemCapabilities, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Return cached capabilities
	caps := tx.store.capabilities
	return &caps, nil
}

func (tx *sqliteTransaction) SetFilesystemCapabilities(capabilities metadata.FilesystemCapabilities) {
	tx.store.capabilities = capabilities
}

func (tx *sqliteTransaction) GetFilesystemStatistics(ctx context.Context, handle metadata.FileHandle) (*metadata.FilesystemStatistics, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Scope the aggregate to the share encoded in the handle (statfsQuery).
	// Without the WHERE predicate every share reports the store-wide total. An
	// invalid handle falls back to the store-wide aggregate (single-share
	// compatible).
	sql, args := statfsQuery(handle)
	var bytesUsed, filesUsed int64
	if err := tx.tx.QueryRow(ctx, sql, args...).Scan(&bytesUsed, &filesUsed); err != nil {
		return nil, mapDBError(err, "GetFilesystemStatistics", "")
	}

	return buildFilesystemStatistics(bytesUsed, filesUsed), nil
}

// ============================================================================
// Transaction Files Operations (additional)
// ============================================================================

func (tx *sqliteTransaction) GetFileByPayloadID(ctx context.Context, payloadID metadata.PayloadID) (*metadata.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if payloadID == "" {
		return nil, &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "content ID cannot be empty",
		}
	}

	query := `
		SELECT
			f.id, f.share_name, ` + inodePathExpr + `,
			f.file_type, f.mode, f.uid, f.gid, f.size,
			f.atime, f.mtime, f.ctime, f.creation_time,
			f.content_id, f.link_target, f.device_major, f.device_minor,
			f.hidden, f.acl, f.eas, f.object_id,
			f.deleted_at, f.original_path, f.deleted_by, f.nlink
		FROM inodes f
		WHERE f.content_id = ?1
		LIMIT 1
	`

	row := tx.tx.QueryRow(ctx, query, string(payloadID))
	file, err := sqlcodec.FileRowToFileWithNlink(row)
	if err != nil {
		return nil, mapDBError(err, "GetFileByPayloadID", string(payloadID))
	}

	return file, nil
}

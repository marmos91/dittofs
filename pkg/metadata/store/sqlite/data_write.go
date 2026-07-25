package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/internal/sqlcodec"
)

// ApplyDataWrite implements metadata.DataWriteApplier: the hot-path metadata
// update for a data WRITE, done as a narrow SELECT-then-UPDATE instead of the
// GetFile (aggregate block-refs read) + PutFile (20-column rewrite) pair.
//
// It grows size to max(old, new) — so an out-of-order write at a lower offset
// never shrinks the file — stamps mtime/ctime to now, and clears setuid/setgid
// when clearSUID is set. Only regular files are affected; the usedBytes and
// per-identity quota deltas are accumulated on the tx and applied once after a
// successful commit, exactly like PutFile, so a retry never double-counts.
func (tx *sqliteTransaction) ApplyDataWrite(
	ctx context.Context, handle metadata.FileHandle, newSize uint64, now time.Time, clearSUID bool,
) (uint64, error) {
	shareName, id, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return 0, &metadata.StoreError{Code: metadata.ErrInvalidHandle, Message: "invalid file handle"}
	}

	// Pre-update size/owner/type for the max() decision and the quota delta.
	// SQLite's single writer holds the write lock for the whole tx, so there is
	// no interleaving between this read and the UPDATE below.
	var oldSize int64
	var oldUID, oldGID uint32
	var oldType int
	err = tx.tx.QueryRow(ctx,
		`SELECT size, uid, gid, file_type FROM inodes WHERE id = ?1 AND share_name = ?2`,
		id, shareName).Scan(&oldSize, &oldUID, &oldGID, &oldType)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, &metadata.StoreError{Code: metadata.ErrNotFound, Message: "file not found"}
	case err != nil:
		return 0, mapDBError(err, "ApplyDataWrite", "")
	}
	if metadata.FileType(oldType) != metadata.FileTypeRegular {
		return 0, &metadata.StoreError{Code: metadata.ErrIsDirectory, Message: "not a regular file"}
	}

	finalSize := oldSize
	if int64(newSize) > finalSize {
		finalSize = int64(newSize)
	}
	var mask uint32
	if clearSUID {
		mask = 0o6000
	}
	ft := sqlcodec.TimeToFiletime(now)

	if _, err := tx.tx.Exec(ctx,
		`UPDATE inodes SET size = ?1, mtime = ?2, ctime = ?2, mode = mode & ~?3 WHERE id = ?4 AND share_name = ?5`,
		finalSize, ft, mask, id, shareName); err != nil {
		return 0, mapDBError(err, "ApplyDataWrite", "")
	}

	if delta := finalSize - oldSize; delta != 0 {
		tx.pendingDelta += delta
		tx.quota.Add(oldUID, oldGID, delta, 0)
	}
	return uint64(finalSize), nil
}

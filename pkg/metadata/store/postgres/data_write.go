package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/basestore"
	"github.com/marmos91/dittofs/pkg/metadata/store/internal/sqlcodec"
)

// ApplyDataWrite implements metadata.DataWriteApplier: the hot-path metadata
// update for a data WRITE, done as a single statement instead of GetFile
// (aggregate block-refs read) + UpdateAttrs (20-column rewrite). Postgres RETURNING
// can reference the pre-update CTE, so the read of the old size/owner and the
// in-place update collapse into one round-trip.
//
// size grows to max(old, new) — an out-of-order write at a lower offset never
// shrinks the file — mtime/ctime are stamped to now, and setuid/setgid clear
// when clearSUID is set. Only regular files are affected. The usedBytes and
// per-identity quota deltas are accumulated on the tx and applied once after a
// successful commit, exactly like UpdateAttrs.
func (tx *postgresTransaction) ApplyDataWrite(
	ctx context.Context, handle metadata.FileHandle, newSize uint64, now time.Time, clearSUID bool,
) (uint64, error) {
	shareName, id, err := metadata.DecodeFileHandle(handle)
	if err != nil {
		return 0, &metadata.StoreError{Code: metadata.ErrInvalidHandle, Message: "invalid file handle"}
	}
	var mask int32
	if clearSUID {
		mask = 0o6000
	}
	ft := sqlcodec.TimeToFiletime(now)

	// old captures the pre-update size/owner/type; upd performs the guarded
	// in-place update and returns the new size (NULL when the row exists but is
	// not a regular file, so it is left untouched); the outer SELECT joins them.
	// An empty old (missing row) yields zero rows.
	const q = `
		WITH old AS (
			SELECT size, uid, gid, file_type, nlink FROM inodes
			WHERE id = $1 AND share_name = $2
			FOR UPDATE
		),
		upd AS (
			UPDATE inodes SET
				size  = GREATEST(inodes.size, $3::bigint),
				mtime = $4::bigint,
				ctime = $4::bigint,
				mode  = inodes.mode & ~($5::int)
			WHERE inodes.id = $1 AND inodes.share_name = $2 AND inodes.file_type = $6::smallint
			RETURNING inodes.size
		)
		SELECT (SELECT size FROM upd), old.size, old.uid, old.gid, old.nlink FROM old
	`
	var newSz *int64
	var oldSize int64
	var oldUID, oldGID int32
	var oldNlink int32
	err = tx.tx.QueryRow(ctx, q, id, shareName, int64(newSize), ft, mask, int16(metadata.FileTypeRegular)).
		Scan(&newSz, &oldSize, &oldUID, &oldGID, &oldNlink)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, &metadata.StoreError{Code: metadata.ErrNotFound, Message: "file not found"}
	case err != nil:
		return 0, mapPgError(err, "ApplyDataWrite", "")
	}
	if newSz == nil {
		// Row exists but is not a regular file: the guarded UPDATE matched nothing.
		return 0, &metadata.StoreError{Code: metadata.ErrIsDirectory, Message: "not a regular file"}
	}

	// An unlinked-but-open file still accepts writes, but it gave its bytes
	// back to the share when its last name went, so its growth is not the
	// share's to account for.
	finalSize := *newSz
	if delta := finalSize - oldSize; delta != 0 && basestore.Charged(metadata.FileTypeRegular, uint32(oldNlink)) {
		tx.quota.Add(shareName, uint32(oldUID), uint32(oldGID), delta, 0)
	}
	return uint64(finalSize), nil
}

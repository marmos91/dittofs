package postgres

import (
	"database/sql"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// fileRowToFile converts a database row to a File struct
func fileRowToFile(row pgx.Row) (*metadata.File, error) {
	var (
		id           uuid.UUID
		shareName    string
		path         string
		fileType     int16
		mode         int32
		uid          int32
		gid          int32
		size         int64
		atime        time.Time
		mtime        time.Time
		ctime        time.Time
		creationTime time.Time
		contentID    sql.NullString
		linkTarget   sql.NullString
		deviceMajor  sql.NullInt32
		deviceMinor  sql.NullInt32
		hidden       bool
	)

	err := row.Scan(
		&id,
		&shareName,
		&path,
		&fileType,
		&mode,
		&uid,
		&gid,
		&size,
		&atime,
		&mtime,
		&ctime,
		&creationTime,
		&contentID,
		&linkTarget,
		&deviceMajor,
		&deviceMinor,
		&hidden,
	)
	if err != nil {
		return nil, err
	}

	file := &metadata.File{
		ID:        id,
		ShareName: shareName,
		Path:      path,
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
	}

	// Handle nullable fields
	if contentID.Valid {
		file.ContentID = metadata.ContentID(contentID.String)
	}

	if linkTarget.Valid {
		file.LinkTarget = linkTarget.String
	}

	// Device numbers stored inline in FileAttr aren't standard
	// We'll need to handle these separately if CreateSpecialFile is used
	// For now, they're stored in the DB but not populated in FileAttr
	_ = deviceMajor
	_ = deviceMinor

	return file, nil
}

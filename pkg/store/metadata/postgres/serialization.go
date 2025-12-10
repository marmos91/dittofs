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
		contentID    sql.NullString
		linkTarget   sql.NullString
		deviceMajor  sql.NullInt32
		deviceMinor  sql.NullInt32
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
		&contentID,
		&linkTarget,
		&deviceMajor,
		&deviceMinor,
	)
	if err != nil {
		return nil, err
	}

	file := &metadata.File{
		ID:        id,
		ShareName: shareName,
		Path:      path,
		FileAttr: metadata.FileAttr{
			Type:  metadata.FileType(fileType),
			Mode:  uint32(mode),
			UID:   uint32(uid),
			GID:   uint32(gid),
			Size:  uint64(size),
			Atime: atime,
			Mtime: mtime,
			Ctime: ctime,
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

// fileRowsToFiles converts multiple database rows to File structs
func fileRowsToFiles(rows pgx.Rows) ([]*metadata.File, error) {
	var files []*metadata.File

	for rows.Next() {
		file, err := fileRowToFile(rows)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return files, nil
}

// fileAttrToDBParams converts a FileAttr to database parameters
// Returns: fileType, mode, uid, gid, size, atime, mtime, ctime, contentID, linkTarget, deviceMajor, deviceMinor
func fileAttrToDBParams(attr *metadata.FileAttr) (
	fileType int16,
	mode int32,
	uid int32,
	gid int32,
	size int64,
	atime time.Time,
	mtime time.Time,
	ctime time.Time,
	contentID sql.NullString,
	linkTarget sql.NullString,
	deviceMajor sql.NullInt32,
	deviceMinor sql.NullInt32,
) {
	fileType = int16(attr.Type)
	mode = int32(attr.Mode)
	uid = int32(attr.UID)
	gid = int32(attr.GID)
	size = int64(attr.Size)
	atime = attr.Atime
	mtime = attr.Mtime
	ctime = attr.Ctime

	// Content ID (nullable)
	if attr.ContentID != "" {
		contentID = sql.NullString{String: string(attr.ContentID), Valid: true}
	}

	// Link target for symlinks (nullable)
	if attr.LinkTarget != "" {
		linkTarget = sql.NullString{String: attr.LinkTarget, Valid: true}
	}

	// Device numbers (nullable, not yet fully supported in FileAttr)
	// These would need to be stored separately if CreateSpecialFile is implemented

	return
}

// contentIDToNullString converts a ContentID to a nullable string for database storage
func contentIDToNullString(contentID metadata.ContentID) sql.NullString {
	if contentID == "" {
		return sql.NullString{Valid: false}
	}
	return sql.NullString{String: string(contentID), Valid: true}
}

// nullStringToContentID converts a nullable string from database to a ContentID
func nullStringToContentID(ns sql.NullString) metadata.ContentID {
	if !ns.Valid {
		return ""
	}
	return metadata.ContentID(ns.String)
}

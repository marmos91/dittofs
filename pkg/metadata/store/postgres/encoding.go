package postgres

import (
	"database/sql"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// File Handle Encoding/Decoding
// ============================================================================

// decodeFileHandle extracts the share name and UUID from a file handle
// This is a convenience wrapper around metadata.DecodeFileHandle
func decodeFileHandle(handle metadata.FileHandle) (shareName string, id uuid.UUID, err error) {
	return metadata.DecodeFileHandle(handle)
}

// encodeFileHandle creates a file handle from share name and UUID string
func encodeFileHandle(shareName string, idStr string) (metadata.FileHandle, error) {
	id, err := uuid.Parse(idStr)
	if err != nil {
		return nil, err
	}
	return metadata.EncodeShareHandle(shareName, id)
}

// ============================================================================
// Database Row Serialization
// ============================================================================

// fileRowToFile converts a database row to a File struct.
// Expected columns (without link count): id, share_name, path, file_type, mode, uid, gid, size,
// atime, mtime, ctime, creation_time, content_id, link_target, device_major, device_minor, hidden
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
			Nlink:        1, // Default, will be overwritten if link count is queried
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

	// Populate Rdev for device files
	if deviceMajor.Valid && deviceMinor.Valid {
		file.Rdev = metadata.MakeRdev(uint32(deviceMajor.Int32), uint32(deviceMinor.Int32))
	}

	return file, nil
}

// fileRowToFileWithNlink converts a database row to a File struct, including link count.
// Expected columns: id, share_name, path, file_type, mode, uid, gid, size,
// atime, mtime, ctime, creation_time, content_id, link_target, device_major, device_minor, hidden, link_count
func fileRowToFileWithNlink(row pgx.Row) (*metadata.File, error) {
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
		linkCount    sql.NullInt32
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
		&linkCount,
	)
	if err != nil {
		return nil, err
	}

	// Default to 1 if link count is not found
	nlink := uint32(1)
	if linkCount.Valid {
		nlink = uint32(linkCount.Int32)
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
			Nlink:        nlink,
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

	// Populate Rdev for device files
	if deviceMajor.Valid && deviceMinor.Valid {
		file.Rdev = metadata.MakeRdev(uint32(deviceMajor.Int32), uint32(deviceMinor.Int32))
	}

	return file, nil
}

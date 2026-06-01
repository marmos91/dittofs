package postgres

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
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
// Timestamp Encoding (BIGINT nanoseconds, lossless FILETIME parity)
// ============================================================================
//
// File timestamps are stored as BIGINT unix nanoseconds rather than TIMESTAMPTZ
// (microsecond) so sub-microsecond FILETIME values round-trip losslessly, on
// par with the memory/badger backends (#882). A zero time.Time maps to 0 and
// back, matching the zero-value semantics those backends use.

// timeToPGNanos converts a time.Time to the BIGINT unix-nanosecond value stored
// in the files timestamp columns. The zero time maps to 0.
func timeToPGNanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// pgNanosToTime converts a stored BIGINT unix-nanosecond value back to a UTC
// time.Time. 0 maps to the zero time.Time.
func pgNanosToTime(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

// ============================================================================
// Database Row Serialization
// ============================================================================

// fileRowToFileWithNlink converts a database row to a File struct, including link count.
// Expected columns: id, share_name, path, file_type, mode, uid, gid, size,
// atime, mtime, ctime, creation_time, content_id, link_target, device_major, device_minor, hidden, acl, object_id, deleted_at, original_path, deleted_by, link_count
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
		atime        int64
		mtime        int64
		ctime        int64
		creationTime int64
		payloadID    sql.NullString
		linkTarget   sql.NullString
		deviceMajor  sql.NullInt32
		deviceMinor  sql.NullInt32
		hidden       bool
		aclJSON      []byte
		easJSON      []byte
		objectIDRaw  []byte
		deletedAt    sql.NullInt64
		originalPath string
		deletedBy    string
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
		&payloadID,
		&linkTarget,
		&deviceMajor,
		&deviceMinor,
		&hidden,
		&aclJSON,
		&easJSON,
		&objectIDRaw,
		&deletedAt,
		&originalPath,
		&deletedBy,
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
			Atime:        pgNanosToTime(atime),
			Mtime:        pgNanosToTime(mtime),
			Ctime:        pgNanosToTime(ctime),
			CreationTime: pgNanosToTime(creationTime),
			Hidden:       hidden,
		},
	}

	// Handle nullable fields
	if payloadID.Valid {
		file.PayloadID = metadata.PayloadID(payloadID.String)
	}

	if linkTarget.Valid {
		file.LinkTarget = linkTarget.String
	}

	// Populate Rdev for device files
	if deviceMajor.Valid && deviceMinor.Valid {
		file.Rdev = metadata.MakeRdev(uint32(deviceMajor.Int32), uint32(deviceMinor.Int32))
	}

	// Unmarshal ACL from JSONB if present
	if len(aclJSON) > 0 {
		var fileACL acl.ACL
		if err := json.Unmarshal(aclJSON, &fileACL); err == nil {
			file.ACL = &fileACL
		}
	}

	// Unmarshal extended attributes from JSONB if present. A malformed row is
	// treated as "no EAs" rather than failing the whole read.
	if len(easJSON) > 0 {
		var eas map[string][]byte
		if err := json.Unmarshal(easJSON, &eas); err == nil && len(eas) > 0 {
			file.EAs = eas
		}
	}

	// object_id BYTEA -> FileAttr.ObjectID.
	// NULL or empty -> ObjectID stays zero (sentinel: never quiesced).
	if len(objectIDRaw) > 0 {
		if len(objectIDRaw) != blockstore.HashSize {
			return nil, fmt.Errorf(
				"postgres fileRowToFileWithNlink: object_id has invalid length %d (want %d)",
				len(objectIDRaw), blockstore.HashSize,
			)
		}
		copy(file.ObjectID[:], objectIDRaw)
	}

	// Recycle-bin metadata (#190). deleted_at is BIGINT unix-nanoseconds (like
	// the other file timestamps): NULL -> live node (nil pointer); a valid value
	// decodes via pgNanosToTime for lossless nanosecond round-trip.
	// original_path / deleted_by default to '' for live nodes.
	if deletedAt.Valid {
		t := pgNanosToTime(deletedAt.Int64)
		file.DeletedAt = &t
	}
	file.OriginalPath = originalPath
	file.DeletedBy = deletedBy

	return file, nil
}

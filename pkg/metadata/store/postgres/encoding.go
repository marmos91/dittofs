package postgres

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
)

// ============================================================================
// File Handle Encoding/Decoding
// ============================================================================

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
//
// The `path` column is no longer stored on the inode (#1166); callers supply it
// as a reconstructed expression (inodePathExpr) walking parent_child_map up to
// the share root. For a hard-linked inode this yields one of its paths.
func fileRowToFileWithNlink(row pgx.Row) (*metadata.File, error) {
	return fileRowToFileWithNlinkAndBlocks(row, false)
}

// fileRowToFileWithNlinkAndBlocks decodes a file row that optionally carries a
// trailing blocks column. When withBlocks is true the SELECT list MUST append
// blockRefsAggExpr as its final column; the row's FileAttr.Blocks is then
// hydrated in the same round-trip rather than via a second loadFileBlockRefs
// query (#1176). With withBlocks=false this is identical to the pre-#1176 read.
//
// The folded aggregate is ordered by "offset" ASC and decoded into the same
// []block.BlockRef shape loadFileBlockRefs produces — an empty/absent set
// (directories, symlinks, blockless regular files) yields a nil slice.
func fileRowToFileWithNlinkAndBlocks(row pgx.Row, withBlocks bool) (*metadata.File, error) {
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
		blocksJSON   []byte
	)

	dest := []any{
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
	}
	if withBlocks {
		dest = append(dest, &blocksJSON)
	}

	if err := row.Scan(dest...); err != nil {
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
		if len(objectIDRaw) != block.HashSize {
			return nil, fmt.Errorf(
				"postgres fileRowToFileWithNlink: object_id has invalid length %d (want %d)",
				len(objectIDRaw), block.HashSize,
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

	// Folded block refs (#1176): when the SELECT appended blockRefsAggExpr,
	// hydrate FileAttr.Blocks from that aggregate rather than a second query.
	if withBlocks && len(blocksJSON) > 0 {
		blocks, err := decodeBlockRefsJSON(blocksJSON)
		if err != nil {
			return nil, err
		}
		file.Blocks = blocks
	}

	return file, nil
}

// blockRefsAggExpr is a correlated scalar subquery aggregating a file's
// file_block_refs rows into a single JSON array, ordered by "offset" ASC to
// match loadFileBlockRefs. Splice it as the FINAL column of
// a SELECT whose inode row is aliased `f` (it references f.id) and decode the
// result with fileRowToFileWithNlinkAndBlocks(row, true).
//
// Each element is [offset, size, hash_hex]; hash is encode(...,'hex') so the
// BYTEA round-trips byte-for-byte. An inode with no refs yields SQL NULL, which
// decodes to a nil slice (parity with loadFileBlockRefs on an empty set).
const blockRefsAggExpr = `(
	SELECT json_agg(
		json_build_array(fbr."offset", fbr.size, encode(fbr.hash, 'hex'))
		ORDER BY fbr."offset" ASC
	)
	FROM file_block_refs fbr
	WHERE fbr.file_id = f.id
)`

// decodeBlockRefsJSON decodes the JSON array produced by blockRefsAggExpr into
// []block.BlockRef, applying the same hash-length validation as
// loadFileBlockRefs (a malformed hash is surfaced as an error, never coerced to
// a half-decoded ref).
func decodeBlockRefsJSON(raw []byte) ([]block.BlockRef, error) {
	// Element shape: [offset(int64), size(int32), hash_hex(string)].
	var rows [][3]json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("decode folded block refs: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	out := make([]block.BlockRef, 0, len(rows))
	for _, r := range rows {
		var (
			off     int64
			sz      int32
			hashHex string
		)
		if err := json.Unmarshal(r[0], &off); err != nil {
			return nil, fmt.Errorf("decode folded block ref offset: %w", err)
		}
		if err := json.Unmarshal(r[1], &sz); err != nil {
			return nil, fmt.Errorf("decode folded block ref size: %w", err)
		}
		if err := json.Unmarshal(r[2], &hashHex); err != nil {
			return nil, fmt.Errorf("decode folded block ref hash: %w", err)
		}
		rawHash, err := hex.DecodeString(hashHex)
		if err != nil {
			return nil, fmt.Errorf("decode folded block ref hash hex: %w", err)
		}
		if len(rawHash) != block.HashSize {
			return nil, fmt.Errorf(
				"folded block ref hash has unexpected length %d (want %d)",
				len(rawHash), block.HashSize,
			)
		}
		var br block.BlockRef
		copy(br.Hash[:], rawHash)
		br.Offset = uint64(off)
		br.Size = uint32(sz)
		out = append(out, br)
	}
	return out, nil
}

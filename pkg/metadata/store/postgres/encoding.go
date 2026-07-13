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
// Timestamp Encoding (BIGINT Windows FILETIME: 100ns ticks since 1601)
// ============================================================================
//
// File timestamps are stored as Windows FILETIME — signed 100-nanosecond ticks
// since 1601-01-01 UTC — in BIGINT columns rather than TIMESTAMPTZ (microsecond)
// or unix nanoseconds. This spans years 1601–~30828, versus the int64
// unix-nanosecond window's ~1678–2262, so extreme-but-valid SMB timestamps
// round-trip losslessly on par with the memory/badger backends. The unix epoch
// (1970) becomes a distinct non-zero value rather than colliding with the
// zero-time sentinel — an int64-nanosecond encoding conflates the two and
// overflows past 2262 (#1663, was #882).
//
// ponytail: 100ns is exactly SMB FILETIME granularity and matches every value
// the storetest conformance suite asserts; only sub-100ns fractional
// nanoseconds truncate. Switch to a (sec, nsec) column pair only if 1ns
// NFS-side round-trip parity is ever actually required.

// filetimeEpochOffset is the number of 100ns ticks between the FILETIME epoch
// (1601-01-01) and the unix epoch (1970-01-01).
const filetimeEpochOffset = 116444736000000000

// timeToPGFiletime converts a time.Time to the BIGINT FILETIME value stored in
// the inode timestamp columns. The zero time maps to 0 (the "unset" sentinel;
// FILETIME 0 is 1601-01-01, which no real filesystem timestamp uses).
func timeToPGFiletime(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()*10_000_000 + int64(t.Nanosecond())/100 + filetimeEpochOffset
}

// pgFiletimeToTime converts a stored BIGINT FILETIME value back to a UTC
// time.Time. 0 maps to the zero time.Time. time.Unix normalizes the (possibly
// negative) second/nanosecond split for pre-1970 values.
func pgFiletimeToTime(ft int64) time.Time {
	if ft == 0 {
		return time.Time{}
	}
	ticks := ft - filetimeEpochOffset
	return time.Unix(ticks/10_000_000, (ticks%10_000_000)*100).UTC()
}

// ============================================================================
// Database Row Serialization
// ============================================================================

// fileRowToFileWithNlink converts a database row to a File struct, including link count.
// Expected columns: id, share_name, path, file_type, mode, uid, gid, size,
// atime, mtime, ctime, creation_time, content_id, link_target, device_major, device_minor, hidden, acl, eas, object_id, deleted_at, original_path, deleted_by, nlink
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
// hydrated in the same round-trip rather than via a second loadFileChunkRefs
// query (#1176). With withBlocks=false this is identical to the pre-#1176 read.
//
// The folded aggregate is ordered by "offset" ASC and decoded into the same
// []block.ChunkRef shape loadFileChunkRefs produces — an empty/absent set
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
		nlink        int32
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
		&nlink,
	}
	if withBlocks {
		dest = append(dest, &blocksJSON)
	}

	if err := row.Scan(dest...); err != nil {
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
			Nlink:        uint32(nlink),
			Size:         uint64(size),
			Atime:        pgFiletimeToTime(atime),
			Mtime:        pgFiletimeToTime(mtime),
			Ctime:        pgFiletimeToTime(ctime),
			CreationTime: pgFiletimeToTime(creationTime),
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

	// Recycle-bin metadata (#190). deleted_at is BIGINT Windows FILETIME (like
	// the other file timestamps): NULL -> live node (nil pointer); a valid value
	// decodes via pgFiletimeToTime for lossless nanosecond round-trip.
	// original_path / deleted_by default to '' for live nodes.
	if deletedAt.Valid {
		t := pgFiletimeToTime(deletedAt.Int64)
		file.DeletedAt = &t
	}
	file.OriginalPath = originalPath
	file.DeletedBy = deletedBy

	// Folded block refs (#1176): when the SELECT appended blockRefsAggExpr,
	// hydrate FileAttr.Blocks from that aggregate rather than a second query.
	if withBlocks && len(blocksJSON) > 0 {
		blocks, err := decodeChunkRefsJSON(blocksJSON)
		if err != nil {
			return nil, err
		}
		file.Blocks = blocks
	}

	return file, nil
}

// blockRefsAggExpr is a correlated scalar subquery aggregating a file's
// file_block_refs rows into a single JSON array, ordered by "offset" ASC to
// match loadFileChunkRefs. Splice it as the FINAL column of
// a SELECT whose inode row is aliased `f` (it references f.id) and decode the
// result with fileRowToFileWithNlinkAndBlocks(row, true).
//
// Each element is [offset, size, hash_hex]; hash is encode(...,'hex') so the
// BYTEA round-trips byte-for-byte. An inode with no refs yields SQL NULL, which
// decodes to a nil slice (parity with loadFileChunkRefs on an empty set).
const blockRefsAggExpr = `(
	SELECT json_agg(
		json_build_array(fbr."offset", fbr.size, encode(fbr.hash, 'hex'))
		ORDER BY fbr."offset" ASC
	)
	FROM file_block_refs fbr
	WHERE fbr.file_id = f.id
)`

// decodeChunkRefsJSON decodes the JSON array produced by blockRefsAggExpr into
// []block.ChunkRef, applying the same hash-length validation as
// loadFileChunkRefs (a malformed hash is surfaced as an error, never coerced to
// a half-decoded ref).
func decodeChunkRefsJSON(raw []byte) ([]block.ChunkRef, error) {
	// Element shape: [offset(int64), size(int32), hash_hex(string)].
	var rows [][3]json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("decode folded block refs: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	out := make([]block.ChunkRef, 0, len(rows))
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
		var br block.ChunkRef
		copy(br.Hash[:], rawHash)
		br.Offset = uint64(off)
		br.Size = uint32(sz)
		out = append(out, br)
	}
	return out, nil
}

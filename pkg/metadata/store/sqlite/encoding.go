package sqlite

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
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
// Timestamp Encoding (INTEGER Windows FILETIME, 100ns ticks since 1601)
// ============================================================================
//
// File timestamps are stored as INTEGER Windows FILETIME — 100-nanosecond ticks
// since 1601-01-01 UTC. This is the native SMB/NTFS timestamp unit and, unlike
// unix nanoseconds, it fits the full FILETIME range in an int64 (year 1601 to
// ~30828) so extreme values round-trip losslessly (smbtorture
// smb2.timestamps.time_t_10000000000 / _15032385535 set timestamps in years
// 2286/2446, which overflow time.Time.UnixNano — undefined past ~2262).
//
// FILETIME also makes the unix epoch (1970) a *nonzero* value
// (116444736000000000), so it no longer collides with the 0 sentinel used for
// an unset/zero time.Time (fixes smbtorture smb2.timestamps.time_t_0, which
// previously read back the zero time instead of 1970). 100ns is exactly the
// precision the metadata conformance suite requires (storetest values are all
// 100ns-aligned) and matches memory/badger fidelity for FILETIME inputs.
//
// The timeToNanos/nanosToTime names are historical (the columns held unix
// nanoseconds before this change, migrated in place by the ..._filetime_ts
// migration); the stored unit is now FILETIME.

// filetimeEpochDelta is the number of 100ns ticks between the FILETIME epoch
// (1601-01-01) and the unix epoch (1970-01-01). ticksPerSecond is 100ns ticks
// per second.
const (
	filetimeEpochDelta = int64(116444736000000000)
	ticksPerSecond     = int64(10000000)
)

// timeToNanos converts a time.Time to the INTEGER Windows FILETIME value stored
// in the timestamp columns. The zero time maps to 0.
func timeToNanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()*ticksPerSecond + int64(t.Nanosecond())/100 + filetimeEpochDelta
}

// nanosToTime converts a stored INTEGER Windows FILETIME value back to a UTC
// time.Time. 0 maps to the zero time.Time. time.Unix normalizes the negative
// sub-second remainder for pre-1970 FILETIMEs.
func nanosToTime(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	ticks := n - filetimeEpochDelta
	return time.Unix(ticks/ticksPerSecond, (ticks%ticksPerSecond)*100).UTC()
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
func fileRowToFileWithNlink(r scanRow) (*metadata.File, error) {
	return fileRowToFileWithNlinkAndBlocks(r, false)
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
func fileRowToFileWithNlinkAndBlocks(r scanRow, withBlocks bool) (*metadata.File, error) {
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

	if err := r.Scan(dest...); err != nil {
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
			Atime:        nanosToTime(atime),
			Mtime:        nanosToTime(mtime),
			Ctime:        nanosToTime(ctime),
			CreationTime: nanosToTime(creationTime),
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

	// object_id BLOB -> FileAttr.ObjectID.
	// NULL or empty -> ObjectID stays zero (sentinel: never quiesced).
	if len(objectIDRaw) > 0 {
		if len(objectIDRaw) != block.HashSize {
			return nil, fmt.Errorf(
				"fileRowToFileWithNlink: object_id has invalid length %d (want %d)",
				len(objectIDRaw), block.HashSize,
			)
		}
		copy(file.ObjectID[:], objectIDRaw)
	}

	// Recycle-bin metadata (#190). deleted_at is INTEGER unix-nanoseconds (like
	// the other file timestamps): NULL -> live node (nil pointer); a valid value
	// decodes via nanosToTime for lossless nanosecond round-trip.
	// original_path / deleted_by default to '' for live nodes.
	if deletedAt.Valid {
		t := nanosToTime(deletedAt.Int64)
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
// Each element is [offset, size, hash_hex]; hash is lower(hex(...)) so the
// BLOB round-trips byte-for-byte. An inode with no refs yields a JSON empty
// array '[]', which decodes to a nil slice (parity with loadFileChunkRefs on an
// empty set).
//
// SQLite's json_group_array does not accept an inner ORDER BY, so the rows are
// ordered by "offset" ASC in a derived subquery before aggregation.
const blockRefsAggExpr = `(
	SELECT json_group_array(json_array(fbr."offset", fbr.size, lower(hex(fbr.hash))))
	FROM (
		SELECT "offset", size, hash
		FROM file_block_refs
		WHERE file_id = f.id
		ORDER BY "offset" ASC
	) AS fbr
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

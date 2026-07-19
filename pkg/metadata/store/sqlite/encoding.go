package sqlite

import (
	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/metadata"
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

// blockRefsAggExpr is a correlated scalar subquery aggregating a file's
// file_block_refs rows into a single JSON array, ordered by "offset" ASC to
// match loadFileChunkRefs. Splice it as the FINAL column of
// a SELECT whose inode row is aliased `f` (it references f.id) and decode the
// result with sqlcodec.FileRowToFileWithNlinkAndBlocks(row, true).
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

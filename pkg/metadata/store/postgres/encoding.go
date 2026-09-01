package postgres

// ============================================================================
// Shared SELECT expressions
// ============================================================================

// blockRefsAggExpr is a correlated scalar subquery aggregating a file's
// file_block_refs rows into a single JSON array, ordered by "offset" ASC to
// match loadFileChunkRefs. Splice it as the FINAL column of
// a SELECT whose inode row is aliased `f` (it references f.id) and decode the
// result with sqlcodec.FileRowToFileWithNlinkAndBlocks(row, true).
//
// Each element is [offset, size, hash_hex, start_offset]; hash is encode(...,'hex') so the
// BYTEA round-trips byte-for-byte. An inode with no refs yields SQL NULL, which
// decodes to a nil slice (parity with loadFileChunkRefs on an empty set).
const blockRefsAggExpr = `(
	SELECT json_agg(
		json_build_array(fbr."offset", fbr.size, encode(fbr.hash, 'hex'), fbr.start_offset)
		ORDER BY fbr."offset" ASC
	)
	FROM file_block_refs fbr
	WHERE fbr.file_id = f.id
)`

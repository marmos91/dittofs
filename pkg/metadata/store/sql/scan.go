package sql

import (
	"database/sql"
	"fmt"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// ScanFileChunk reads one chunk row. Both dialects persist the same column
// layout, so this is shared verbatim.
//
// A malformed hash is an error, never the zero hash: coercing it would let the
// GC mark phase read the row as a legacy pre-CAS entry and reap a still-live CAS
// object once the grace TTL lapsed. Fail closed here so the caller aborts.
func ScanFileChunk(row Row) (*metadata.FileChunk, error) {
	var (
		chunk             metadata.FileChunk
		hash              sql.NullString
		lastSyncAttemptAt sql.NullTime
	)
	if err := row.Scan(&chunk.ID, &hash, &chunk.DataSize, &chunk.StartOffset,
		&chunk.RefCount, &chunk.LastAccess, &chunk.CreatedAt, &chunk.State, &lastSyncAttemptAt); err != nil {
		return nil, err
	}
	if hash.Valid {
		h, perr := metadata.ParseContentHash(hash.String)
		if perr != nil {
			return nil, fmt.Errorf("scan file chunk %s: parse hash %q: %w",
				chunk.ID, hash.String, perr)
		}
		chunk.Hash = h
	}
	if lastSyncAttemptAt.Valid {
		chunk.LastSyncAttemptAt = lastSyncAttemptAt.Time
	}
	return &chunk, nil
}

// ScanFileChunkRows drains a chunk result set.
func ScanFileChunkRows(rows Rows) ([]*metadata.FileChunk, error) {
	var result []*metadata.FileChunk
	for rows.Next() {
		chunk, err := ScanFileChunk(rows)
		if err != nil {
			return nil, fmt.Errorf("scan file chunk: %w", err)
		}
		result = append(result, chunk)
	}
	return result, rows.Err()
}

package postgres

import (
	"github.com/marmos91/dittofs/pkg/metadata"
)

// statfsQuery builds the aggregate query (and its args) for
// GetFilesystemStatistics. When the handle decodes to a share name the
// aggregate is scoped to that share with a WHERE predicate; otherwise it falls
// back to the store-wide aggregate (single-share compatible). An undecodable
// handle is treated as the store-wide case rather than an error.
//
// The aggregate counts only regular files, matching the semantics of the
// store-wide usedBytes counter (initUsedBytesCounter) it replaces: directories,
// symlinks and other non-regular entries carry no logical bytes and must not
// inflate UsedFiles (the share root directory would otherwise be counted).
//
// Both the pool and transaction implementations share this so the scoping rule
// lives in exactly one place.
func statfsQuery(handle metadata.FileHandle) (sql string, args []any) {
	shareName, _, decodeErr := metadata.DecodeFileHandle(handle)
	if decodeErr != nil {
		shareName = ""
	}
	if shareName != "" {
		return `SELECT COALESCE(SUM(size), 0), COUNT(*) FROM inodes WHERE share_name = $1 AND file_type = $2`,
			[]any{shareName, int(metadata.FileTypeRegular)}
	}
	return `SELECT COALESCE(SUM(size), 0), COUNT(*) FROM inodes WHERE file_type = $1`,
		[]any{int(metadata.FileTypeRegular)}
}

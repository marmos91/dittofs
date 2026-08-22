package sqlite

import (
	"github.com/marmos91/dittofs/pkg/metadata"
)

// statfsQuery builds the aggregate query (and its args) behind
// GetFilesystemStatistics, scoped to one share.
//
// The aggregate counts only regular files, matching the semantics of the
// store-wide usedBytes counter it replaces: directories, symlinks and other
// non-regular entries carry no logical bytes and must not inflate UsedFiles
// (the share root directory would otherwise be counted).
//
// Both the pool and the transaction implementations share this, so the scoping
// rule lives in exactly one place.
func statfsQuery(shareName string) (sql string, args []any) {
	return `SELECT COALESCE(SUM(size), 0), COUNT(*) FROM inodes WHERE share_name = ?1 AND file_type = ?2`,
		[]any{shareName, int(metadata.FileTypeRegular)}
}

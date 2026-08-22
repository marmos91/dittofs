package sqlite

import (
	"github.com/marmos91/dittofs/pkg/metadata"
)

// statfsQuery builds the aggregate query (and its args) behind
// GetFilesystemStatistics, for the pool and the transaction path alike. The
// share predicate is not optional: without it every share reports the
// store-wide total.
//
// Only regular files are counted, matching the store-wide usedBytes counter:
// directories, symlinks and other non-regular entries carry no logical bytes
// and would otherwise inflate UsedFiles by the share root.
func statfsQuery(shareName string) (sql string, args []any) {
	return `SELECT COALESCE(SUM(size), 0), COUNT(*) FROM inodes WHERE share_name = ?1 AND file_type = ?2`,
		[]any{shareName, int(metadata.FileTypeRegular)}
}

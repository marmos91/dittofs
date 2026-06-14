package postgres

import (
	"github.com/marmos91/dittofs/pkg/metadata"
)

// Sentinel capacity values reported by GetFilesystemStatistics. The Postgres
// backend imposes no hard quota, so these are "effectively unlimited" ceilings.
const (
	statfsTotalBytes uint64 = 1 << 50 // 1 PB
	statfsTotalFiles uint64 = 1 << 32 // 4 billion files
)

// statfsQuery builds the aggregate query (and its args) for
// GetFilesystemStatistics. When the handle decodes to a share name the
// aggregate is scoped to that share with a WHERE predicate; otherwise it falls
// back to the store-wide aggregate (single-share compatible). An undecodable
// handle is treated as the store-wide case rather than an error.
//
// Both the pool and transaction implementations share this so the scoping rule
// lives in exactly one place.
func statfsQuery(handle metadata.FileHandle) (sql string, args []any) {
	shareName, _, decodeErr := decodeFileHandle(handle)
	if decodeErr != nil {
		shareName = ""
	}
	if shareName != "" {
		return `SELECT COALESCE(SUM(size), 0), COUNT(*) FROM files WHERE share_name = $1`,
			[]any{shareName}
	}
	return `SELECT COALESCE(SUM(size), 0), COUNT(*) FROM files`, nil
}

// buildFilesystemStatistics assembles a FilesystemStatistics from the scanned
// aggregate counts, clamping available space to zero if usage somehow exceeds
// the (effectively unlimited) ceilings.
func buildFilesystemStatistics(bytesUsed, filesUsed int64) *metadata.FilesystemStatistics {
	used := uint64(bytesUsed)
	usedFiles := uint64(filesUsed)

	availableBytes := uint64(0)
	if statfsTotalBytes > used {
		availableBytes = statfsTotalBytes - used
	}
	availableFiles := uint64(0)
	if statfsTotalFiles > usedFiles {
		availableFiles = statfsTotalFiles - usedFiles
	}

	return &metadata.FilesystemStatistics{
		TotalBytes:     statfsTotalBytes,
		AvailableBytes: availableBytes,
		UsedBytes:      used,
		TotalFiles:     statfsTotalFiles,
		AvailableFiles: availableFiles,
		UsedFiles:      usedFiles,
	}
}

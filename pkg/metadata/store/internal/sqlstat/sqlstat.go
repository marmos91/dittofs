// Package sqlstat holds the statfs assembly shared by the SQL metadata store
// backends. Each backend runs its own aggregate query (the placeholder syntax
// differs between drivers) and hands the scanned used-bytes / used-files counts
// here to build the reported FilesystemStatistics.
package sqlstat

import "github.com/marmos91/dittofs/pkg/metadata"

// Sentinel capacity values reported by GetFilesystemStatistics. The SQL
// backends impose no hard quota, so these are "effectively unlimited" ceilings.
const (
	TotalBytes uint64 = 1 << 50 // 1 PB
	TotalFiles uint64 = 1 << 32 // 4 billion files
)

// Build assembles a FilesystemStatistics from the scanned aggregate counts,
// clamping available space to zero if usage somehow exceeds the (effectively
// unlimited) ceilings.
func Build(bytesUsed, filesUsed int64) *metadata.FilesystemStatistics {
	used := uint64(bytesUsed)
	usedFiles := uint64(filesUsed)

	availableBytes := uint64(0)
	if TotalBytes > used {
		availableBytes = TotalBytes - used
	}
	availableFiles := uint64(0)
	if TotalFiles > usedFiles {
		availableFiles = TotalFiles - usedFiles
	}

	return &metadata.FilesystemStatistics{
		TotalBytes:     TotalBytes,
		AvailableBytes: availableBytes,
		UsedBytes:      used,
		TotalFiles:     TotalFiles,
		AvailableFiles: availableFiles,
		UsedFiles:      usedFiles,
	}
}

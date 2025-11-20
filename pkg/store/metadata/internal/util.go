package internal

// BuildContentID constructs a ContentID from share name and full path.
//
// This creates a path-based ContentID suitable for S3 storage that:
//   - Removes leading "/" from both shareName and path
//   - Results in keys like "export/docs/report.pdf"
//
// This format enables:
//   - Easy S3 bucket inspection (human-readable)
//   - Metadata reconstruction from S3 (disaster recovery)
//   - Simple migrations and backups
//
// Parameters:
//   - shareName: The share/export name (e.g., "/export" or "export")
//   - fullPath: Full path with leading "/" (e.g., "/docs/report.pdf")
//
// Returns:
//   - string: ContentID in format "shareName/path" (e.g., "export/docs/report.pdf")
//
// Edge Cases:
//   - If both shareName and path are empty after stripping "/", returns empty string
//   - Root files (e.g., "/export" + "/") return just the share name
//
// Examples:
//   - BuildContentID("/export", "/file.txt") → "export/file.txt"
//   - BuildContentID("/export", "/docs/report.pdf") → "export/docs/report.pdf"
//   - BuildContentID("export", "/docs/report.pdf") → "export/docs/report.pdf"
//   - BuildContentID("/export", "/") → "export"
//   - BuildContentID("/", "/") → "" (edge case - should not occur in normal usage)
func BuildContentID(shareName, fullPath string) string {
	// Remove leading "/" from shareName
	share := shareName
	if len(share) > 0 && share[0] == '/' {
		share = share[1:]
	}

	// Remove leading "/" from fullPath
	path := fullPath
	if len(path) > 0 && path[0] == '/' {
		path = path[1:]
	}

	// Handle edge cases
	if len(share) == 0 {
		// If share is empty, path should be the content ID
		// This is an edge case that shouldn't occur in normal usage
		return path
	}

	if len(path) == 0 {
		// Root file in share - just return share name
		return share
	}

	return share + "/" + path
}

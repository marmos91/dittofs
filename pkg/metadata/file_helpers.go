package metadata

// ============================================================================
// File Helpers
//
// Path construction utilities and device number encoding/decoding functions.
// ============================================================================

// buildPath constructs a full path from parent path and child name.
func buildPath(parentPath, childName string) string {
	if parentPath == "/" {
		return "/" + childName
	}
	return parentPath + "/" + childName
}

// buildPayloadID constructs a content ID from share name and path.
func buildPayloadID(shareName, path string) string {
	// Remove leading "/" from path and combine with share name
	if len(path) > 0 && path[0] == '/' {
		path = path[1:]
	}
	if len(shareName) > 0 && shareName[0] == '/' {
		shareName = shareName[1:]
	}
	return shareName + "/" + path
}

// ============================================================================
// Device Number Helpers
// ============================================================================

// MakeRdev encodes major and minor device numbers into a single Rdev value.
func MakeRdev(major, minor uint32) uint64 {
	return (uint64(major) << 20) | uint64(minor&0xFFFFF)
}

// RdevMajor extracts the major device number from an Rdev value.
func RdevMajor(rdev uint64) uint32 {
	return uint32(rdev >> 20)
}

// RdevMinor extracts the minor device number from an Rdev value.
func RdevMinor(rdev uint64) uint32 {
	return uint32(rdev & 0xFFFFF)
}

// GetInitialLinkCount returns the initial link count for a new file.
func GetInitialLinkCount(fileType FileType) uint32 {
	if fileType == FileTypeDirectory {
		return 2 // . and parent entry
	}
	return 1
}

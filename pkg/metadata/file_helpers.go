package metadata

import (
	"strings"

	"github.com/google/uuid"
)

// buildPath constructs a full path from parent path and child name.
func buildPath(parentPath, childName string) string {
	if parentPath == "/" {
		return "/" + childName
	}
	return parentPath + "/" + childName
}

// buildPayloadID constructs a content ID from share name and the inode UUID.
// UUID-based (not path-based) so the content_id is stable across rename/relink:
// renaming changes the path but not the inode's UUID, so GetFileByPayloadID
// keeps resolving (issue #1166). Existing path-based content_ids written before
// this change continue to work — PayloadID is stored at create time and read
// back from the inode field, never recomputed from path.
func buildPayloadID(shareName string, id uuid.UUID) string {
	return strings.TrimPrefix(shareName, "/") + "/" + id.String()
}

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

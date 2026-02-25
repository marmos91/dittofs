// Package handlers provides SMB2 command handlers and session management.
package handlers

import (
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/mfsymlink"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// clusterSize is the allocation unit size used for AllocationSize calculations.
// SMB clients expect AllocationSize to be a multiple of the filesystem cluster size.
// We use 4096 bytes (4KB) as this is the default allocation unit size on NTFS and
// most modern filesystems. This is a logical value for SMB reporting purposes only;
// it does not reflect the actual backing store's block size.
const clusterSize = 4096

// calculateAllocationSize returns the size rounded up to the nearest cluster boundary.
func calculateAllocationSize(size uint64) uint64 {
	return ((size + clusterSize - 1) / clusterSize) * clusterSize
}

// getSMBSize returns the appropriate size for SMB reporting.
// For symlinks, this returns the MFsymlink size (1067 bytes) since SMB clients
// expect symlinks to be stored as MFsymlink files.
func getSMBSize(attr *metadata.FileAttr) uint64 {
	if attr.Type == metadata.FileTypeSymlink {
		return uint64(mfsymlink.Size)
	}
	return attr.Size
}

// IsSpecialFile returns true if the file type is a Unix special file
// (FIFO, socket, block device, character device) that should be hidden from SMB.
// These file types have no meaningful representation in the SMB protocol.
func IsSpecialFile(fileType metadata.FileType) bool {
	switch fileType {
	case metadata.FileTypeFIFO, metadata.FileTypeSocket,
		metadata.FileTypeBlockDevice, metadata.FileTypeCharDevice:
		return true
	}
	return false
}

// IsHiddenFile returns true if a file should have the Hidden attribute set.
// A file is hidden if:
//   - The filename starts with a dot (Unix convention)
//   - The Hidden flag is explicitly set in metadata (Windows convention)
func IsHiddenFile(name string, attr *metadata.FileAttr) bool {
	// Unix convention: dot-prefix files are hidden
	if strings.HasPrefix(name, ".") {
		return true
	}
	// Windows convention: explicit Hidden flag
	if attr != nil && attr.Hidden {
		return true
	}
	return false
}

// FileAttrToSMBAttributes converts metadata FileAttr to SMB file attributes.
// This version does not include hidden attribute - use FileAttrToSMBAttributesWithName
// when the filename is available.
func FileAttrToSMBAttributes(attr *metadata.FileAttr) types.FileAttributes {
	return fileAttrToSMBAttributesInternal(attr, false)
}

// FileAttrToSMBAttributesWithName converts metadata FileAttr to SMB file attributes,
// including the Hidden attribute based on filename (dot-prefix) and metadata flag.
func FileAttrToSMBAttributesWithName(attr *metadata.FileAttr, name string) types.FileAttributes {
	return fileAttrToSMBAttributesInternal(attr, IsHiddenFile(name, attr))
}

// fileAttrToSMBAttributesInternal is the internal implementation for attribute conversion.
func fileAttrToSMBAttributesInternal(attr *metadata.FileAttr, hidden bool) types.FileAttributes {
	var attrs types.FileAttributes

	switch attr.Type {
	case metadata.FileTypeDirectory:
		attrs |= types.FileAttributeDirectory
	case metadata.FileTypeRegular:
		if attr.Size == 0 {
			attrs |= types.FileAttributeNormal
		}
	case metadata.FileTypeSymlink:
		attrs |= types.FileAttributeReparsePoint
	case metadata.FileTypeFIFO, metadata.FileTypeSocket,
		metadata.FileTypeBlockDevice, metadata.FileTypeCharDevice:
		// Special files appear as regular files (though they should be filtered out)
		attrs |= types.FileAttributeNormal
	}

	// Set hidden attribute
	if hidden {
		attrs |= types.FileAttributeHidden
	}

	if attrs == 0 {
		attrs = types.FileAttributeNormal
	}

	// Note: We intentionally do NOT set FileAttributeReadonly based on Unix mode.
	// macOS SMB clients interpret FileAttributeReadonly as "share is read-only"
	// and refuse to create files, even before contacting the server.
	// Unix permission enforcement happens at file operation time, not via attributes.
	// See docs/KNOWN_LIMITATIONS.md for details on this macOS compatibility behavior.

	return attrs
}

// FileAttrToSMBTimes extracts SMB time fields from FileAttr.
// Returns: CreationTime, LastAccessTime, LastWriteTime, ChangeTime
func FileAttrToSMBTimes(attr *metadata.FileAttr) (creation, access, write, change time.Time) {
	creation = attr.CreationTime
	access = attr.Atime
	write = attr.Mtime
	change = attr.Ctime // SMB ChangeTime maps to Unix ctime

	// If CreationTime is not set, use Ctime as a fallback
	if creation.IsZero() {
		creation = attr.Ctime
	}

	return
}

// FileAttrToFileBasicInfo converts FileAttr to SMB FILE_BASIC_INFORMATION.
func FileAttrToFileBasicInfo(attr *metadata.FileAttr) *FileBasicInfo {
	creation, access, write, change := FileAttrToSMBTimes(attr)

	return &FileBasicInfo{
		CreationTime:   creation,
		LastAccessTime: access,
		LastWriteTime:  write,
		ChangeTime:     change,
		FileAttributes: FileAttrToSMBAttributes(attr),
	}
}

// FileAttrToFileStandardInfo converts FileAttr to SMB FILE_STANDARD_INFORMATION.
func FileAttrToFileStandardInfo(attr *metadata.FileAttr, isDeletePending bool) *FileStandardInfo {
	// Get appropriate size (MFsymlink size for symlinks)
	size := getSMBSize(attr)
	// Allocation size is typically rounded up to cluster size (4KB typical)
	allocationSize := calculateAllocationSize(size)

	return &FileStandardInfo{
		AllocationSize: allocationSize,
		EndOfFile:      size,
		NumberOfLinks:  1, // TODO: Track actual link count when available
		DeletePending:  isDeletePending,
		Directory:      attr.Type == metadata.FileTypeDirectory,
	}
}

// FileAttrToFileNetworkOpenInfo converts FileAttr to SMB FILE_NETWORK_OPEN_INFORMATION.
func FileAttrToFileNetworkOpenInfo(attr *metadata.FileAttr) *FileNetworkOpenInfo {
	creation, access, write, change := FileAttrToSMBTimes(attr)
	// Get appropriate size (MFsymlink size for symlinks)
	size := getSMBSize(attr)
	allocationSize := calculateAllocationSize(size)

	return &FileNetworkOpenInfo{
		CreationTime:   creation,
		LastAccessTime: access,
		LastWriteTime:  write,
		ChangeTime:     change,
		AllocationSize: allocationSize,
		EndOfFile:      size,
		FileAttributes: FileAttrToSMBAttributes(attr),
	}
}

// FileAttrToDirectoryEntry converts FileAttr to a directory listing entry.
func FileAttrToDirectoryEntry(file *metadata.File, name string, fileIndex uint64) *DirectoryEntry {
	creation, access, write, change := FileAttrToSMBTimes(&file.FileAttr)
	// Get appropriate size (MFsymlink size for symlinks)
	size := getSMBSize(&file.FileAttr)
	allocationSize := calculateAllocationSize(size)

	return &DirectoryEntry{
		FileName:       name,
		FileIndex:      fileIndex,
		CreationTime:   creation,
		LastAccessTime: access,
		LastWriteTime:  write,
		ChangeTime:     change,
		EndOfFile:      size,
		AllocationSize: allocationSize,
		FileAttributes: FileAttrToSMBAttributes(&file.FileAttr),
		FileID:         fileIndex, // Use index as FileID for now
	}
}

// DirEntryToDirectoryEntry converts a metadata DirEntry to SMB DirectoryEntry.
// If the DirEntry has Attr populated, uses it; otherwise uses default values.
func DirEntryToDirectoryEntry(entry *metadata.DirEntry, fileIndex uint64) *DirectoryEntry {
	dirEntry := &DirectoryEntry{
		FileName:  entry.Name,
		FileIndex: fileIndex,
		FileID:    entry.ID,
	}

	if entry.Attr != nil {
		creation, access, write, change := FileAttrToSMBTimes(entry.Attr)
		// Get appropriate size (MFsymlink size for symlinks)
		size := getSMBSize(entry.Attr)
		allocationSize := calculateAllocationSize(size)

		dirEntry.CreationTime = creation
		dirEntry.LastAccessTime = access
		dirEntry.LastWriteTime = write
		dirEntry.ChangeTime = change
		dirEntry.EndOfFile = size
		dirEntry.AllocationSize = allocationSize
		dirEntry.FileAttributes = FileAttrToSMBAttributes(entry.Attr)
	} else {
		// Default values when Attr is not populated
		dirEntry.FileAttributes = types.FileAttributeNormal
	}

	return dirEntry
}

// SMBAttributesToFileType converts SMB file attributes to metadata FileType.
func SMBAttributesToFileType(attrs types.FileAttributes) metadata.FileType {
	if attrs&types.FileAttributeDirectory != 0 {
		return metadata.FileTypeDirectory
	}
	if attrs&types.FileAttributeReparsePoint != 0 {
		return metadata.FileTypeSymlink
	}
	return metadata.FileTypeRegular
}

// SMBTimesToSetAttrs converts SMB time fields and attributes to SetAttrs for SETATTR operations.
func SMBTimesToSetAttrs(basicInfo *FileBasicInfo) *metadata.SetAttrs {
	attrs := &metadata.SetAttrs{}

	// Only set times that are non-zero (SMB uses 0 or -1 to indicate "don't change")
	zeroTime := time.Time{}
	negOneTime := types.FiletimeToTime(0xFFFFFFFFFFFFFFFF)

	if basicInfo.CreationTime != zeroTime && basicInfo.CreationTime != negOneTime {
		t := basicInfo.CreationTime
		attrs.CreationTime = &t
	}
	if basicInfo.LastAccessTime != zeroTime && basicInfo.LastAccessTime != negOneTime {
		t := basicInfo.LastAccessTime
		attrs.Atime = &t
	}
	if basicInfo.LastWriteTime != zeroTime && basicInfo.LastWriteTime != negOneTime {
		t := basicInfo.LastWriteTime
		attrs.Mtime = &t
	}
	// Note: ChangeTime (ctime) is typically not settable by clients

	// Handle Hidden attribute from FileAttributes
	// Only set if FileAttributes is non-zero (0 means "don't change")
	if basicInfo.FileAttributes != 0 {
		hidden := basicInfo.FileAttributes&types.FileAttributeHidden != 0
		attrs.Hidden = &hidden
	}

	return attrs
}

// MetadataErrorToSMBStatus maps metadata store errors to SMB NT status codes.
func MetadataErrorToSMBStatus(err error) types.Status {
	if err == nil {
		return types.StatusSuccess
	}

	// Check for metadata store errors
	if storeErr, ok := err.(*metadata.StoreError); ok {
		switch storeErr.Code {
		case metadata.ErrNotFound:
			return types.StatusObjectNameNotFound
		case metadata.ErrAlreadyExists:
			return types.StatusObjectNameCollision
		case metadata.ErrNotDirectory:
			return types.StatusNotADirectory
		case metadata.ErrIsDirectory:
			return types.StatusFileIsADirectory
		case metadata.ErrNotEmpty:
			return types.StatusDirectoryNotEmpty
		case metadata.ErrAccessDenied:
			return types.StatusAccessDenied
		case metadata.ErrInvalidArgument:
			return types.StatusInvalidParameter
		case metadata.ErrInvalidHandle:
			return types.StatusInvalidHandle
		case metadata.ErrNotSupported:
			return types.StatusNotSupported
		case metadata.ErrIOError:
			return types.StatusUnexpectedIOError
		case metadata.ErrNoSpace:
			return types.StatusDiskFull
		default:
			return types.StatusInternalError
		}
	}

	// Generic error
	return types.StatusInternalError
}

// ContentErrorToSMBStatus maps content store errors to SMB NT status codes.
func ContentErrorToSMBStatus(err error) types.Status {
	if err == nil {
		return types.StatusSuccess
	}

	// For now, use generic I/O error mapping
	// This could be expanded to handle specific content store errors
	return types.StatusUnexpectedIOError
}

// ResolveCreateDisposition determines the action based on disposition and file existence.
// Returns the create action and any error.
func ResolveCreateDisposition(disposition types.CreateDisposition, exists bool) (types.CreateAction, error) {
	switch disposition {
	case types.FileOpen:
		// Open existing only
		if !exists {
			return 0, &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "file does not exist",
			}
		}
		return types.FileOpened, nil

	case types.FileCreate:
		// Create new only (fail if exists)
		if exists {
			return 0, &metadata.StoreError{
				Code:    metadata.ErrAlreadyExists,
				Message: "file already exists",
			}
		}
		return types.FileCreated, nil

	case types.FileOpenIf:
		// Open or create
		if exists {
			return types.FileOpened, nil
		}
		return types.FileCreated, nil

	case types.FileOverwrite:
		// Open and overwrite (fail if not exists)
		if !exists {
			return 0, &metadata.StoreError{
				Code:    metadata.ErrNotFound,
				Message: "file does not exist",
			}
		}
		return types.FileOverwritten, nil

	case types.FileOverwriteIf:
		// Overwrite or create
		if exists {
			return types.FileOverwritten, nil
		}
		return types.FileCreated, nil

	case types.FileSupersede:
		// Replace if exists, create if not
		if exists {
			return types.FileSuperseded, nil
		}
		return types.FileCreated, nil

	default:
		return 0, &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: "invalid create disposition",
		}
	}
}

// CreateOptionsToMetadataType converts SMB create options to metadata file type.
func CreateOptionsToMetadataType(options types.CreateOptions, attrs types.FileAttributes) metadata.FileType {
	if options&types.FileDirectoryFile != 0 {
		return metadata.FileTypeDirectory
	}
	if attrs&types.FileAttributeDirectory != 0 {
		return metadata.FileTypeDirectory
	}
	return metadata.FileTypeRegular
}

// SMBModeFromAttrs converts SMB file attributes to Unix mode.
// This is a simplified conversion for file creation.
func SMBModeFromAttrs(attrs types.FileAttributes, isDirectory bool) uint32 {
	var mode uint32

	if isDirectory {
		mode = 0755 // rwxr-xr-x for directories
	} else {
		mode = 0644 // rw-r--r-- for files
	}

	// If read-only attribute is set, remove write permission
	if attrs&types.FileAttributeReadonly != 0 {
		mode &= ^uint32(0222) // Remove write bits
	}

	return mode
}

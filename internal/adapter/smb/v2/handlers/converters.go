// Package handlers provides SMB2 command handlers and session management.
package handlers

import (
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/mfsymlink"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// Filesystem geometry constants for SMB reporting purposes.
// NTFS reports SectorsPerAllocationUnit=8, BytesPerSector=512, giving
// 4096 bytes per cluster (8 * 512 = 4096). These values are used in
// AllocationSize calculations and FileFsSizeInformation responses.
const (
	bytesPerSector uint32 = 512
	sectorsPerUnit uint32 = 8
	clusterSize           = uint64(sectorsPerUnit) * uint64(bytesPerSector) // 4096

	// ntfsVolumeSerialNumber is the synthetic NTFS volume serial number reported
	// consistently across FILE_ID_INFORMATION, FSCTL_GET_NTFS_VOLUME_DATA, and
	// FileFsVolumeInformation responses. WPTS tests verify these values match.
	ntfsVolumeSerialNumber uint64 = 0x12345678

	// modeDOSExplicit is a high bit in the Unix mode field indicating that DOS
	// attributes were explicitly set via SET_INFO (FileBasicInformation). When
	// set, the ARCHIVE attribute is derived from modeDOSArchive instead of being
	// implicitly added for all regular files.
	modeDOSExplicit = uint32(0x10000)

	// modeDOSArchive tracks the DOS ARCHIVE bit when DOS attributes have been
	// explicitly set. Only meaningful when modeDOSExplicit is also set.
	modeDOSArchive = uint32(0x20000)

	// modeDOSCompressed tracks whether FSCTL_SET_COMPRESSION has been applied
	// to a file or directory. When set, FILE_ATTRIBUTE_COMPRESSED (0x0800) is
	// included in the SMB file attributes returned by GETINFO queries.
	// This is stored persistently in the metadata Mode field so it survives
	// handle close/reopen cycles.
	modeDOSCompressed = uint32(0x40000)

	// filetimeFreeze is the FILETIME sentinel value -1 (0xFFFFFFFFFFFFFFFF).
	// Per MS-FSA 2.1.5.14.2: The object store MUST NOT change this attribute
	// for this or subsequent operations on this handle.
	filetimeFreeze = uint64(0xFFFFFFFFFFFFFFFF)

	// filetimeUnfreeze is the FILETIME sentinel value -2 (0xFFFFFFFFFFFFFFFE).
	// Per MS-FSA 2.1.5.14.2: Re-enable auto-update for subsequent operations.
	filetimeUnfreeze = uint64(0xFFFFFFFFFFFFFFFE)
)

// isFiletimeSentinel reports whether ft is a freeze (-1) or unfreeze (-2) sentinel.
func isFiletimeSentinel(ft uint64) bool {
	return ft == filetimeFreeze || ft == filetimeUnfreeze
}

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
	return strings.HasPrefix(name, ".") || (attr != nil && attr.Hidden)
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
		// Per MS-FSCC, ARCHIVE is set by default for regular files. However,
		// when DOS attributes have been explicitly set via SET_INFO, honour the
		// stored value instead of unconditionally adding ARCHIVE.
		if attr.Mode&modeDOSExplicit != 0 {
			if attr.Mode&modeDOSArchive != 0 {
				attrs |= types.FileAttributeArchive
			}
		} else {
			attrs |= types.FileAttributeArchive
		}
	case metadata.FileTypeSymlink:
		attrs |= types.FileAttributeReparsePoint
	case metadata.FileTypeFIFO, metadata.FileTypeSocket,
		metadata.FileTypeBlockDevice, metadata.FileTypeCharDevice:
		// Special files appear as regular files (though they should be filtered out)
	}

	// Per MS-FSCC 2.6: FILE_ATTRIBUTE_READONLY is set when the file's
	// Unix mode has no owner-write bit (mode & 0200 == 0).
	// This reflects SET_INFO operations that applied READONLY.
	if attr.Type == metadata.FileTypeRegular && (attr.Mode&0200) == 0 {
		attrs |= types.FileAttributeReadonly
	}

	// Per MS-FSCC 2.6: FILE_ATTRIBUTE_COMPRESSED is set when the file has been
	// marked compressed via FSCTL_SET_COMPRESSION. Stored in modeDOSCompressed.
	if attr.Mode&modeDOSCompressed != 0 {
		attrs |= types.FileAttributeCompressed
	}

	// Set hidden attribute
	if hidden {
		attrs |= types.FileAttributeHidden
	}

	// Per MS-FSCC 2.6, FileAttributeNormal MUST NOT be combined with any other
	// file attributes. Only set it when no other attributes are set.
	if attrs == 0 {
		attrs = types.FileAttributeNormal
	}

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

// FileAttrToFileBasicInfo converts metadata FileAttr to SMB FILE_BASIC_INFORMATION
// [MS-FSCC] 2.4.7. Populates creation, access, write, and change timestamps,
// plus the SMB file attributes bitmask. Used by QUERY_INFO to respond to
// FileBasicInformation queries from clients.
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

// FileAttrToFileStandardInfo converts metadata FileAttr to SMB FILE_STANDARD_INFORMATION
// [MS-FSCC] 2.4.41. Computes AllocationSize (cluster-aligned) and EndOfFile from the
// file size, and reports link count, delete-pending, and directory flags. For symlinks,
// the EndOfFile reflects the MFsymlink size (1067 bytes) rather than the target path length.
func FileAttrToFileStandardInfo(attr *metadata.FileAttr, isDeletePending bool) *FileStandardInfo {
	// Get appropriate size (MFsymlink size for symlinks)
	size := getSMBSize(attr)
	// Allocation size is typically rounded up to cluster size (4KB typical)
	allocationSize := calculateAllocationSize(size)

	return &FileStandardInfo{
		AllocationSize: allocationSize,
		EndOfFile:      size,
		NumberOfLinks:  max(attr.Nlink, 1), // Use actual link count, minimum 1 for safety
		DeletePending:  isDeletePending,
		Directory:      attr.Type == metadata.FileTypeDirectory,
	}
}

// FileAttrToFileNetworkOpenInfo converts metadata FileAttr to SMB FILE_NETWORK_OPEN_INFORMATION
// [MS-FSCC] 2.4.27. Combines timestamps, allocation size, end of file, and attributes
// into a single structure. This is a performance optimization for SMB2 CREATE since
// clients can retrieve all open information in a single query instead of multiple calls.
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

// FileAttrToDirectoryEntry converts metadata File to an SMB directory listing entry
// for QUERY_DIRECTORY responses [MS-SMB2] 2.2.33. Populates all fields including
// timestamps, sizes, attributes, and the file index used for enumeration continuations.
// For symlinks, sizes reflect the MFsymlink on-disk representation.
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

// DirEntryToDirectoryEntry converts a metadata DirEntry to an SMB DirectoryEntry.
// This is the preferred conversion for QUERY_DIRECTORY since DirEntry contains
// pre-resolved attributes from the metadata store. If the entry has Attr populated,
// uses it for timestamps, sizes, and attributes; otherwise falls back to defaults.
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

// SMBAttributesToFileType converts SMB file attributes to the corresponding
// metadata FileType. Checks FileAttributeDirectory and FileAttributeReparsePoint
// flags to distinguish directories, symlinks, and regular files. Used during
// SET_INFO operations to determine the target file type from client-provided attributes.
func SMBAttributesToFileType(attrs types.FileAttributes) metadata.FileType {
	if attrs&types.FileAttributeDirectory != 0 {
		return metadata.FileTypeDirectory
	}
	if attrs&types.FileAttributeReparsePoint != 0 {
		return metadata.FileTypeSymlink
	}
	return metadata.FileTypeRegular
}

// DecodeBasicInfoToSetAttrs decodes FILE_BASIC_INFORMATION from a raw buffer
// directly into SetAttrs, properly handling all FILETIME sentinel values per
// [MS-FSCC] 2.4.7 and [MS-FSA] 2.1.5.14.2:
//   - 0: don't change this timestamp
//   - 0xFFFFFFFFFFFFFFFF (-1): don't change this timestamp; disable auto-update
//   - 0xFFFFFFFFFFFFFFFE (-2): don't change this timestamp; enable auto-update
//
// All three sentinel values result in no explicit timestamp change. The -1 vs -2
// distinction controls whether the server auto-updates the timestamp on subsequent
// operations; since DittoFS does not yet track per-field auto-update state, both
// are treated identically as "don't change".
func DecodeBasicInfoToSetAttrs(buffer []byte) *metadata.SetAttrs {
	attrs := &metadata.SetAttrs{}

	r := smbenc.NewReader(buffer)
	processFiletimeForSet(r.ReadUint64(), &attrs.CreationTime)
	processFiletimeForSet(r.ReadUint64(), &attrs.Atime)
	processFiletimeForSet(r.ReadUint64(), &attrs.Mtime)
	processFiletimeForSet(r.ReadUint64(), &attrs.Ctime)

	// Handle file attributes (offset 32-36)
	if len(buffer) >= 36 {
		fileAttrs := types.FileAttributes(r.ReadUint32())
		if r.Err() == nil && fileAttrs != 0 {
			hidden := fileAttrs&types.FileAttributeHidden != 0
			attrs.Hidden = &hidden
		}
	}

	return attrs
}

// processFiletimeForSet interprets a FILETIME value for SET_INFO operations.
// Per [MS-FSA] 2.1.5.14.2:
//   - 0: don't change (server MUST NOT change this attribute)
//   - -1: don't change; disable auto-update for subsequent operations
//   - -2: don't change; re-enable auto-update for subsequent operations
//
// All three sentinel values leave the timestamp unchanged. Only explicit
// (non-sentinel, non-zero) FILETIME values cause a timestamp update.
func processFiletimeForSet(ft uint64, target **time.Time) {
	switch ft {
	case 0, filetimeFreeze, filetimeUnfreeze:
		// 0, -1, and -2: don't change this timestamp
	default:
		t := types.FiletimeToTime(ft)
		if !t.IsZero() {
			*target = &t
		}
	}
}

// Note (ADAPT-03, D-06/D-08 §2): MetadataErrorToSMBStatus and
// ContentErrorToSMBStatus were consolidated into
// internal/adapter/common/errmap.go and content_errmap.go. Handlers now call
// common.MapToSMB and common.MapContentToSMB directly — common uses
// errors.As (via the goerrors alias) so wrapped StoreErrors unwrap
// correctly, fixing a latent bug in the pre-consolidation type-assertion
// path.

// ResolveCreateDisposition determines the CREATE action based on the requested
// disposition and whether the file already exists [MS-SMB2] 2.2.13.
// Handles all six dispositions: FILE_OPEN, FILE_CREATE, FILE_OPEN_IF,
// FILE_OVERWRITE, FILE_OVERWRITE_IF, and FILE_SUPERSEDE. Returns the
// appropriate CreateAction (Opened, Created, Overwritten, Superseded) or an error.
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

// CreateOptionsToMetadataType converts SMB2 CREATE options and file attributes to the
// corresponding metadata FileType [MS-SMB2] 2.2.13. Checks FILE_DIRECTORY_FILE option
// first, then the FileAttributeDirectory attribute flag. Returns FileTypeDirectory
// for directories, FileTypeRegular for all other file types.
func CreateOptionsToMetadataType(options types.CreateOptions, attrs types.FileAttributes) metadata.FileType {
	if options&types.FileDirectoryFile != 0 {
		return metadata.FileTypeDirectory
	}
	if attrs&types.FileAttributeDirectory != 0 {
		return metadata.FileTypeDirectory
	}
	return metadata.FileTypeRegular
}

// SMBModeFromAttrs converts SMB file attributes to a Unix permission mode for file
// creation. Directories default to 0755 (rwxr-xr-x) and files to 0644 (rw-r--r--).
// If the FileAttributeReadonly flag is set, write bits are removed. This provides
// a reasonable default since SMB does not carry full Unix permission information.
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

	// Track that DOS attributes were explicitly set, and whether ARCHIVE is included.
	// This allows fileAttrToSMBAttributesInternal to return exactly the attributes
	// the client set, rather than unconditionally adding ARCHIVE for regular files.
	mode |= modeDOSExplicit
	if attrs&types.FileAttributeArchive != 0 {
		mode |= modeDOSArchive
	}

	return mode
}

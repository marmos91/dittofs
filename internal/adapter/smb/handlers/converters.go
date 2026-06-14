// Package handlers provides SMB2 command handlers and session management.
package handlers

import (
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

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

	// modeDOSSystem tracks the DOS SYSTEM bit when DOS attributes have been
	// explicitly set. Only meaningful when modeDOSExplicit is also set.
	modeDOSSystem = uint32(0x80000)

	// modeDOSCompressed tracks whether FSCTL_SET_COMPRESSION has been applied
	// to a file or directory. When set, FILE_ATTRIBUTE_COMPRESSED (0x0800) is
	// included in the SMB file attributes returned by GETINFO queries.
	// This is stored persistently in the metadata Mode field so it survives
	// handle close/reopen cycles.
	modeDOSCompressed = uint32(0x40000)

	// modeDOSReadonly tracks the DOS READONLY bit when DOS attributes have been
	// explicitly set via SET_INFO FileBasicInformation. Storing READONLY in a
	// high mode bit (rather than stripping owner-write from POSIX permissions)
	// keeps the POSIX-derived DACL stable across attribute toggles — required
	// by smbtorture smb2.winattr which captures the synthesized SD before any
	// SET_INFO and asserts the ACEs are unchanged after each attribute round-trip
	// (source4/torture/smb2/attr.c:365). Mirrors Samba's xattr-stored DOSMODE in
	// source3/smbd/dosmode.c, which decouples DOS bits from POSIX mode.
	modeDOSReadonly = uint32(0x100000)

	// modeDOSSparse tracks whether FSCTL_SET_SPARSE has been applied. When set,
	// FILE_ATTRIBUTE_SPARSE_FILE (0x0200) is included in the SMB file attributes
	// returned by GETINFO queries. Persisted in metadata Mode so it survives
	// handle close/reopen — smbtorture smb2.ioctl.sparse_file_flag asserts the
	// attribute reflects in FileBasicInformation immediately after SET_SPARSE.
	modeDOSSparse = uint32(0x200000)

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

// calculateAllocationSize returns the size rounded up to the nearest cluster
// boundary. The round-up addition saturates: a size within clusterSize-1 of the
// uint64 max (e.g. a client-supplied AllocationSize reservation [MS-SMB2]
// 2.2.13.2.2) would otherwise wrap to a small value, so such inputs clamp to the
// largest cluster-aligned uint64.
func calculateAllocationSize(size uint64) uint64 {
	const maxAligned = (^uint64(0) / clusterSize) * clusterSize
	if size > maxAligned {
		return maxAligned
	}
	return ((size + clusterSize - 1) / clusterSize) * clusterSize
}

// effectiveAllocationSize returns the AllocationSize to report for a file,
// honouring a client-requested initial allocation [MS-SMB2] 2.2.13.2.2. It is
// the larger of the file's own cluster-aligned size and the cluster-aligned
// requested reservation, so an empty file opened with a non-zero requested
// allocation reports a non-zero AllocationSize (smb2.durable-open.alloc-size).
//
// DittoFS does not preallocate backing storage; the reservation is tracked
// per-open-handle (OpenFile.RequestedAllocSize) so the CREATE response and a
// subsequent QUERY_INFO on the same handle report a consistent value
// (smb2.create.open asserts CREATE out.alloc_size == QUERY alloc_size).
// Directories ignore the request — callers pass requested=0 for them
// (smb2.create.dir-alloc-size).
func effectiveAllocationSize(size, requested uint64) uint64 {
	alloc := calculateAllocationSize(size)
	if reqAlloc := calculateAllocationSize(requested); reqAlloc > alloc {
		return reqAlloc
	}
	return alloc
}

// allocReservationFor returns the per-handle allocation reservation to record
// for a file. Directories never honour a client-requested AllocationSize
// (smb2.create.dir-alloc-size, which sets a 1 GiB request on a directory and
// asserts the reported allocation stays small), so the request is dropped for
// them; regular files keep it.
func allocReservationFor(isDirectory bool, requested uint64) uint64 {
	if isDirectory {
		return 0
	}
	return requested
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
		// Per smbtorture smb2.winattr (source4/torture/smb2/attr.c:439):
		// directories honour explicit DOS ARCHIVE round-trip when set via
		// SET_INFO. When modeDOSExplicit is unset, directories report no
		// ARCHIVE (matching MS-FSCC §2.6 — ARCHIVE on a directory is a
		// client-managed bit, not server-default).
		if attr.Mode&modeDOSExplicit != 0 && attr.Mode&modeDOSArchive != 0 {
			attrs |= types.FileAttributeArchive
		}
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

	// Per MS-FSCC §2.6: FILE_ATTRIBUTE_READONLY round-trip via SET_INFO
	// FileBasicInformation. We store the bit in modeDOSReadonly rather than
	// stripping POSIX owner-write so the mode-synthesized DACL is stable —
	// smb2.winattr (source4/torture/smb2/attr.c:365) verifies the DACL ACEs
	// don't change across READONLY toggles. The legacy fallback (owner-write
	// missing) is retained for files whose POSIX permissions were set out-of-band
	// via NFS chmod or shell chmod — those must still report READONLY to SMB
	// clients (matching Samba dosmode.c::dos_mode_from_sbuf).
	if attr.Mode&modeDOSReadonly != 0 {
		// SMB SET_INFO explicitly set READONLY (modeDOSReadonly persists
		// across ApplyModeDefault per pkg/metadata.modeMask). modeDOSExplicit
		// itself is masked off by ApplyModeDefault, so gating on it would
		// silently lose READONLY after a CREATE-defaults round-trip.
		attrs |= types.FileAttributeReadonly
	} else if attr.Mode&modeDOSExplicit == 0 && attr.Type == metadata.FileTypeRegular && (attr.Mode&0200) == 0 {
		// Legacy POSIX fallback for files whose owner-write bit was cleared
		// out-of-band (NFS chmod, shell chmod). Skipped when modeDOSExplicit
		// is set so SMB-managed attributes are not double-counted.
		attrs |= types.FileAttributeReadonly
	}

	// Per MS-FSCC 2.6: FILE_ATTRIBUTE_COMPRESSED is set when the file has been
	// marked compressed via FSCTL_SET_COMPRESSION. Stored in modeDOSCompressed.
	if attr.Mode&modeDOSCompressed != 0 {
		attrs |= types.FileAttributeCompressed
	}

	// Per MS-FSCC 2.6: FILE_ATTRIBUTE_SPARSE_FILE is set when the file has been
	// marked sparse via FSCTL_SET_SPARSE. Stored in modeDOSSparse.
	if attr.Mode&modeDOSSparse != 0 {
		attrs |= types.FileAttributeSparseFile
	}

	// Per MS-FSCC 2.6: FILE_ATTRIBUTE_SYSTEM is stored in modeDOSSystem.
	if attr.Mode&modeDOSSystem != 0 {
		attrs |= types.FileAttributeSystem
	}

	// Per MS-FSCC 2.6: FILE_ATTRIBUTE_HIDDEN is set when either the caller
	// explicitly passed hidden=true (dot-prefix detection) or when the metadata
	// Hidden flag was set via a prior SET_INFO FileBasicInformation call.
	if hidden || attr.Hidden {
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

// FileAttrToFileBasicInfoWithName converts metadata FileAttr to SMB
// FILE_BASIC_INFORMATION [MS-FSCC] 2.4.7. It applies IsHiddenFile so dot-prefixed entries
// surface FILE_ATTRIBUTE_HIDDEN even when attr.Hidden was not explicitly set.
// Required by smbtorture smb2.dosmode which creates ".dotfile" and asserts the
// HIDDEN bit via FileBasicInformation (source4/torture/smb2/dosmode.c:158).
func FileAttrToFileBasicInfoWithName(attr *metadata.FileAttr, name string) *FileBasicInfo {
	creation, access, write, change := FileAttrToSMBTimes(attr)

	return &FileBasicInfo{
		CreationTime:   creation,
		LastAccessTime: access,
		LastWriteTime:  write,
		ChangeTime:     change,
		FileAttributes: FileAttrToSMBAttributesWithName(attr, name),
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

	// NumberOfLinks reflects the open's delete-on-close disposition. Per Windows
	// / MS-FSA §2.1.5.11.6, once a handle marks the file for deletion the file is
	// on its way out and FILE_STANDARD_INFORMATION reports NumberOfLinks = 0;
	// clearing the disposition restores it to the real link count (>= 1).
	// smbtorture smb2.setinfo (setinfo.c:229) asserts nlink == 0 with
	// delete_pending == 1 and nlink == 1 with delete_pending == 0.
	numberOfLinks := max(attr.Nlink, 1) // actual link count, minimum 1 for safety
	if isDeletePending {
		numberOfLinks = 0
	}

	return &FileStandardInfo{
		AllocationSize: allocationSize,
		EndOfFile:      size,
		NumberOfLinks:  numberOfLinks,
		DeletePending:  isDeletePending,
		Directory:      attr.Type == metadata.FileTypeDirectory,
	}
}

// FileAttrToFileNetworkOpenInfoWithName converts metadata FileAttr to SMB
// FILE_NETWORK_OPEN_INFORMATION [MS-FSCC] 2.4.27. Combines timestamps, allocation
// size, end of file, and attributes into a single structure. It applies
// IsHiddenFile so dot-prefixed entries surface FILE_ATTRIBUTE_HIDDEN.
func FileAttrToFileNetworkOpenInfoWithName(attr *metadata.FileAttr, name string) *FileNetworkOpenInfo {
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
		FileAttributes: FileAttrToSMBAttributesWithName(attr, name),
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

// Note: MetadataErrorToSMBStatus and
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
//
// DOS attributes (READONLY, ARCHIVE, SYSTEM) are stored in high mode bits
// (modeDOSReadonly, modeDOSArchive, modeDOSSystem) rather than in POSIX
// permission bits. This decouples DOS attribute toggles from the
// mode-synthesized DACL — smbtorture smb2.winattr
// (source4/torture/smb2/attr.c:365) asserts that the SD ACEs do not change
// after a SET_INFO that flips READONLY. Mirrors Samba's xattr-stored
// `user.DOSATTRIB` semantics in source3/smbd/dosmode.c.
func SMBModeFromAttrs(attrs types.FileAttributes, isDirectory bool) uint32 {
	var mode uint32

	if isDirectory {
		mode = 0755 // rwxr-xr-x for directories
	} else {
		mode = 0644 // rw-r--r-- for files
	}

	// Track that DOS attributes were explicitly set, and which optional bits are set.
	// This allows fileAttrToSMBAttributesInternal to return exactly the attributes
	// the client set, rather than unconditionally adding ARCHIVE for regular files.
	mode |= modeDOSExplicit
	if attrs&types.FileAttributeArchive != 0 {
		mode |= modeDOSArchive
	}
	if attrs&types.FileAttributeSystem != 0 {
		mode |= modeDOSSystem
	}
	if attrs&types.FileAttributeReadonly != 0 {
		mode |= modeDOSReadonly
	}

	return mode
}

// ============================================================================
// UTF-16LE <-> string conversion for SMB filenames (surrogate-safe)
// ============================================================================
//
// SMB2 transmits all string data — filenames in particular — as UTF-16LE
// ([MS-SMB2] 2.1). A Windows filename is an *arbitrary* sequence of 16-bit code
// units; it is NOT required to be well-formed UTF-16. NTFS happily stores names
// containing unpaired surrogates (U+D800–U+DFFF on their own), and the
// smbtorture smb2.charset.Testing suite (source4/torture/smb2/charset.c)
// asserts exactly this: it CREATEs three names — {0xD800}, {0xDC00}, and the
// well-formed pair {0xD800,0xDC00} — and every one must succeed with
// NT_STATUS_OK. The two lone surrogates are *distinct* names and must not
// collide with each other.
//
// Go's stdlib utf16.Decode replaces every unpaired surrogate with U+FFFD, so
// {0xD800} and {0xDC00} both decode to the same "�" string and collapse
// into one name — the second CREATE then fails with a spurious collision. To
// preserve the distinction we decode into WTF-8 (the superset of UTF-8 that
// permits surrogate code points), giving each lone surrogate its own 3-byte
// encoding. encodeUTF16LESurrogateSafe is the exact inverse: it re-splits
// supplementary runes into surrogate pairs and re-emits any preserved lone
// surrogate as a single code unit, so a name round-trips byte-for-byte.
//
// (The wide-A case — fullwidth 'Ａ' U+FF21 colliding with fullwidth 'ａ'
// U+FF41 but not with ASCII 'a' — is handled by case-insensitive name matching
// in the metadata layer, whose strings.EqualFold already simple-case-folds
// U+FF21 to U+FF41. No special handling is required here.)
//
// WIRING: these are the canonical filename converters. The handlers-package
// decodeUTF16LE / encodeUTF16LE in encoding.go delegate here, so every live
// SMB string path (CREATE, QUERY_DIRECTORY, SET_INFO rename, path lookup, …)
// is surrogate-safe. The surrogate-safe codec is a strict superset of the
// previous lossy stdlib path: well-formed UTF-16 (and plain ASCII control
// strings like the share path or the "DittoFS"/"NTFS" labels) round-trips
// byte-for-byte, while two distinct lone surrogates no longer alias — which is
// what makes smb2.charset.Testing's test_surrogate pass against the running
// server.

// decodeUTF16LESurrogateSafe converts UTF-16LE bytes to a Go string without
// losing information for malformed input. Well-formed surrogate pairs combine
// into their supplementary code point; unpaired surrogates are preserved as
// distinct WTF-8 sequences rather than being collapsed to U+FFFD. An odd
// trailing byte is dropped (it cannot form a code unit).
func decodeUTF16LESurrogateSafe(b []byte) string {
	if len(b)%2 != 0 {
		b = b[:len(b)-1] // drop dangling byte; it can't form a code unit
	}

	r := smbenc.NewReader(b)
	units := make([]uint16, len(b)/2)
	for i := range units {
		units[i] = r.ReadUint16()
	}

	var sb strings.Builder
	sb.Grow(len(units) * 3)
	var buf [3]byte
	for i := 0; i < len(units); i++ {
		u := units[i]
		switch {
		case u >= 0xD800 && u <= 0xDBFF && i+1 < len(units):
			// High surrogate with a following code unit: try to pair it.
			if dec := utf16.DecodeRune(rune(u), rune(units[i+1])); dec != utf8.RuneError {
				sb.WriteString(string(dec))
				i++ // consumed the trailing low surrogate
				continue
			}
			sb.WriteString(encodeLoneSurrogate(buf[:], u))
		case u >= 0xD800 && u <= 0xDFFF:
			// Lone surrogate (high without a low, or a low on its own):
			// preserve as a distinct WTF-8 sequence.
			sb.WriteString(encodeLoneSurrogate(buf[:], u))
		default:
			sb.WriteRune(rune(u))
		}
	}
	return sb.String()
}

// encodeLoneSurrogate writes the WTF-8 (3-byte) encoding of a surrogate code
// unit into buf and returns it as a string. utf8.EncodeRune cannot be used —
// it rejects surrogates and substitutes U+FFFD — so the bytes are emitted
// directly. The surrogate code points D800–DFFF all fall in the 3-byte UTF-8
// range, so this is a fixed 3-byte form.
func encodeLoneSurrogate(buf []byte, u uint16) string {
	cp := uint32(u)
	buf[0] = byte(0xE0 | (cp >> 12))
	buf[1] = byte(0x80 | ((cp >> 6) & 0x3F))
	buf[2] = byte(0x80 | (cp & 0x3F))
	return string(buf[:3])
}

// encodeUTF16LESurrogateSafe converts a Go string to UTF-16LE bytes, acting as
// the exact inverse of decodeUTF16LESurrogateSafe. Runes in the surrogate range
// (decoded from WTF-8) are emitted as a single code unit; supplementary runes
// are split into a high/low surrogate pair via the stdlib. This guarantees a
// name produced by decodeUTF16LESurrogateSafe re-encodes to its original bytes.
func encodeUTF16LESurrogateSafe(s string) []byte {
	w := smbenc.NewWriter(len(s) * 2)
	for _, r := range decodeWTF8Runes(s) {
		switch {
		case r >= 0xD800 && r <= 0xDFFF:
			// Preserved lone surrogate: emit the single code unit verbatim.
			w.WriteUint16(uint16(r))
		case r > 0xFFFF:
			hi, lo := utf16.EncodeRune(r)
			w.WriteUint16(uint16(hi))
			w.WriteUint16(uint16(lo))
		default:
			w.WriteUint16(uint16(r))
		}
	}
	return w.Bytes()
}

// decodeWTF8Runes walks a (possibly WTF-8) string and yields its code points,
// recovering surrogate code points that range-over-string would otherwise hide.
// Ranging a Go string with `for _, r := range s` decodes invalid sequences to
// U+FFFD, which would discard the lone surrogates decodeUTF16LESurrogateSafe
// took care to preserve; this hand walk keeps them intact.
func decodeWTF8Runes(s string) []rune {
	runes := make([]rune, 0, len(s))
	for i := 0; i < len(s); {
		// Detect a raw 3-byte surrogate sequence ED A0..BF 80..BF.
		if i+3 <= len(s) && s[i] == 0xED && s[i+1] >= 0xA0 && s[i+1] <= 0xBF &&
			s[i+2] >= 0x80 && s[i+2] <= 0xBF {
			cp := (rune(s[i]&0x0F) << 12) | (rune(s[i+1]&0x3F) << 6) | rune(s[i+2]&0x3F)
			runes = append(runes, cp)
			i += 3
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		runes = append(runes, r)
		if size == 0 {
			size = 1 // never stall on an empty decode
		}
		i += size
	}
	return runes
}

// modeBitMaskAttrs builds a metadata.SetAttrs that flips a single FSCTL-managed
// mode bit (e.g. modeDOSSparse, modeDOSCompressed) atomically inside the store's
// own read-modify-write. Using the mask form rather than reading Mode in the
// handler and writing it back avoids a lost-update race when two IOCTLs
// (SET_SPARSE vs SET_COMPRESSION) touch independent bits concurrently.
func modeBitMaskAttrs(bit uint32, set bool) metadata.SetAttrs {
	mask := bit
	var attrs metadata.SetAttrs
	if set {
		attrs.ModeOrMask = &mask
	} else {
		attrs.ModeAndNotMask = &mask
	}
	return attrs
}

package handlers

import (
	"fmt"
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// maxDirectoryReadBytes is the maximum number of bytes to request from the
// metadata store when reading directory entries. This limits memory usage
// for directories with many entries. 1MB allows listing directories with
// approximately 5000 entries (estimated at ~200 bytes per entry average).
// Actual entry sizes vary with filename length; entries with long filenames
// will be larger. This is a per-request limit, not a per-connection limit.
const maxDirectoryReadBytes uint32 = 1048576

// ============================================================================
// Request and Response Structures
// ============================================================================

// QueryDirectoryRequest represents an SMB2 QUERY_DIRECTORY request from a client [MS-SMB2] 2.2.33.
// QUERY_DIRECTORY enumerates files and subdirectories in a directory handle,
// optionally filtered by a search pattern. The fixed wire format is 32 bytes
// plus a variable-length search pattern.
type QueryDirectoryRequest struct {
	// FileInfoClass specifies the type of directory information to return.
	// Common values:
	//   - 3 (FileDirectoryInformation): Basic info
	//   - 4 (FileFullDirectoryInformation): Full info
	//   - 5 (FileBothDirectoryInformation): Both info
	//   - 12 (FileNamesInformation): Names only
	//   - 37 (FileIdBothDirectoryInformation): Both info with FileID
	FileInfoClass uint8

	// Flags controls enumeration behavior.
	// Bit flags:
	//   - 0x01 (SMB2_RESTART_SCANS): Restart enumeration from beginning
	//   - 0x02 (SMB2_RETURN_SINGLE_ENTRY): Return only one entry
	//   - 0x04 (SMB2_INDEX_SPECIFIED): Use FileIndex for resumption
	//   - 0x10 (SMB2_REOPEN): Reopen directory handle
	Flags uint8

	// FileIndex is used for resuming enumeration (when SMB2_INDEX_SPECIFIED is set).
	FileIndex uint32

	// FileID is the SMB2 directory handle from CREATE response.
	FileID [16]byte

	// FileNameOffset is the offset to the search pattern from the SMB2 header.
	FileNameOffset uint16

	// FileNameLength is the length of the search pattern in bytes.
	FileNameLength uint16

	// OutputBufferLength is the maximum bytes to return.
	OutputBufferLength uint32

	// FileName is the search pattern (e.g., "*", "*.txt", "report*").
	// Empty or "*" matches all entries.
	FileName string
}

// QueryDirectoryResponse represents an SMB2 QUERY_DIRECTORY response to a client [MS-SMB2] 2.2.34.
// The response contains an array of directory entries matching the search pattern.
type QueryDirectoryResponse struct {
	SMBResponseBase // Embeds Status field and GetStatus() method

	// Data contains encoded directory entries.
	// Format depends on FileInfoClass from the request.
	Data []byte
}

// DirectoryEntry represents a file entry in directory listing.
// Used by QUERY_DIRECTORY to return information about files and
// subdirectories. Maps to various FILE_*_INFORMATION wire structures.
type DirectoryEntry struct {
	// FileName is the name of the file or directory.
	FileName string

	// FileIndex is the position within the directory.
	FileIndex uint64

	// CreationTime is when the file was created.
	CreationTime time.Time

	// LastAccessTime is when the file was last accessed.
	LastAccessTime time.Time

	// LastWriteTime is when the file was last written.
	LastWriteTime time.Time

	// ChangeTime is when the file metadata last changed.
	ChangeTime time.Time

	// EndOfFile is the actual file size in bytes.
	EndOfFile uint64

	// AllocationSize is the allocated size in bytes (cluster-aligned).
	AllocationSize uint64

	// FileAttributes contains the file's attributes.
	FileAttributes types.FileAttributes

	// EaSize is the size of extended attributes (usually 0).
	EaSize uint32

	// FileID is a unique identifier for the file.
	FileID uint64

	// ShortName is the 8.3 format short name (legacy DOS compatibility).
	ShortName string
}

// ============================================================================
// Encoding/Decoding Functions
// ============================================================================

// DecodeQueryDirectoryRequest parses an SMB2 QUERY_DIRECTORY request body [MS-SMB2] 2.2.33.
// Returns an error if the body is less than 32 bytes.
func DecodeQueryDirectoryRequest(body []byte) (*QueryDirectoryRequest, error) {
	if len(body) < 32 {
		return nil, fmt.Errorf("QUERY_DIRECTORY request too short: %d bytes", len(body))
	}

	r := smbenc.NewReader(body)
	_ = r.ReadUint16()                   // StructureSize (always 33)
	fileInfoClass := r.ReadUint8()       // FileInfoClass
	flags := r.ReadUint8()               // Flags
	fileIndex := r.ReadUint32()          // FileIndex
	fileID := r.ReadBytes(16)            // FileID
	fileNameOffset := r.ReadUint16()     // FileNameOffset
	fileNameLength := r.ReadUint16()     // FileNameLength
	outputBufferLength := r.ReadUint32() // OutputBufferLength

	req := &QueryDirectoryRequest{
		FileInfoClass:      fileInfoClass,
		Flags:              flags,
		FileIndex:          fileIndex,
		FileNameOffset:     fileNameOffset,
		FileNameLength:     fileNameLength,
		OutputBufferLength: outputBufferLength,
	}
	if fileID != nil {
		copy(req.FileID[:], fileID)
	}

	// Extract filename pattern (UTF-16LE encoded)
	// FileNameOffset is relative to the start of SMB2 header (64 bytes)
	// body starts after the header, so:
	//   body offset = FileNameOffset - 64
	// Typical FileNameOffset is 96 (64 header + 32 fixed part), giving body offset 32
	if req.FileNameLength > 0 {
		bodyOffset := int(req.FileNameOffset) - 64

		// Clamp to valid range (filename can't start before the Buffer field at byte 32)
		bodyOffset = max(bodyOffset, 32)

		if bodyOffset+int(req.FileNameLength) <= len(body) {
			req.FileName = decodeUTF16LE(body[bodyOffset : bodyOffset+int(req.FileNameLength)])
		}
	}

	return req, nil
}

// Encode serializes the QueryDirectoryResponse into SMB2 wire format [MS-SMB2] 2.2.34.
func (resp *QueryDirectoryResponse) Encode() ([]byte, error) {
	// Fixed response header is 8 bytes, data follows immediately
	w := smbenc.NewWriter(8 + len(resp.Data))
	w.WriteUint16(9)                      // StructureSize (per spec, always 9)
	w.WriteUint16(uint16(64 + 8))         // OutputBufferOffset (header + 8 byte response)
	w.WriteUint32(uint32(len(resp.Data))) // OutputBufferLength
	w.WriteBytes(resp.Data)

	return w.Bytes(), nil
}

// EncodeDirectoryEntry encodes a single directory entry for FILE_ID_BOTH_DIRECTORY_INFORMATION.
// The result is 8-byte aligned. nextOffset should be 0 for the last entry.
func EncodeDirectoryEntry(entry *DirectoryEntry, nextOffset uint32) []byte {
	// FILE_ID_BOTH_DIRECTORY_INFORMATION structure
	// Fixed part is 104 bytes + variable FileName

	fileNameBytes := encodeUTF16LE(entry.FileName)
	shortNameBytes := encodeUTF16LE(entry.ShortName)
	if len(shortNameBytes) > 24 {
		shortNameBytes = shortNameBytes[:24] // Max 24 bytes for ShortName
	}

	// Total size must be 8-byte aligned
	totalSize := 104 + len(fileNameBytes)
	paddedSize := (totalSize + 7) &^ 7

	w := smbenc.NewWriter(paddedSize)
	w.WriteUint32(nextOffset)                                 // NextEntryOffset
	w.WriteUint32(uint32(entry.FileIndex))                    // FileIndex
	w.WriteUint64(types.TimeToFiletime(entry.CreationTime))   // CreationTime
	w.WriteUint64(types.TimeToFiletime(entry.LastAccessTime)) // LastAccessTime
	w.WriteUint64(types.TimeToFiletime(entry.LastWriteTime))  // LastWriteTime
	w.WriteUint64(types.TimeToFiletime(entry.ChangeTime))     // ChangeTime
	w.WriteUint64(entry.EndOfFile)                            // EndOfFile
	w.WriteUint64(entry.AllocationSize)                       // AllocationSize
	w.WriteUint32(uint32(entry.FileAttributes))               // FileAttributes
	w.WriteUint32(uint32(len(fileNameBytes)))                 // FileNameLength
	w.WriteUint32(entry.EaSize)                               // EaSize
	w.WriteUint8(byte(len(shortNameBytes)))                   // ShortNameLength
	w.WriteUint8(0)                                           // Reserved1
	// ShortName: 24-byte fixed field, zero-padded
	shortPadded := make([]byte, 24)
	copy(shortPadded, shortNameBytes)
	w.WriteBytes(shortPadded)   // ShortName (24 bytes max)
	w.WriteUint16(0)            // Reserved2
	w.WriteUint64(entry.FileID) // FileId
	w.WriteBytes(fileNameBytes) // FileName
	// Pad to 8-byte alignment
	if paddedSize > w.Len() {
		w.WriteZeros(paddedSize - w.Len())
	}

	return w.Bytes()
}

// ============================================================================
// Protocol Handler
// ============================================================================

// QueryDirectory handles SMB2 QUERY_DIRECTORY command [MS-SMB2] 2.2.33, 2.2.34.
//
// QUERY_DIRECTORY enumerates files and subdirectories in a directory handle,
// filtered by an optional search pattern. It supports multiple FileInfoClass
// formats, handles enumeration state (restart/resume), and adds the "." and
// ".." special entries on the first scan. Returns StatusNoMoreFiles when
// enumeration is complete.
func (h *Handler) QueryDirectory(ctx *SMBHandlerContext, req *QueryDirectoryRequest) (*QueryDirectoryResponse, error) {
	logger.Debug("QUERY_DIRECTORY request",
		"fileInfoClass", req.FileInfoClass,
		"flags", req.Flags,
		"fileID", fmt.Sprintf("%x", req.FileID),
		"fileName", req.FileName)

	// Get OpenFile by FileID
	openFile, ok := h.GetOpenFile(req.FileID)
	if !ok {
		logger.Debug("QUERY_DIRECTORY: file handle not found (closed)", "fileID", fmt.Sprintf("%x", req.FileID))
		return &QueryDirectoryResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusFileClosed}}, nil
	}

	if !openFile.IsDirectory {
		logger.Debug("QUERY_DIRECTORY: not a directory", "path", openFile.Path)
		return &QueryDirectoryResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidParameter}}, nil
	}

	// Build AuthContext
	authCtx, err := BuildAuthContext(ctx)
	if err != nil {
		logger.Warn("QUERY_DIRECTORY: failed to build auth context", "error", err)
		return &QueryDirectoryResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusAccessDenied}}, nil
	}

	// Handle enumeration state
	flags := types.QueryDirectoryFlags(req.Flags)
	restartScans := flags&types.SMB2RestartScans != 0

	returnSingleEntry := flags&types.SMB2ReturnSingleEntry != 0
	reopenFlag := flags&types.SMB2Reopen != 0

	if reopenFlag {
		// SMB2_REOPEN: reset enumeration state
		openFile.EnumerationComplete = false
		openFile.EnumerationIndex = 0
		openFile.EnumerationPattern = ""
	}

	// Per MS-SMB2 3.3.5.17: If the search pattern has changed since the
	// last query, the server MUST restart the enumeration from the beginning.
	// This allows a client to first enumerate with "*" and then query for a
	// specific file by name without getting NO_MORE_FILES.
	patternChanged := req.FileName != "" && openFile.EnumerationPattern != "" &&
		req.FileName != openFile.EnumerationPattern
	if patternChanged {
		logger.Debug("QUERY_DIRECTORY: search pattern changed, restarting enumeration",
			"old", openFile.EnumerationPattern, "new", req.FileName)
		openFile.EnumerationComplete = false
		openFile.EnumerationIndex = 0
	}

	if openFile.EnumerationComplete && !restartScans && !reopenFlag {
		return &QueryDirectoryResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusNoMoreFiles}}, nil
	}

	// Track whether this is a fresh enumeration (first time, restart, or reopen)
	startingFresh := openFile.EnumerationIndex == 0

	if restartScans {
		openFile.EnumerationComplete = false
		openFile.EnumerationIndex = 0
		openFile.EnumerationPattern = ""
		startingFresh = true
	}

	// Store the current search pattern for change detection on subsequent calls
	if req.FileName != "" {
		openFile.EnumerationPattern = req.FileName
	}

	// Read directory entries from metadata store
	metaSvc := h.Registry.GetMetadataService()
	page, err := metaSvc.ReadDirectory(authCtx, openFile.MetadataHandle, 0, maxDirectoryReadBytes)
	if err != nil {
		logger.Debug("QUERY_DIRECTORY: failed to read directory", "path", openFile.Path, "error", err)
		return &QueryDirectoryResponse{SMBResponseBase: SMBResponseBase{Status: MetadataErrorToSMBStatus(err)}}, nil
	}

	// Fetch the directory's own attributes so "." and ".." entries report the
	// actual directory timestamps instead of NowFiletime(). This prevents
	// CreationTime drift between consecutive QUERY_DIRECTORY calls.
	var dirAttr *metadata.FileAttr
	dirFile, err := metaSvc.GetFile(authCtx.Context, openFile.MetadataHandle)
	if err == nil && dirFile != nil {
		dirAttr = &dirFile.FileAttr
	}

	// Filter entries and build response
	filteredEntries := filterDirEntries(page.Entries, req.FileName)

	isWildcardSearch := req.FileName == "" || req.FileName == "*" || req.FileName == "*.*"
	// Include "." and ".." when starting a fresh enumeration with a wildcard search
	includeSpecial := startingFresh && isWildcardSearch

	// For SMB2_RETURN_SINGLE_ENTRY, skip entries already returned in previous calls
	// and return only one entry at a time, advancing EnumerationIndex.
	if returnSingleEntry {
		// For wildcard searches that started fresh, "." and ".." are the first two
		// entries in the enumeration. We must consistently account for them across
		// ALL calls during the same enumeration (not just the first call), because
		// EnumerationIndex tracks position in the combined (specials + real) list.
		//
		// Special entries are included when:
		// - This is a wildcard search ("*", "*.*", or empty)
		// - AND the enumeration was started fresh (index was 0 at some point)
		//
		// We use isWildcardSearch (not includeSpecial) because includeSpecial
		// requires startingFresh, which is only true on the very first call.
		// On subsequent calls with the same wildcard enumeration, we still need
		// specialCount=2 so that totalEntries and index offsets remain consistent.
		specialCount := 0
		if isWildcardSearch {
			specialCount = 2
		}
		totalEntries := specialCount + len(filteredEntries)
		idx := openFile.EnumerationIndex

		if idx >= totalEntries {
			openFile.EnumerationComplete = true
			h.StoreOpenFile(openFile)
			return &QueryDirectoryResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusNoMoreFiles}}, nil
		}

		// Build a single entry at the current index
		fileInfoClass := types.FileInfoClass(req.FileInfoClass)
		var singleFilteredEntries []metadata.DirEntry
		singleIncludeSpecial := false

		if idx < specialCount {
			// Return a "." or ".." entry
			singleIncludeSpecial = true
		} else {
			// Return a regular entry at offset idx-specialCount
			realIdx := idx - specialCount
			if realIdx < len(filteredEntries) {
				singleFilteredEntries = filteredEntries[realIdx : realIdx+1]
			}
		}

		var entries []byte
		if singleIncludeSpecial {
			// Build both special entries plus nothing else, then truncate to the right one
			entries = buildDirInfoEntries(fileInfoClass, nil, true, dirAttr)
			if idx == 0 {
				entries = truncateToFirstEntry(entries) // "."
			} else {
				entries = truncateToNthEntry(entries, 1) // ".."
			}
		} else {
			entries = buildDirInfoEntries(fileInfoClass, singleFilteredEntries, false, dirAttr)
		}

		if len(entries) == 0 {
			openFile.EnumerationComplete = true
			h.StoreOpenFile(openFile)
			return &QueryDirectoryResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusNoMoreFiles}}, nil
		}

		openFile.EnumerationIndex = idx + 1
		if openFile.EnumerationIndex >= totalEntries {
			openFile.EnumerationComplete = true
		}
		h.StoreOpenFile(openFile)

		return h.buildQueryDirectoryResponse(entries, req.OutputBufferLength, openFile, fileInfoClass)
	}

	fileInfoClass := types.FileInfoClass(req.FileInfoClass)
	entries := buildDirInfoEntries(fileInfoClass, filteredEntries, includeSpecial, dirAttr)

	if len(entries) == 0 {
		return &QueryDirectoryResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusNoMoreFiles}}, nil
	}

	// Mark enumeration as complete - next call without restart will return NO_MORE_FILES
	openFile.EnumerationComplete = true
	h.StoreOpenFile(openFile)

	return h.buildQueryDirectoryResponse(entries, req.OutputBufferLength, openFile, fileInfoClass)
}

// ============================================================================
// Helper Functions - Directory Entry Building
// ============================================================================

// dirEntryWireFields holds the pre-computed wire-format fields shared by all
// FILE_*_INFORMATION directory entry structures. Avoids repeating the
// attr-to-wire conversion in every per-format encoding function.
type dirEntryWireFields struct {
	creationTime   uint64
	accessTime     uint64
	writeTime      uint64
	changeTime     uint64
	size           uint64
	allocationSize uint64
	attrs          types.FileAttributes
}

// resolveDirEntryFields computes wire-format fields from a FileAttr and name.
// When attr is nil (e.g. for "." and ".." entries without a resolved attr), uses
// current time and directory attributes as a fallback. Callers should prefer
// passing actual attributes via resolveDirEntryFieldsWithFallback.
func resolveDirEntryFields(attr *metadata.FileAttr, name string) dirEntryWireFields {
	if attr == nil {
		now := types.NowFiletime()
		return dirEntryWireFields{
			creationTime: now,
			accessTime:   now,
			writeTime:    now,
			changeTime:   now,
			attrs:        types.FileAttributeDirectory,
		}
	}

	creation, access, write, change := FileAttrToSMBTimes(attr)
	size := getSMBSize(attr)
	return dirEntryWireFields{
		creationTime:   types.TimeToFiletime(creation),
		accessTime:     types.TimeToFiletime(access),
		writeTime:      types.TimeToFiletime(write),
		changeTime:     types.TimeToFiletime(change),
		size:           size,
		allocationSize: calculateAllocationSize(size),
		attrs:          FileAttrToSMBAttributesWithName(attr, name),
	}
}

// writeCommonDirFields writes the 56-byte timestamp/size/attribute block shared
// across all directory info structures starting at the given offset:
//
//	[0:8]   CreationTime
//	[8:16]  LastAccessTime
//	[16:24] LastWriteTime
//	[24:32] ChangeTime
//	[32:40] EndOfFile
//	[40:48] AllocationSize
//	[48:52] FileAttributes
//	[52:56] FileNameLength
func writeCommonDirFields(entry []byte, offset int, f dirEntryWireFields, fileNameLen int) {
	w := smbenc.NewWriter(56)
	w.WriteUint64(f.creationTime)
	w.WriteUint64(f.accessTime)
	w.WriteUint64(f.writeTime)
	w.WriteUint64(f.changeTime)
	w.WriteUint64(f.size)
	w.WriteUint64(f.allocationSize)
	w.WriteUint32(uint32(f.attrs))
	w.WriteUint32(uint32(fileNameLen))
	copy(entry[offset:], w.Bytes())
}

// allocAlignedEntry allocates a zero-filled byte slice of at least baseSize+nameLen
// bytes, padded to an 8-byte boundary.
func allocAlignedEntry(baseSize, nameLen int) []byte {
	return make([]byte, (baseSize+nameLen+7)&^7)
}

// linkEntry appends entry to result and updates the NextEntryOffset chain.
// prevNextOffset tracks the byte offset within result where the previous
// entry's NextEntryOffset field starts.
func linkEntry(result []byte, prevNextOffset *int, entry []byte) []byte {
	if len(result) > 0 {
		w := smbenc.NewWriter(4)
		w.WriteUint32(uint32(len(result) - *prevNextOffset))
		copy(result[*prevNextOffset:], w.Bytes())
	}
	*prevNextOffset = len(result)
	return append(result, entry...)
}

// buildDirInfoEntries dispatches to the appropriate per-format builder based on fileInfoClass.
// dirAttr is the FileAttr of the directory itself, used for "." and ".." entries so they
// report the directory's actual timestamps instead of NowFiletime(). May be nil as fallback.
func buildDirInfoEntries(fileInfoClass types.FileInfoClass, entries []metadata.DirEntry, includeSpecial bool, dirAttr *metadata.FileAttr) []byte {
	switch fileInfoClass {
	case types.FileBothDirectoryInformation:
		return buildFileBothDirInfo(entries, includeSpecial, dirAttr)
	case types.FileIdBothDirectoryInformation:
		return buildFileIdBothDirInfo(entries, includeSpecial, dirAttr)
	case types.FileIdFullDirectoryInformation:
		return buildFileIdFullDirInfo(entries, includeSpecial, dirAttr)
	case types.FileFullDirectoryInformation:
		return buildFileFullDirInfo(entries, includeSpecial, dirAttr)
	case types.FileDirectoryInformation:
		return buildFileDirInfo(entries, includeSpecial, dirAttr)
	case types.FileNamesInformation:
		return buildFileNamesInfo(entries, includeSpecial)
	default:
		return buildFileBothDirInfo(entries, includeSpecial, dirAttr)
	}
}

// buildFileBothDirInfo builds FILE_BOTH_DIR_INFORMATION structures [MS-FSCC] 2.4.8.
func buildFileBothDirInfo(entries []metadata.DirEntry, includeSpecial bool, dirAttr *metadata.FileAttr) []byte {
	var result []byte
	var prevNextOffset int
	var fileIndex uint64 = 1

	if includeSpecial {
		result = linkEntry(result, &prevNextOffset, encodeBothDirEntry(".", dirAttr, fileIndex))
		fileIndex++
		result = linkEntry(result, &prevNextOffset, encodeBothDirEntry("..", dirAttr, fileIndex))
		fileIndex++
	}

	for i := range entries {
		result = linkEntry(result, &prevNextOffset, encodeBothDirEntry(entries[i].Name, entries[i].Attr, fileIndex))
		fileIndex++
	}

	return result
}

// encodeBothDirEntry encodes a single FILE_BOTH_DIR_INFORMATION entry (94-byte base + filename).
func encodeBothDirEntry(name string, attr *metadata.FileAttr, fileIndex uint64) []byte {
	nameBytes := encodeUTF16LE(name)
	f := resolveDirEntryFields(attr, name)

	entry := allocAlignedEntry(94, len(nameBytes))
	// NextEntryOffset (4 bytes at 0) left as zero, patched by linkEntry
	w := smbenc.NewWriter(4)
	w.WriteUint32(uint32(fileIndex))
	copy(entry[4:8], w.Bytes()) // FileIndex
	writeCommonDirFields(entry, 8, f, len(nameBytes))
	// EaSize (4 bytes at 64) left as zero
	shortNameBytes := generate83ShortName(name)
	shortNameLen := len(shortNameBytes)
	if shortNameLen > 24 {
		shortNameLen = 24
	}
	entry[68] = byte(shortNameLen) // ShortNameLength
	// Reserved (1 byte at 69)
	if shortNameLen > 0 {
		copy(entry[70:70+shortNameLen], shortNameBytes[:shortNameLen]) // ShortName (24 bytes max)
	}
	copy(entry[94:], nameBytes)

	return entry
}

// buildFileIdBothDirInfo builds FILE_ID_BOTH_DIR_INFORMATION structures [MS-FSCC] 2.4.17.
func buildFileIdBothDirInfo(entries []metadata.DirEntry, includeSpecial bool, dirAttr *metadata.FileAttr) []byte {
	var result []byte
	var prevNextOffset int
	var fileIndex uint64 = 1

	if includeSpecial {
		// "." gets a non-zero FileId (the directory's own identifier).
		// WPTS asserts FileId != 0 for the "." entry.
		// ".." gets FileId=0 (parent directory reference is unknown/not tracked).
		// WPTS asserts FileId == 0 for the ".." entry.
		result = linkEntry(result, &prevNextOffset, encodeIdBothDirEntry(".", dirAttr, fileIndex, fileIndex))
		fileIndex++
		result = linkEntry(result, &prevNextOffset, encodeIdBothDirEntry("..", dirAttr, fileIndex, 0))
		fileIndex++
	}

	for i := range entries {
		result = linkEntry(result, &prevNextOffset, encodeIdBothDirEntry(entries[i].Name, entries[i].Attr, fileIndex, entries[i].ID))
		fileIndex++
	}

	return result
}

// encodeIdBothDirEntry encodes a single FILE_ID_BOTH_DIR_INFORMATION entry (104-byte base + filename).
func encodeIdBothDirEntry(name string, attr *metadata.FileAttr, fileIndex uint64, fileID uint64) []byte {
	nameBytes := encodeUTF16LE(name)
	f := resolveDirEntryFields(attr, name)

	entry := allocAlignedEntry(104, len(nameBytes))
	// NextEntryOffset (4 bytes at 0) left as zero, patched by linkEntry
	w := smbenc.NewWriter(4)
	w.WriteUint32(uint32(fileIndex))
	copy(entry[4:8], w.Bytes()) // FileIndex
	writeCommonDirFields(entry, 8, f, len(nameBytes))
	// EaSize (4 bytes at 64) left as zero
	shortNameBytes := generate83ShortName(name)
	shortNameLen := len(shortNameBytes)
	if shortNameLen > 24 {
		shortNameLen = 24
	}
	entry[68] = byte(shortNameLen) // ShortNameLength
	// Reserved1 (1 byte at 69)
	if shortNameLen > 0 {
		copy(entry[70:70+shortNameLen], shortNameBytes[:shortNameLen]) // ShortName (24 bytes max)
	}
	// Reserved2 (2 bytes at 94-95)
	wID := smbenc.NewWriter(8)
	wID.WriteUint64(fileID)
	copy(entry[96:104], wID.Bytes()) // FileId
	copy(entry[104:], nameBytes)

	return entry
}

// buildFileIdFullDirInfo builds FILE_ID_FULL_DIR_INFORMATION structures [MS-FSCC] 2.4.18.
func buildFileIdFullDirInfo(entries []metadata.DirEntry, includeSpecial bool, dirAttr *metadata.FileAttr) []byte {
	var result []byte
	var prevNextOffset int
	var fileIndex uint64 = 1

	if includeSpecial {
		// "." gets a non-zero FileId (the directory's own identifier).
		// ".." gets FileId=0 (parent directory reference is unknown/not tracked).
		result = linkEntry(result, &prevNextOffset, encodeIdFullDirEntry(".", dirAttr, fileIndex, fileIndex))
		fileIndex++
		result = linkEntry(result, &prevNextOffset, encodeIdFullDirEntry("..", dirAttr, fileIndex, 0))
		fileIndex++
	}

	for i := range entries {
		result = linkEntry(result, &prevNextOffset, encodeIdFullDirEntry(entries[i].Name, entries[i].Attr, fileIndex, entries[i].ID))
		fileIndex++
	}

	return result
}

// encodeIdFullDirEntry encodes a single FILE_ID_FULL_DIR_INFORMATION entry (80-byte base + filename).
func encodeIdFullDirEntry(name string, attr *metadata.FileAttr, fileIndex uint64, fileID uint64) []byte {
	nameBytes := encodeUTF16LE(name)
	f := resolveDirEntryFields(attr, name)

	entry := allocAlignedEntry(80, len(nameBytes))
	// NextEntryOffset (4 bytes at 0) left as zero, patched by linkEntry
	w := smbenc.NewWriter(4)
	w.WriteUint32(uint32(fileIndex))
	copy(entry[4:8], w.Bytes()) // FileIndex
	writeCommonDirFields(entry, 8, f, len(nameBytes))
	// EaSize (4 bytes at 64), Reserved (4 bytes at 68) are left as zero
	wID := smbenc.NewWriter(8)
	wID.WriteUint64(fileID)
	copy(entry[72:80], wID.Bytes()) // FileId
	copy(entry[80:], nameBytes)

	return entry
}

// buildFileFullDirInfo builds FILE_FULL_DIR_INFORMATION structures [MS-FSCC] 2.4.14.
func buildFileFullDirInfo(entries []metadata.DirEntry, includeSpecial bool, dirAttr *metadata.FileAttr) []byte {
	var result []byte
	var prevNextOffset int
	var fileIndex uint64 = 1

	if includeSpecial {
		result = linkEntry(result, &prevNextOffset, encodeFullDirEntry(".", dirAttr, fileIndex))
		fileIndex++
		result = linkEntry(result, &prevNextOffset, encodeFullDirEntry("..", dirAttr, fileIndex))
		fileIndex++
	}

	for i := range entries {
		result = linkEntry(result, &prevNextOffset, encodeFullDirEntry(entries[i].Name, entries[i].Attr, fileIndex))
		fileIndex++
	}

	return result
}

// encodeFullDirEntry encodes a single FILE_FULL_DIR_INFORMATION entry (68-byte base + filename).
func encodeFullDirEntry(name string, attr *metadata.FileAttr, fileIndex uint64) []byte {
	nameBytes := encodeUTF16LE(name)
	f := resolveDirEntryFields(attr, name)

	entry := allocAlignedEntry(68, len(nameBytes))
	// NextEntryOffset (4 bytes at 0) left as zero, patched by linkEntry
	w := smbenc.NewWriter(4)
	w.WriteUint32(uint32(fileIndex))
	copy(entry[4:8], w.Bytes()) // FileIndex
	writeCommonDirFields(entry, 8, f, len(nameBytes))
	// EaSize (4 bytes at 64) is left as zero
	copy(entry[68:], nameBytes)

	return entry
}

// buildFileDirInfo builds FILE_DIRECTORY_INFORMATION structures [MS-FSCC] 2.4.10.
func buildFileDirInfo(entries []metadata.DirEntry, includeSpecial bool, dirAttr *metadata.FileAttr) []byte {
	var result []byte
	var prevNextOffset int
	var fileIndex uint64 = 1

	if includeSpecial {
		result = linkEntry(result, &prevNextOffset, encodeDirInfoEntry(".", dirAttr, fileIndex))
		fileIndex++
		result = linkEntry(result, &prevNextOffset, encodeDirInfoEntry("..", dirAttr, fileIndex))
		fileIndex++
	}

	for i := range entries {
		result = linkEntry(result, &prevNextOffset, encodeDirInfoEntry(entries[i].Name, entries[i].Attr, fileIndex))
		fileIndex++
	}

	return result
}

// encodeDirInfoEntry encodes a single FILE_DIRECTORY_INFORMATION entry (64-byte base + filename).
func encodeDirInfoEntry(name string, attr *metadata.FileAttr, fileIndex uint64) []byte {
	nameBytes := encodeUTF16LE(name)
	f := resolveDirEntryFields(attr, name)

	entry := allocAlignedEntry(64, len(nameBytes))
	// NextEntryOffset (4 bytes at 0) left as zero, patched by linkEntry
	w := smbenc.NewWriter(4)
	w.WriteUint32(uint32(fileIndex))
	copy(entry[4:8], w.Bytes()) // FileIndex
	writeCommonDirFields(entry, 8, f, len(nameBytes))
	copy(entry[64:], nameBytes)

	return entry
}

// buildFileNamesInfo builds FILE_NAMES_INFORMATION structures [MS-FSCC] 2.4.26.
func buildFileNamesInfo(entries []metadata.DirEntry, includeSpecial bool) []byte {
	var result []byte
	var prevNextOffset int
	var fileIndex uint64 = 1

	if includeSpecial {
		result = linkEntry(result, &prevNextOffset, encodeNamesEntry(".", fileIndex))
		fileIndex++
		result = linkEntry(result, &prevNextOffset, encodeNamesEntry("..", fileIndex))
		fileIndex++
	}

	for i := range entries {
		result = linkEntry(result, &prevNextOffset, encodeNamesEntry(entries[i].Name, fileIndex))
		fileIndex++
	}

	return result
}

// encodeNamesEntry encodes a single FILE_NAMES_INFORMATION entry (12-byte base + filename).
func encodeNamesEntry(name string, fileIndex uint64) []byte {
	nameBytes := encodeUTF16LE(name)

	entry := allocAlignedEntry(12, len(nameBytes))
	// NextEntryOffset (4 bytes at 0) left as zero, patched by linkEntry
	w := smbenc.NewWriter(8)
	w.WriteUint32(uint32(fileIndex))      // FileIndex
	w.WriteUint32(uint32(len(nameBytes))) // FileNameLength
	copy(entry[4:12], w.Bytes())
	copy(entry[12:], nameBytes)

	return entry
}

// ============================================================================
// Helper Functions - Filtering
// ============================================================================

// filterDirEntries filters directory entries based on the SMB2 search pattern.
//
// Pattern can be:
//   - "*" or empty: match all entries
//   - Exact name: match only that specific entry (case-insensitive on Windows/SMB)
//   - Wildcard pattern: support basic wildcards like "*.txt", "foo*", etc.
//
// Additionally, Unix special files (FIFO, socket, device nodes) are always filtered
// out from SMB directory listings since they have no meaningful representation in SMB.
func filterDirEntries(entries []metadata.DirEntry, pattern string) []metadata.DirEntry {
	var filtered []metadata.DirEntry

	matchAll := pattern == "" || pattern == "*" || pattern == "<" || pattern == "*.*"

	for _, entry := range entries {
		// Skip Unix special files (FIFO, socket, block/char device) - they have no SMB equivalent
		if entry.Attr != nil && IsSpecialFile(entry.Attr.Type) {
			continue
		}

		if matchAll || matchSMBPattern(entry.Name, pattern) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

// matchSMBPattern matches a filename against an SMB/DOS search pattern.
//
// Per MS-FSCC 2.1.4.4, SMB uses DOS-style wildcards including:
//   - * matches zero or more characters
//   - ? matches exactly one character (including dot and end-of-name)
//   - < (DOS_STAR) matches zero or more characters until the last dot
//   - > (DOS_QM) matches any single character, or a dot, or end-of-name
//   - " (DOS_DOT) matches a period or end-of-name
//
// Matching is case-insensitive (Windows behavior).
func matchSMBPattern(name, pattern string) bool {
	return matchDOSWildcard(strings.ToLower(name), strings.ToLower(pattern))
}

// matchDOSWildcard implements MS-FSCC 2.1.4.4 pattern matching with DOS wildcards.
func matchDOSWildcard(name, pattern string) bool {
	ni := 0 // name index
	pi := 0 // pattern index

	for pi < len(pattern) {
		pc := pattern[pi]

		switch pc {
		case '*':
			// '*' matches zero or more characters
			pi++
			if pi >= len(pattern) {
				return true
			}
			for ni <= len(name) {
				if matchDOSWildcard(name[ni:], pattern[pi:]) {
					return true
				}
				if ni < len(name) {
					ni++
				} else {
					break
				}
			}
			return false

		case '?':
			// '?' matches exactly one character (but not end-of-name)
			if ni >= len(name) {
				return false
			}
			ni++
			pi++

		case '<':
			// DOS_STAR: matches zero or more characters until encountering and
			// matching the final "." in the name. If no dot exists, behaves like '*'.
			// Per MS-FSCC 2.1.4.4, this effectively matches the "base name" portion
			// of a filename before the extension.
			pi++
			lastDot := strings.LastIndex(name[ni:], ".")
			if lastDot == -1 {
				// No dot in remaining name - behaves like '*'
				for ni <= len(name) {
					if matchDOSWildcard(name[ni:], pattern[pi:]) {
						return true
					}
					if ni < len(name) {
						ni++
					} else {
						break
					}
				}
				return false
			}
			// DOS_STAR matches zero or more characters until the last dot, inclusive.
			// Per MS-FSCC 2.1.4.4: "If the pattern is * (DOS_STAR), consume through
			// the last dot." We iterate up to dotAbsPos+1 to try the match at each
			// position from ni through one past the dot (the char after the dot),
			// which allows the remaining pattern to match the extension.
			dotAbsPos := ni + lastDot
			for ni <= dotAbsPos+1 {
				if matchDOSWildcard(name[ni:], pattern[pi:]) {
					return true
				}
				ni++
			}
			return false

		case '>':
			// DOS_QM: matches any single character, or at period/end-of-name
			pi++
			if ni >= len(name) || name[ni] == '.' {
				for pi < len(pattern) && pattern[pi] == '>' {
					pi++
				}
				continue
			}
			ni++

		case '"':
			// DOS_DOT: matches a period or end-of-name
			pi++
			if ni >= len(name) {
				continue
			}
			if name[ni] == '.' {
				ni++
			} else {
				return false
			}

		default:
			if ni >= len(name) || name[ni] != pc {
				return false
			}
			ni++
			pi++
		}
	}

	return ni >= len(name)
}

// truncateToFirstEntry returns only the first directory entry from a linked entry chain.
// Sets the NextEntryOffset of the first entry to 0 (marking it as the last entry).
func truncateToFirstEntry(buf []byte) []byte {
	if len(buf) < 4 {
		return buf
	}
	r := smbenc.NewReader(buf)
	nextOffset := r.ReadUint32()
	if nextOffset == 0 {
		return buf // already single entry
	}
	result := buf[:nextOffset]
	w := smbenc.NewWriter(4)
	w.WriteUint32(0)
	copy(result[0:4], w.Bytes())
	return result
}

// truncateToNthEntry returns only the Nth (0-based) directory entry from a linked entry chain.
// Sets the NextEntryOffset of the selected entry to 0 (marking it as the last entry).
func truncateToNthEntry(buf []byte, n int) []byte {
	offset := 0
	for i := 0; i < n; i++ {
		if offset+4 > len(buf) {
			return nil
		}
		r := smbenc.NewReader(buf[offset:])
		nextOffset := r.ReadUint32()
		if nextOffset == 0 {
			return nil // entry chain shorter than n
		}
		offset += int(nextOffset)
	}
	if offset >= len(buf) {
		return nil
	}
	r := smbenc.NewReader(buf[offset:])
	nextOffset := r.ReadUint32()
	var entryEnd int
	if nextOffset == 0 {
		entryEnd = len(buf)
	} else {
		entryEnd = offset + int(nextOffset)
	}
	result := make([]byte, entryEnd-offset)
	copy(result, buf[offset:entryEnd])
	w := smbenc.NewWriter(4)
	w.WriteUint32(0)
	copy(result[0:4], w.Bytes()) // mark as last entry
	return result
}

// buildQueryDirectoryResponse creates a QueryDirectoryResponse from pre-built entry
// data, truncating at entry boundaries if the data exceeds the output buffer length.
func (h *Handler) buildQueryDirectoryResponse(entries []byte, outputBufferLength uint32, openFile *OpenFile, fileInfoClass types.FileInfoClass) (*QueryDirectoryResponse, error) {
	// Truncate at entry boundaries if necessary
	if uint32(len(entries)) > outputBufferLength {
		entries = truncateAtEntryBoundary(entries, outputBufferLength)
		if len(entries) == 0 {
			return &QueryDirectoryResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusNoMoreFiles}}, nil
		}
	}

	logger.Debug("QUERY_DIRECTORY successful",
		"path", openFile.Path,
		"bufferSize", len(entries))

	return &QueryDirectoryResponse{
		SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
		Data:            entries,
	}, nil
}

// truncateAtEntryBoundary walks the directory entry chain and returns the buffer
// truncated to the last complete entry that fits within maxBytes.
// Each entry starts with a uint32 NextEntryOffset; the last entry has offset 0.
func truncateAtEntryBoundary(buf []byte, maxBytes uint32) []byte {
	var lastEnd uint32
	offset := uint32(0)

	for offset+4 <= uint32(len(buf)) {
		r := smbenc.NewReader(buf[offset:])
		nextOffset := r.ReadUint32()

		var entryEnd uint32
		if nextOffset == 0 {
			entryEnd = uint32(len(buf))
		} else {
			entryEnd = offset + nextOffset
		}

		if entryEnd > maxBytes {
			break
		}

		lastEnd = entryEnd
		if nextOffset == 0 {
			break
		}
		offset += nextOffset
	}

	if lastEnd == 0 {
		return nil
	}

	result := buf[:lastEnd]
	// Zero the NextEntryOffset of the last included entry
	pos := uint32(0)
	for {
		r := smbenc.NewReader(result[pos:])
		next := r.ReadUint32()
		if next == 0 || pos+next >= lastEnd {
			w := smbenc.NewWriter(4)
			w.WriteUint32(0)
			copy(result[pos:pos+4], w.Bytes())
			break
		}
		pos += next
	}

	return result
}

// generate83ShortName generates a Windows 8.3 short name in UTF-16LE.
// For "." and ".." it returns empty (no short name needed).
// For other names, it uppercases and truncates to 8.3 format.
func generate83ShortName(name string) []byte {
	if name == "." || name == ".." || name == "" {
		return nil
	}

	upper := strings.ToUpper(name)

	var base, ext string
	if dot := strings.LastIndex(upper, "."); dot >= 0 {
		base = upper[:dot]
		ext = upper[dot+1:]
	} else {
		base = upper
	}

	// Strip characters invalid in 8.3 names
	base = strings.Map(func(r rune) rune {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '~' {
			return r
		}
		return -1
	}, base)

	if len(base) > 8 {
		base = base[:6] + "~1"
	}
	if len(ext) > 3 {
		ext = ext[:3]
	}

	var shortName string
	if ext != "" {
		shortName = base + "." + ext
	} else {
		shortName = base
	}

	if shortName == "" {
		return nil
	}

	return encodeUTF16LE(shortName)
}

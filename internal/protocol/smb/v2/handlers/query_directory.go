// Package handlers provides SMB2 command handlers and session management.
//
// This file implements the SMB2 QUERY_DIRECTORY command handler [MS-SMB2] 2.2.33, 2.2.34.
// QUERY_DIRECTORY enumerates files in a directory, returning entries that match
// a specified search pattern.
package handlers

import (
	"encoding/binary"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// maxDirectoryReadBytes is the maximum number of bytes to request from the
// metadata store when reading directory entries. This limits memory usage
// for directories with many entries. 64KB is sufficient for most directory
// listings while preventing excessive memory allocation.
const maxDirectoryReadBytes uint32 = 65536

// ============================================================================
// Request and Response Structures
// ============================================================================

// QueryDirectoryRequest represents an SMB2 QUERY_DIRECTORY request from a client [MS-SMB2] 2.2.33.
//
// QUERY_DIRECTORY enumerates files and subdirectories in a directory handle.
// Results can be filtered using a search pattern.
//
// **Wire format (32 bytes fixed + variable filename):**
//
//	Offset  Size  Field              Description
//	0       2     StructureSize      Always 33 (includes 1 byte of buffer)
//	2       1     FileInfoClass      Type of directory info to return
//	3       1     Flags              Query flags (restart, single, etc.)
//	4       4     FileIndex          Index for resuming enumeration
//	8       16    FileId             SMB2 directory handle
//	24      2     FileNameOffset     Offset to search pattern
//	26      2     FileNameLength     Length of search pattern
//	28      4     OutputBufferLength Max bytes to return
//	32+     var   Buffer             Search pattern (UTF-16LE)
//
// **Example:**
//
//	req := &QueryDirectoryRequest{
//	    FileInfoClass:      FileIdBothDirectoryInformation,
//	    Flags:              0x01, // SMB2_RESTART_SCANS
//	    FileID:             dirID,
//	    FileName:           "*.txt",
//	    OutputBufferLength: 65536,
//	}
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
//
// The response contains an array of directory entries matching the search pattern.
//
// **Wire format (8 bytes fixed + variable data):**
//
//	Offset  Size  Field              Description
//	0       2     StructureSize      Always 9 (includes 1 byte of buffer)
//	2       2     OutputBufferOffset Offset from header to data
//	4       4     OutputBufferLength Length of data
//	8+      var   Buffer             Directory entries
type QueryDirectoryResponse struct {
	SMBResponseBase // Embeds Status field and GetStatus() method

	// Data contains encoded directory entries.
	// Format depends on FileInfoClass from the request.
	Data []byte
}

// DirectoryEntry represents a file entry in directory listing.
//
// Used by QUERY_DIRECTORY to return information about files and
// subdirectories within a directory. This is a convenience structure
// that maps to various FILE_*_INFORMATION structures.
//
// **Example:**
//
//	entry := &DirectoryEntry{
//	    FileName:       "document.txt",
//	    FileAttributes: types.FileAttributeNormal,
//	    EndOfFile:      1024,
//	    CreationTime:   time.Now(),
//	}
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
//
// **Parameters:**
//   - body: Request body starting after the SMB2 header (64 bytes)
//
// **Returns:**
//   - *QueryDirectoryRequest: Parsed request structure
//   - error: Decoding error if body is malformed
//
// **Example:**
//
//	req, err := DecodeQueryDirectoryRequest(body)
//	if err != nil {
//	    return NewErrorResult(types.StatusInvalidParameter), nil
//	}
func DecodeQueryDirectoryRequest(body []byte) (*QueryDirectoryRequest, error) {
	if len(body) < 32 {
		return nil, fmt.Errorf("QUERY_DIRECTORY request too short: %d bytes", len(body))
	}

	req := &QueryDirectoryRequest{
		FileInfoClass:      body[2],
		Flags:              body[3],
		FileIndex:          binary.LittleEndian.Uint32(body[4:8]),
		FileNameOffset:     binary.LittleEndian.Uint16(body[24:26]),
		FileNameLength:     binary.LittleEndian.Uint16(body[26:28]),
		OutputBufferLength: binary.LittleEndian.Uint32(body[28:32]),
	}
	copy(req.FileID[:], body[8:24])

	// Extract filename pattern (UTF-16LE encoded)
	// FileNameOffset is relative to the start of SMB2 header (64 bytes)
	// body starts after the header, so:
	//   body offset = FileNameOffset - 64
	// Typical FileNameOffset is 96 (64 header + 32 fixed part), giving body offset 32
	if req.FileNameLength > 0 {
		bodyOffset := int(req.FileNameOffset) - 64

		// Clamp to valid range (filename can't start before the Buffer field at byte 32)
		if bodyOffset < 32 {
			bodyOffset = 32
		}

		if bodyOffset+int(req.FileNameLength) <= len(body) {
			req.FileName = decodeUTF16LE(body[bodyOffset : bodyOffset+int(req.FileNameLength)])
		}
	}

	return req, nil
}

// Encode serializes the QueryDirectoryResponse into SMB2 wire format [MS-SMB2] 2.2.34.
//
// **Returns:**
//   - []byte: Response body with 8-byte header + directory entries
//   - error: Encoding error (currently always nil)
func (resp *QueryDirectoryResponse) Encode() ([]byte, error) {
	// Fixed response header is 8 bytes, data follows immediately
	buf := make([]byte, 8+len(resp.Data))
	binary.LittleEndian.PutUint16(buf[0:2], 9)                      // StructureSize (per spec, always 9)
	binary.LittleEndian.PutUint16(buf[2:4], uint16(64+8))           // OutputBufferOffset (header + 8 byte response)
	binary.LittleEndian.PutUint32(buf[4:8], uint32(len(resp.Data))) // OutputBufferLength
	copy(buf[8:], resp.Data)

	return buf, nil
}

// EncodeDirectoryEntry encodes a single directory entry for FILE_ID_BOTH_DIRECTORY_INFORMATION.
// This function is used for building directory listing responses.
//
// **Parameters:**
//   - entry: Directory entry to encode
//   - nextOffset: Offset to next entry (0 for last entry)
//
// **Returns:**
//   - []byte: Encoded entry (8-byte aligned)
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

	buf := make([]byte, paddedSize)
	binary.LittleEndian.PutUint32(buf[0:4], nextOffset)                                   // NextEntryOffset
	binary.LittleEndian.PutUint32(buf[4:8], uint32(entry.FileIndex))                      // FileIndex
	binary.LittleEndian.PutUint64(buf[8:16], types.TimeToFiletime(entry.CreationTime))    // CreationTime
	binary.LittleEndian.PutUint64(buf[16:24], types.TimeToFiletime(entry.LastAccessTime)) // LastAccessTime
	binary.LittleEndian.PutUint64(buf[24:32], types.TimeToFiletime(entry.LastWriteTime))  // LastWriteTime
	binary.LittleEndian.PutUint64(buf[32:40], types.TimeToFiletime(entry.ChangeTime))     // ChangeTime
	binary.LittleEndian.PutUint64(buf[40:48], entry.EndOfFile)                            // EndOfFile
	binary.LittleEndian.PutUint64(buf[48:56], entry.AllocationSize)                       // AllocationSize
	binary.LittleEndian.PutUint32(buf[56:60], uint32(entry.FileAttributes))               // FileAttributes
	binary.LittleEndian.PutUint32(buf[60:64], uint32(len(fileNameBytes)))                 // FileNameLength
	binary.LittleEndian.PutUint32(buf[64:68], entry.EaSize)                               // EaSize
	buf[68] = byte(len(shortNameBytes))                                                   // ShortNameLength
	buf[69] = 0                                                                           // Reserved1
	copy(buf[70:94], shortNameBytes)                                                      // ShortName (24 bytes max)
	binary.LittleEndian.PutUint16(buf[94:96], 0)                                          // Reserved2
	binary.LittleEndian.PutUint64(buf[96:104], entry.FileID)                              // FileId
	copy(buf[104:], fileNameBytes)                                                        // FileName

	return buf
}

// ============================================================================
// Protocol Handler
// ============================================================================

// QueryDirectory handles SMB2 QUERY_DIRECTORY command [MS-SMB2] 2.2.33, 2.2.34.
//
// QUERY_DIRECTORY enumerates files and subdirectories in a directory handle.
// Results can be filtered using a search pattern and returned in various formats.
//
// **Purpose:**
//
// The QUERY_DIRECTORY command allows clients to:
//   - List all files in a directory
//   - Search for files matching a pattern (e.g., "*.txt")
//   - Get detailed file information during enumeration
//   - Resume enumeration across multiple calls
//
// **Process:**
//
//  1. Decode and validate the request
//  2. Look up the open directory by FileID
//  3. Verify the handle is a directory
//  4. Handle enumeration state (restart, resume)
//  5. Read directory entries from metadata store
//  6. Filter entries based on search pattern
//  7. Build entries in the requested FileInfoClass format
//  8. Return the encoded response
//
// **Error Handling:**
//
// Returns appropriate SMB status codes:
//   - StatusInvalidParameter: Malformed request or not a directory
//   - StatusInvalidHandle: Invalid FileID
//   - StatusBadNetworkName: Share not found
//   - StatusAccessDenied: Permission denied
//   - StatusNoMoreFiles: Enumeration complete
//
// **Parameters:**
//   - ctx: SMB handler context with session information
//   - req: Parsed QUERY_DIRECTORY request
//
// **Returns:**
//   - *QueryDirectoryResponse: Response with directory entries
//   - error: Internal error (rare)
func (h *Handler) QueryDirectory(ctx *SMBHandlerContext, req *QueryDirectoryRequest) (*QueryDirectoryResponse, error) {
	logger.Debug("QUERY_DIRECTORY request",
		"fileInfoClass", req.FileInfoClass,
		"flags", req.Flags,
		"fileID", fmt.Sprintf("%x", req.FileID),
		"fileName", req.FileName)

	// ========================================================================
	// Step 1: Get OpenFile by FileID
	// ========================================================================

	openFile, ok := h.GetOpenFile(req.FileID)
	if !ok {
		logger.Debug("QUERY_DIRECTORY: invalid file ID", "fileID", fmt.Sprintf("%x", req.FileID))
		return &QueryDirectoryResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidHandle}}, nil
	}

	// Check if it's a directory
	if !openFile.IsDirectory {
		logger.Debug("QUERY_DIRECTORY: not a directory", "path", openFile.Path)
		return &QueryDirectoryResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidParameter}}, nil
	}

	// ========================================================================
	// Step 2: Get metadata store
	// ========================================================================

	metadataStore, err := h.Registry.GetMetadataStoreForShare(openFile.ShareName)
	if err != nil {
		logger.Warn("QUERY_DIRECTORY: failed to get metadata store", "share", openFile.ShareName, "error", err)
		return &QueryDirectoryResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusBadNetworkName}}, nil
	}

	// ========================================================================
	// Step 3: Build AuthContext
	// ========================================================================

	authCtx, err := BuildAuthContext(ctx, h.Registry)
	if err != nil {
		logger.Warn("QUERY_DIRECTORY: failed to build auth context", "error", err)
		return &QueryDirectoryResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusAccessDenied}}, nil
	}

	// ========================================================================
	// Step 4: Handle enumeration state
	// ========================================================================

	flags := types.QueryDirectoryFlags(req.Flags)
	restartScans := flags&types.SMB2RestartScans != 0

	// If enumeration is already complete and this is not a restart, return no more files
	if openFile.EnumerationComplete && !restartScans {
		return &QueryDirectoryResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusNoMoreFiles}}, nil
	}

	// Reset enumeration state on restart
	if restartScans {
		openFile.EnumerationComplete = false
		openFile.EnumerationIndex = 0
	}

	// ========================================================================
	// Step 5: Read directory entries from metadata store
	// ========================================================================

	page, err := metadataStore.ReadDirectory(authCtx, openFile.MetadataHandle, "", maxDirectoryReadBytes)
	if err != nil {
		logger.Debug("QUERY_DIRECTORY: failed to read directory", "path", openFile.Path, "error", err)
		return &QueryDirectoryResponse{SMBResponseBase: SMBResponseBase{Status: MetadataErrorToSMBStatus(err)}}, nil
	}

	// ========================================================================
	// Step 6: Filter entries based on filename pattern
	// ========================================================================

	filteredEntries := filterDirEntries(page.Entries, req.FileName)

	// ========================================================================
	// Step 7: Build directory entries based on info class
	// ========================================================================

	// For restart scans (first query), add . and .. ONLY when enumerating all files.
	// When searching for a specific file pattern (not "*"), don't include special entries.
	isWildcardSearch := req.FileName == "" || req.FileName == "*" || req.FileName == "*.*"
	includeSpecial := restartScans && isWildcardSearch

	// Convert entries
	var entries []byte
	fileInfoClass := types.FileInfoClass(req.FileInfoClass)

	switch fileInfoClass {
	case types.FileBothDirectoryInformation:
		entries = h.buildFileBothDirInfoFromStore(filteredEntries, includeSpecial)
	case types.FileIdBothDirectoryInformation:
		entries = h.buildFileIdBothDirInfoFromStore(filteredEntries, includeSpecial)
	case types.FileIdFullDirectoryInformation:
		entries = h.buildFileIdFullDirInfoFromStore(filteredEntries, includeSpecial)
	case types.FileFullDirectoryInformation:
		entries = h.buildFileFullDirInfoFromStore(filteredEntries, includeSpecial)
	case types.FileDirectoryInformation:
		entries = h.buildFileDirInfoFromStore(filteredEntries, includeSpecial)
	case types.FileNamesInformation:
		entries = h.buildFileNamesInfoFromStore(filteredEntries, includeSpecial)
	default:
		// Default to FileBothDirectoryInformation
		entries = h.buildFileBothDirInfoFromStore(filteredEntries, includeSpecial)
	}

	if len(entries) == 0 {
		return &QueryDirectoryResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusNoMoreFiles}}, nil
	}

	// Mark enumeration as complete - next call without restart will return NO_MORE_FILES
	openFile.EnumerationComplete = true
	h.StoreOpenFile(openFile)

	// Truncate if necessary
	if uint32(len(entries)) > req.OutputBufferLength {
		entries = entries[:req.OutputBufferLength]
	}

	logger.Debug("QUERY_DIRECTORY successful",
		"path", openFile.Path,
		"pattern", req.FileName,
		"totalEntries", len(page.Entries),
		"matchedEntries", len(filteredEntries),
		"bufferSize", len(entries))

	// ========================================================================
	// Step 8: Build success response
	// ========================================================================

	return &QueryDirectoryResponse{
		SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
		Data:            entries,
	}, nil
}

// ============================================================================
// Helper Functions - Directory Entry Building
// ============================================================================

// buildFileBothDirInfoFromStore builds FILE_BOTH_DIR_INFORMATION structures [MS-FSCC] 2.4.8.
func (h *Handler) buildFileBothDirInfoFromStore(entries []metadata.DirEntry, includeSpecial bool) []byte {
	var result []byte
	var prevNextOffset int
	var fileIndex uint64 = 1

	// Add special entries first
	if includeSpecial {
		result = h.appendBothDirEntryFromAttr(result, &prevNextOffset, ".", nil, fileIndex)
		fileIndex++
		result = h.appendBothDirEntryFromAttr(result, &prevNextOffset, "..", nil, fileIndex)
		fileIndex++
	}

	for i := range entries {
		result = h.appendBothDirEntryFromAttr(result, &prevNextOffset, entries[i].Name, entries[i].Attr, fileIndex)
		fileIndex++
	}

	return result
}

// appendBothDirEntryFromAttr appends a FILE_BOTH_DIR_INFORMATION entry from FileAttr.
func (h *Handler) appendBothDirEntryFromAttr(result []byte, prevNextOffset *int, name string, attr *metadata.FileAttr, fileIndex uint64) []byte {
	// Encode filename
	nameUTF16 := utf16.Encode([]rune(name))
	nameBytes := make([]byte, len(nameUTF16)*2)
	for i, r := range nameUTF16 {
		binary.LittleEndian.PutUint16(nameBytes[i*2:], r)
	}

	// FILE_BOTH_DIR_INFORMATION structure (94 bytes base + filename)
	// Align to 8-byte boundary
	totalLen := 94 + len(nameBytes)
	padding := (8 - (totalLen % 8)) % 8
	totalLen += padding

	entry := make([]byte, totalLen)

	// NextEntryOffset (will update later for previous entry)
	binary.LittleEndian.PutUint32(entry[0:4], 0)
	binary.LittleEndian.PutUint32(entry[4:8], uint32(fileIndex)) // FileIndex

	var creationTime, accessTime, writeTime, changeTime uint64
	var size uint64
	var attrs types.FileAttributes

	if attr != nil {
		creation, access, write, change := FileAttrToSMBTimes(attr)
		creationTime = types.TimeToFiletime(creation)
		accessTime = types.TimeToFiletime(access)
		writeTime = types.TimeToFiletime(write)
		changeTime = types.TimeToFiletime(change)
		size = getSMBSize(attr) // Use MFsymlink size for symlinks
		// Use FileAttrToSMBAttributesWithName to include Hidden attribute for dot-prefix files
		attrs = FileAttrToSMBAttributesWithName(attr, name)
	} else {
		// Special entries (. and ..)
		now := types.NowFiletime()
		creationTime = now
		accessTime = now
		writeTime = now
		changeTime = now
		attrs = types.FileAttributeDirectory
	}

	binary.LittleEndian.PutUint64(entry[8:16], creationTime)            // CreationTime
	binary.LittleEndian.PutUint64(entry[16:24], accessTime)             // LastAccessTime
	binary.LittleEndian.PutUint64(entry[24:32], writeTime)              // LastWriteTime
	binary.LittleEndian.PutUint64(entry[32:40], changeTime)             // ChangeTime
	binary.LittleEndian.PutUint64(entry[40:48], size)                   // EndOfFile
	binary.LittleEndian.PutUint64(entry[48:56], size)                   // AllocationSize
	binary.LittleEndian.PutUint32(entry[56:60], uint32(attrs))          // FileAttributes
	binary.LittleEndian.PutUint32(entry[60:64], uint32(len(nameBytes))) // FileNameLength
	binary.LittleEndian.PutUint32(entry[64:68], 0)                      // EaSize
	entry[68] = 0                                                       // ShortNameLength
	entry[69] = 0                                                       // Reserved
	// ShortName (24 bytes) - leave as zeros (positions 70-93)
	copy(entry[94:], nameBytes)

	// Update previous entry's NextEntryOffset to point to this entry
	if len(result) > 0 {
		binary.LittleEndian.PutUint32(result[*prevNextOffset:], uint32(len(result)-*prevNextOffset))
	}

	// Remember this entry's position for the next iteration
	*prevNextOffset = len(result)
	return append(result, entry...)
}

// buildFileIdBothDirInfoFromStore builds FILE_ID_BOTH_DIR_INFORMATION structures [MS-FSCC] 2.4.17.
func (h *Handler) buildFileIdBothDirInfoFromStore(entries []metadata.DirEntry, includeSpecial bool) []byte {
	var result []byte
	var prevNextOffset int
	var fileIndex uint64 = 1

	if includeSpecial {
		result = h.appendIdBothDirEntryFromAttr(result, &prevNextOffset, ".", nil, fileIndex, fileIndex)
		fileIndex++
		result = h.appendIdBothDirEntryFromAttr(result, &prevNextOffset, "..", nil, fileIndex, fileIndex)
		fileIndex++
	}

	for i := range entries {
		result = h.appendIdBothDirEntryFromAttr(result, &prevNextOffset, entries[i].Name, entries[i].Attr, fileIndex, entries[i].ID)
		fileIndex++
	}

	return result
}

// appendIdBothDirEntryFromAttr appends a FILE_ID_BOTH_DIR_INFORMATION entry from FileAttr.
func (h *Handler) appendIdBothDirEntryFromAttr(result []byte, prevNextOffset *int, name string, attr *metadata.FileAttr, fileIndex uint64, fileID uint64) []byte {
	nameUTF16 := utf16.Encode([]rune(name))
	nameBytes := make([]byte, len(nameUTF16)*2)
	for i, r := range nameUTF16 {
		binary.LittleEndian.PutUint16(nameBytes[i*2:], r)
	}

	// FILE_ID_BOTH_DIR_INFORMATION structure (104 bytes base + filename)
	totalLen := 104 + len(nameBytes)
	padding := (8 - (totalLen % 8)) % 8
	totalLen += padding

	entry := make([]byte, totalLen)

	binary.LittleEndian.PutUint32(entry[0:4], 0)
	binary.LittleEndian.PutUint32(entry[4:8], uint32(fileIndex)) // FileIndex

	var creationTime, accessTime, writeTime, changeTime uint64
	var size uint64
	var attrs types.FileAttributes

	if attr != nil {
		creation, access, write, change := FileAttrToSMBTimes(attr)
		creationTime = types.TimeToFiletime(creation)
		accessTime = types.TimeToFiletime(access)
		writeTime = types.TimeToFiletime(write)
		changeTime = types.TimeToFiletime(change)
		size = getSMBSize(attr) // Use MFsymlink size for symlinks
		// Use FileAttrToSMBAttributesWithName to include Hidden attribute for dot-prefix files
		attrs = FileAttrToSMBAttributesWithName(attr, name)
	} else {
		now := types.NowFiletime()
		creationTime = now
		accessTime = now
		writeTime = now
		changeTime = now
		attrs = types.FileAttributeDirectory
	}

	binary.LittleEndian.PutUint64(entry[8:16], creationTime)
	binary.LittleEndian.PutUint64(entry[16:24], accessTime)
	binary.LittleEndian.PutUint64(entry[24:32], writeTime)
	binary.LittleEndian.PutUint64(entry[32:40], changeTime)
	binary.LittleEndian.PutUint64(entry[40:48], size)
	binary.LittleEndian.PutUint64(entry[48:56], size)
	binary.LittleEndian.PutUint32(entry[56:60], uint32(attrs))
	binary.LittleEndian.PutUint32(entry[60:64], uint32(len(nameBytes)))
	binary.LittleEndian.PutUint32(entry[64:68], 0) // EaSize
	entry[68] = 0                                  // ShortNameLength
	entry[69] = 0                                  // Reserved1
	// ShortName (24 bytes) - positions 70-93
	binary.LittleEndian.PutUint16(entry[94:96], 0)       // Reserved2
	binary.LittleEndian.PutUint64(entry[96:104], fileID) // FileId
	copy(entry[104:], nameBytes)

	// Update previous entry's NextEntryOffset to point to this entry
	if len(result) > 0 {
		binary.LittleEndian.PutUint32(result[*prevNextOffset:], uint32(len(result)-*prevNextOffset))
	}

	// Remember this entry's position for the next iteration
	*prevNextOffset = len(result)
	return append(result, entry...)
}

// buildFileIdFullDirInfoFromStore builds FILE_ID_FULL_DIR_INFORMATION structures [MS-FSCC] 2.4.18.
func (h *Handler) buildFileIdFullDirInfoFromStore(entries []metadata.DirEntry, includeSpecial bool) []byte {
	var result []byte
	var prevNextOffset int
	var fileIndex uint64 = 1

	if includeSpecial {
		result = h.appendIdFullDirEntryFromAttr(result, &prevNextOffset, ".", nil, fileIndex, fileIndex)
		fileIndex++
		result = h.appendIdFullDirEntryFromAttr(result, &prevNextOffset, "..", nil, fileIndex, fileIndex)
		fileIndex++
	}

	for i := range entries {
		result = h.appendIdFullDirEntryFromAttr(result, &prevNextOffset, entries[i].Name, entries[i].Attr, fileIndex, entries[i].ID)
		fileIndex++
	}

	return result
}

// appendIdFullDirEntryFromAttr appends a FILE_ID_FULL_DIR_INFORMATION entry from FileAttr.
func (h *Handler) appendIdFullDirEntryFromAttr(result []byte, prevNextOffset *int, name string, attr *metadata.FileAttr, fileIndex uint64, fileID uint64) []byte {
	nameUTF16 := utf16.Encode([]rune(name))
	nameBytes := make([]byte, len(nameUTF16)*2)
	for i, r := range nameUTF16 {
		binary.LittleEndian.PutUint16(nameBytes[i*2:], r)
	}

	// FILE_ID_FULL_DIR_INFORMATION structure (80 bytes base + filename)
	// [MS-FSCC] 2.4.18
	totalLen := 80 + len(nameBytes)
	padding := (8 - (totalLen % 8)) % 8
	totalLen += padding

	entry := make([]byte, totalLen)

	binary.LittleEndian.PutUint32(entry[0:4], 0)                 // NextEntryOffset (set later)
	binary.LittleEndian.PutUint32(entry[4:8], uint32(fileIndex)) // FileIndex

	var creationTime, accessTime, writeTime, changeTime uint64
	var size uint64
	var attrs types.FileAttributes

	if attr != nil {
		creation, access, write, change := FileAttrToSMBTimes(attr)
		creationTime = types.TimeToFiletime(creation)
		accessTime = types.TimeToFiletime(access)
		writeTime = types.TimeToFiletime(write)
		changeTime = types.TimeToFiletime(change)
		size = getSMBSize(attr)
		attrs = FileAttrToSMBAttributesWithName(attr, name)
	} else {
		now := types.NowFiletime()
		creationTime = now
		accessTime = now
		writeTime = now
		changeTime = now
		attrs = types.FileAttributeDirectory
	}

	binary.LittleEndian.PutUint64(entry[8:16], creationTime)
	binary.LittleEndian.PutUint64(entry[16:24], accessTime)
	binary.LittleEndian.PutUint64(entry[24:32], writeTime)
	binary.LittleEndian.PutUint64(entry[32:40], changeTime)
	binary.LittleEndian.PutUint64(entry[40:48], size)                   // EndOfFile
	binary.LittleEndian.PutUint64(entry[48:56], size)                   // AllocationSize
	binary.LittleEndian.PutUint32(entry[56:60], uint32(attrs))          // FileAttributes
	binary.LittleEndian.PutUint32(entry[60:64], uint32(len(nameBytes))) // FileNameLength
	binary.LittleEndian.PutUint32(entry[64:68], 0)                      // EaSize
	binary.LittleEndian.PutUint32(entry[68:72], 0)                      // Reserved
	binary.LittleEndian.PutUint64(entry[72:80], fileID)                 // FileId
	copy(entry[80:], nameBytes)                                         // FileName

	// Update previous entry's NextEntryOffset to point to this entry
	if len(result) > 0 {
		binary.LittleEndian.PutUint32(result[*prevNextOffset:], uint32(len(result)-*prevNextOffset))
	}

	// Remember this entry's position for the next iteration
	*prevNextOffset = len(result)
	return append(result, entry...)
}

// buildFileFullDirInfoFromStore builds FILE_FULL_DIR_INFORMATION structures [MS-FSCC] 2.4.14.
func (h *Handler) buildFileFullDirInfoFromStore(entries []metadata.DirEntry, includeSpecial bool) []byte {
	var result []byte
	var prevNextOffset int
	var fileIndex uint64 = 1

	if includeSpecial {
		result = h.appendFullDirEntryFromAttr(result, &prevNextOffset, ".", nil, fileIndex)
		fileIndex++
		result = h.appendFullDirEntryFromAttr(result, &prevNextOffset, "..", nil, fileIndex)
		fileIndex++
	}

	for i := range entries {
		result = h.appendFullDirEntryFromAttr(result, &prevNextOffset, entries[i].Name, entries[i].Attr, fileIndex)
		fileIndex++
	}

	return result
}

// appendFullDirEntryFromAttr appends a FILE_FULL_DIR_INFORMATION entry from FileAttr.
func (h *Handler) appendFullDirEntryFromAttr(result []byte, prevNextOffset *int, name string, attr *metadata.FileAttr, fileIndex uint64) []byte {
	nameUTF16 := utf16.Encode([]rune(name))
	nameBytes := make([]byte, len(nameUTF16)*2)
	for i, r := range nameUTF16 {
		binary.LittleEndian.PutUint16(nameBytes[i*2:], r)
	}

	// FILE_FULL_DIR_INFORMATION structure (68 bytes base + filename)
	totalLen := 68 + len(nameBytes)
	padding := (8 - (totalLen % 8)) % 8
	totalLen += padding

	entry := make([]byte, totalLen)

	binary.LittleEndian.PutUint32(entry[0:4], 0)
	binary.LittleEndian.PutUint32(entry[4:8], uint32(fileIndex))

	var creationTime, accessTime, writeTime, changeTime uint64
	var size uint64
	var attrs types.FileAttributes

	if attr != nil {
		creation, access, write, change := FileAttrToSMBTimes(attr)
		creationTime = types.TimeToFiletime(creation)
		accessTime = types.TimeToFiletime(access)
		writeTime = types.TimeToFiletime(write)
		changeTime = types.TimeToFiletime(change)
		size = getSMBSize(attr) // Use MFsymlink size for symlinks
		// Use FileAttrToSMBAttributesWithName to include Hidden attribute for dot-prefix files
		attrs = FileAttrToSMBAttributesWithName(attr, name)
	} else {
		now := types.NowFiletime()
		creationTime = now
		accessTime = now
		writeTime = now
		changeTime = now
		attrs = types.FileAttributeDirectory
	}

	binary.LittleEndian.PutUint64(entry[8:16], creationTime)
	binary.LittleEndian.PutUint64(entry[16:24], accessTime)
	binary.LittleEndian.PutUint64(entry[24:32], writeTime)
	binary.LittleEndian.PutUint64(entry[32:40], changeTime)
	binary.LittleEndian.PutUint64(entry[40:48], size)
	binary.LittleEndian.PutUint64(entry[48:56], size)
	binary.LittleEndian.PutUint32(entry[56:60], uint32(attrs))
	binary.LittleEndian.PutUint32(entry[60:64], uint32(len(nameBytes)))
	binary.LittleEndian.PutUint32(entry[64:68], 0) // EaSize
	copy(entry[68:], nameBytes)

	// Update previous entry's NextEntryOffset to point to this entry
	if len(result) > 0 {
		binary.LittleEndian.PutUint32(result[*prevNextOffset:], uint32(len(result)-*prevNextOffset))
	}

	// Remember this entry's position for the next iteration
	*prevNextOffset = len(result)
	return append(result, entry...)
}

// buildFileDirInfoFromStore builds FILE_DIRECTORY_INFORMATION structures [MS-FSCC] 2.4.10.
func (h *Handler) buildFileDirInfoFromStore(entries []metadata.DirEntry, includeSpecial bool) []byte {
	var result []byte
	var prevNextOffset int
	var fileIndex uint64 = 1

	if includeSpecial {
		result = h.appendDirEntryFromAttr(result, &prevNextOffset, ".", nil, fileIndex)
		fileIndex++
		result = h.appendDirEntryFromAttr(result, &prevNextOffset, "..", nil, fileIndex)
		fileIndex++
	}

	for i := range entries {
		result = h.appendDirEntryFromAttr(result, &prevNextOffset, entries[i].Name, entries[i].Attr, fileIndex)
		fileIndex++
	}

	return result
}

// appendDirEntryFromAttr appends a FILE_DIRECTORY_INFORMATION entry from FileAttr.
func (h *Handler) appendDirEntryFromAttr(result []byte, prevNextOffset *int, name string, attr *metadata.FileAttr, fileIndex uint64) []byte {
	nameUTF16 := utf16.Encode([]rune(name))
	nameBytes := make([]byte, len(nameUTF16)*2)
	for i, r := range nameUTF16 {
		binary.LittleEndian.PutUint16(nameBytes[i*2:], r)
	}

	// FILE_DIRECTORY_INFORMATION structure (64 bytes base + filename)
	totalLen := 64 + len(nameBytes)
	padding := (8 - (totalLen % 8)) % 8
	totalLen += padding

	entry := make([]byte, totalLen)

	binary.LittleEndian.PutUint32(entry[0:4], 0)
	binary.LittleEndian.PutUint32(entry[4:8], uint32(fileIndex))

	var creationTime, accessTime, writeTime, changeTime uint64
	var size uint64
	var attrs types.FileAttributes

	if attr != nil {
		creation, access, write, change := FileAttrToSMBTimes(attr)
		creationTime = types.TimeToFiletime(creation)
		accessTime = types.TimeToFiletime(access)
		writeTime = types.TimeToFiletime(write)
		changeTime = types.TimeToFiletime(change)
		size = getSMBSize(attr) // Use MFsymlink size for symlinks
		// Use FileAttrToSMBAttributesWithName to include Hidden attribute for dot-prefix files
		attrs = FileAttrToSMBAttributesWithName(attr, name)
	} else {
		now := types.NowFiletime()
		creationTime = now
		accessTime = now
		writeTime = now
		changeTime = now
		attrs = types.FileAttributeDirectory
	}

	binary.LittleEndian.PutUint64(entry[8:16], creationTime)
	binary.LittleEndian.PutUint64(entry[16:24], accessTime)
	binary.LittleEndian.PutUint64(entry[24:32], writeTime)
	binary.LittleEndian.PutUint64(entry[32:40], changeTime)
	binary.LittleEndian.PutUint64(entry[40:48], size)
	binary.LittleEndian.PutUint64(entry[48:56], size)
	binary.LittleEndian.PutUint32(entry[56:60], uint32(attrs))
	binary.LittleEndian.PutUint32(entry[60:64], uint32(len(nameBytes)))
	copy(entry[64:], nameBytes)

	// Update previous entry's NextEntryOffset to point to this entry
	if len(result) > 0 {
		binary.LittleEndian.PutUint32(result[*prevNextOffset:], uint32(len(result)-*prevNextOffset))
	}

	// Remember this entry's position for the next iteration
	*prevNextOffset = len(result)
	return append(result, entry...)
}

// buildFileNamesInfoFromStore builds FILE_NAMES_INFORMATION structures [MS-FSCC] 2.4.26.
func (h *Handler) buildFileNamesInfoFromStore(entries []metadata.DirEntry, includeSpecial bool) []byte {
	var result []byte
	var prevNextOffset int
	var fileIndex uint64 = 1

	if includeSpecial {
		result = h.appendNamesEntryFromStore(result, &prevNextOffset, ".", fileIndex)
		fileIndex++
		result = h.appendNamesEntryFromStore(result, &prevNextOffset, "..", fileIndex)
		fileIndex++
	}

	for i := range entries {
		result = h.appendNamesEntryFromStore(result, &prevNextOffset, entries[i].Name, fileIndex)
		fileIndex++
	}

	return result
}

// appendNamesEntryFromStore appends a FILE_NAMES_INFORMATION entry.
func (h *Handler) appendNamesEntryFromStore(result []byte, prevNextOffset *int, name string, fileIndex uint64) []byte {
	nameUTF16 := utf16.Encode([]rune(name))
	nameBytes := make([]byte, len(nameUTF16)*2)
	for i, r := range nameUTF16 {
		binary.LittleEndian.PutUint16(nameBytes[i*2:], r)
	}

	// FILE_NAMES_INFORMATION structure (12 bytes base + filename)
	totalLen := 12 + len(nameBytes)
	padding := (8 - (totalLen % 8)) % 8
	totalLen += padding

	entry := make([]byte, totalLen)

	binary.LittleEndian.PutUint32(entry[0:4], 0)
	binary.LittleEndian.PutUint32(entry[4:8], uint32(fileIndex)) // FileIndex
	binary.LittleEndian.PutUint32(entry[8:12], uint32(len(nameBytes)))
	copy(entry[12:], nameBytes)

	// Update previous entry's NextEntryOffset to point to this entry
	if len(result) > 0 {
		binary.LittleEndian.PutUint32(result[*prevNextOffset:], uint32(len(result)-*prevNextOffset))
	}

	// Remember this entry's position for the next iteration
	*prevNextOffset = len(result)
	return append(result, entry...)
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

	// Check if pattern matches all
	matchAll := pattern == "" || pattern == "*" || pattern == "<" || pattern == "*.*"

	for _, entry := range entries {
		// Skip Unix special files (FIFO, socket, block/char device) - they have no SMB equivalent
		if entry.Attr != nil && IsSpecialFile(entry.Attr.Type) {
			continue
		}

		// Include entry if pattern matches all or matches the name
		if matchAll || matchSMBPattern(entry.Name, pattern) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

// matchSMBPattern matches a filename against an SMB search pattern.
//
// SMB uses DOS-style wildcards:
//   - * matches zero or more characters
//   - ? matches exactly one character
//
// Matching is case-insensitive (Windows behavior).
func matchSMBPattern(name, pattern string) bool {
	// Case-insensitive comparison (SMB/Windows style)
	nameLower := strings.ToLower(name)
	patternLower := strings.ToLower(pattern)

	// Use filepath.Match for basic wildcard support
	// Note: filepath.Match uses Unix-style patterns, which is close enough for most cases
	matched, err := filepath.Match(patternLower, nameLower)
	if err != nil {
		// Invalid pattern - fall back to exact match
		return nameLower == patternLower
	}
	return matched
}

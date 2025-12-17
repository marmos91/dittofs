package handlers

import (
	"encoding/binary"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf16"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/smb/types"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// QueryDirectory handles SMB2 QUERY_DIRECTORY command [MS-SMB2] 2.2.33, 2.2.34
func (h *Handler) QueryDirectory(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	// ========================================================================
	// Step 1: Decode request
	// ========================================================================

	req, err := DecodeQueryDirectoryRequest(body)
	if err != nil {
		logger.Debug("QUERY_DIRECTORY: failed to decode request", "error", err)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	logger.Debug("QUERY_DIRECTORY request",
		"fileInfoClass", req.FileInfoClass,
		"flags", req.Flags,
		"fileID", fmt.Sprintf("%x", req.FileID),
		"fileName", req.FileName)

	// ========================================================================
	// Step 2: Get OpenFile by FileID
	// ========================================================================

	openFile, ok := h.GetOpenFile(req.FileID)
	if !ok {
		logger.Debug("QUERY_DIRECTORY: invalid file ID", "fileID", fmt.Sprintf("%x", req.FileID))
		return NewErrorResult(types.StatusInvalidHandle), nil
	}

	// Check if it's a directory
	if !openFile.IsDirectory {
		logger.Debug("QUERY_DIRECTORY: not a directory", "path", openFile.Path)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// ========================================================================
	// Step 3: Get metadata store
	// ========================================================================

	metadataStore, err := h.Registry.GetMetadataStoreForShare(openFile.ShareName)
	if err != nil {
		logger.Warn("QUERY_DIRECTORY: failed to get metadata store", "share", openFile.ShareName, "error", err)
		return NewErrorResult(types.StatusBadNetworkName), nil
	}

	// ========================================================================
	// Step 4: Build AuthContext
	// ========================================================================

	authCtx, err := BuildAuthContext(ctx, h.Registry)
	if err != nil {
		logger.Warn("QUERY_DIRECTORY: failed to build auth context", "error", err)
		return NewErrorResult(types.StatusAccessDenied), nil
	}

	// ========================================================================
	// Step 5: Handle enumeration state
	// ========================================================================

	flags := types.QueryDirectoryFlags(req.Flags)
	restartScans := flags&types.SMB2RestartScans != 0

	// If enumeration is already complete and this is not a restart, return no more files
	if openFile.EnumerationComplete && !restartScans {
		return NewErrorResult(types.StatusNoMoreFiles), nil
	}

	// Reset enumeration state on restart
	if restartScans {
		openFile.EnumerationComplete = false
		openFile.EnumerationIndex = 0
	}

	// ========================================================================
	// Step 6: Read directory entries from metadata store
	// ========================================================================

	// Use a reasonable max size for directory entries
	const maxBytes uint32 = 65536

	page, err := metadataStore.ReadDirectory(authCtx, openFile.MetadataHandle, "", maxBytes)
	if err != nil {
		logger.Debug("QUERY_DIRECTORY: failed to read directory", "path", openFile.Path, "error", err)
		return NewErrorResult(MetadataErrorToSMBStatus(err)), nil
	}

	// ========================================================================
	// Step 7: Filter entries based on filename pattern
	// ========================================================================

	filteredEntries := filterDirEntries(page.Entries, req.FileName)

	// ========================================================================
	// Step 8: Build directory entries based on info class
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
		return NewErrorResult(types.StatusNoMoreFiles), nil
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
	// Step 8: Build and encode response
	// ========================================================================

	resp := &QueryDirectoryResponse{
		OutputBufferOffset: 72, // 64 (header) + 8 (response header start)
		OutputBufferLength: uint32(len(entries)),
		Data:               entries,
	}

	respBytes, err := EncodeQueryDirectoryResponse(resp)
	if err != nil {
		logger.Warn("QUERY_DIRECTORY: failed to encode response", "error", err)
		return NewErrorResult(types.StatusInternalError), nil
	}

	return NewResult(types.StatusSuccess, respBytes), nil
}

// buildFileBothDirInfoFromStore builds FILE_BOTH_DIR_INFORMATION structures [MS-FSCC] 2.4.8
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

// appendBothDirEntryFromAttr appends a FILE_BOTH_DIR_INFORMATION entry from FileAttr
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

// buildFileIdBothDirInfoFromStore builds FILE_ID_BOTH_DIR_INFORMATION structures [MS-FSCC] 2.4.17
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

// appendIdBothDirEntryFromAttr appends a FILE_ID_BOTH_DIR_INFORMATION entry from FileAttr
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

// buildFileFullDirInfoFromStore builds FILE_FULL_DIR_INFORMATION structures [MS-FSCC] 2.4.14
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

// appendFullDirEntryFromAttr appends a FILE_FULL_DIR_INFORMATION entry from FileAttr
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

// buildFileDirInfoFromStore builds FILE_DIRECTORY_INFORMATION structures [MS-FSCC] 2.4.10
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

// appendDirEntryFromAttr appends a FILE_DIRECTORY_INFORMATION entry from FileAttr
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

// buildFileNamesInfoFromStore builds FILE_NAMES_INFORMATION structures [MS-FSCC] 2.4.26
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

// appendNamesEntryFromStore appends a FILE_NAMES_INFORMATION entry
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

// filterDirEntries filters directory entries based on the SMB2 search pattern.
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

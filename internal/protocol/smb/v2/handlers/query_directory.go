package handlers

import (
	"encoding/binary"
	"unicode/utf16"

	"github.com/marmos91/dittofs/internal/protocol/smb/types"
)

// QueryDirectory handles SMB2 QUERY_DIRECTORY command [MS-SMB2] 2.2.33, 2.2.34
func (h *Handler) QueryDirectory(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	if len(body) < 33 {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Parse request [MS-SMB2] 2.2.33
	// structureSize := binary.LittleEndian.Uint16(body[0:2]) // Always 33
	fileInfoClass := types.FileInfoClass(body[2])
	flags := types.QueryDirectoryFlags(body[3])
	// fileIndex := binary.LittleEndian.Uint32(body[4:8])
	var fileID [16]byte
	copy(fileID[:], body[8:24])
	// fileNameOffset := binary.LittleEndian.Uint16(body[24:26])
	// fileNameLength := binary.LittleEndian.Uint16(body[26:28])
	outputBufferLength := binary.LittleEndian.Uint32(body[28:32])

	// Validate file handle
	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		return NewErrorResult(types.StatusInvalidHandle), nil
	}

	// Check if it's a directory
	if !openFile.IsDirectory {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Check for restart scans flag (first query or explicit restart)
	restartScans := flags&types.SMB2RestartScans != 0

	// If enumeration is already complete and this is not a restart, return no more files
	if openFile.EnumerationComplete && !restartScans {
		return NewErrorResult(types.StatusNoMoreFiles), nil
	}

	// Reset enumeration state on restart
	if restartScans {
		openFile.EnumerationComplete = false
	}

	// Get mock directory contents
	files := h.GetMockFiles(openFile.ShareName, openFile.Path)

	// For restart scans (first query), add . and ..
	includeSpecial := restartScans

	// Build directory entries based on info class
	var entries []byte
	switch fileInfoClass {
	case types.FileBothDirectoryInformation:
		entries = h.buildFileBothDirectoryInfo(files, includeSpecial)
	case types.FileIdBothDirectoryInformation:
		entries = h.buildFileIdBothDirectoryInfo(files, includeSpecial)
	case types.FileFullDirectoryInformation:
		entries = h.buildFileFullDirectoryInfo(files, includeSpecial)
	case types.FileDirectoryInformation:
		entries = h.buildFileDirectoryInfo(files, includeSpecial)
	case types.FileNamesInformation:
		entries = h.buildFileNamesInfo(files, includeSpecial)
	default:
		// Default to FileBothDirectoryInformation
		entries = h.buildFileBothDirectoryInfo(files, includeSpecial)
	}

	if len(entries) == 0 {
		return NewErrorResult(types.StatusNoMoreFiles), nil
	}

	// Mark enumeration as complete - next call without restart will return NO_MORE_FILES
	openFile.EnumerationComplete = true
	h.StoreOpenFile(openFile)

	// Truncate if necessary
	if uint32(len(entries)) > outputBufferLength {
		entries = entries[:outputBufferLength]
	}

	// Build response [MS-SMB2] 2.2.34 (9 bytes header + entries)
	resp := make([]byte, 8+len(entries))
	binary.LittleEndian.PutUint16(resp[0:2], 9)                    // StructureSize
	binary.LittleEndian.PutUint16(resp[2:4], 72)                   // OutputBufferOffset (64 + 8 = header + response struct)
	binary.LittleEndian.PutUint32(resp[4:8], uint32(len(entries))) // OutputBufferLength
	copy(resp[8:], entries)

	return NewResult(types.StatusSuccess, resp), nil
}

// buildFileBothDirectoryInfo builds FILE_BOTH_DIR_INFORMATION structures [MS-FSCC] 2.4.8
func (h *Handler) buildFileBothDirectoryInfo(files map[string]*MockFile, includeSpecial bool) []byte {
	var result []byte
	var prevNextOffset int

	// Add special entries first
	if includeSpecial {
		result = h.appendBothDirEntry(result, &prevNextOffset, ".", true, 0, uint32(types.FileAttributeDirectory))
		result = h.appendBothDirEntry(result, &prevNextOffset, "..", true, 0, uint32(types.FileAttributeDirectory))
	}

	for _, f := range files {
		result = h.appendBothDirEntry(result, &prevNextOffset, f.Name, f.IsDir, f.Size, f.Attributes)
	}

	return result
}

// appendBothDirEntry appends a FILE_BOTH_DIR_INFORMATION entry
func (h *Handler) appendBothDirEntry(result []byte, prevNextOffset *int, name string, isDir bool, size int64, attrs uint32) []byte {
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
	binary.LittleEndian.PutUint32(entry[4:8], 0) // FileIndex

	now := types.NowFiletime()
	binary.LittleEndian.PutUint64(entry[8:16], now)                     // CreationTime
	binary.LittleEndian.PutUint64(entry[16:24], now)                    // LastAccessTime
	binary.LittleEndian.PutUint64(entry[24:32], now)                    // LastWriteTime
	binary.LittleEndian.PutUint64(entry[32:40], now)                    // ChangeTime
	binary.LittleEndian.PutUint64(entry[40:48], uint64(size))           // EndOfFile
	binary.LittleEndian.PutUint64(entry[48:56], uint64(size))           // AllocationSize
	binary.LittleEndian.PutUint32(entry[56:60], attrs)                  // FileAttributes
	binary.LittleEndian.PutUint32(entry[60:64], uint32(len(nameBytes))) // FileNameLength
	binary.LittleEndian.PutUint32(entry[64:68], 0)                      // EaSize
	entry[68] = 0                                                       // ShortNameLength
	entry[69] = 0                                                       // Reserved
	// ShortName (24 bytes) - leave as zeros (positions 70-93)
	copy(entry[94:], nameBytes)

	// Update previous entry's NextEntryOffset
	if *prevNextOffset > 0 {
		binary.LittleEndian.PutUint32(result[*prevNextOffset:], uint32(len(result)-*prevNextOffset))
	}

	*prevNextOffset = len(result)
	return append(result, entry...)
}

// buildFileIdBothDirectoryInfo builds FILE_ID_BOTH_DIR_INFORMATION structures [MS-FSCC] 2.4.17
func (h *Handler) buildFileIdBothDirectoryInfo(files map[string]*MockFile, includeSpecial bool) []byte {
	var result []byte
	var prevNextOffset int
	var fileIndex uint64 = 1

	if includeSpecial {
		result = h.appendIdBothDirEntry(result, &prevNextOffset, ".", true, 0, uint32(types.FileAttributeDirectory), fileIndex)
		fileIndex++
		result = h.appendIdBothDirEntry(result, &prevNextOffset, "..", true, 0, uint32(types.FileAttributeDirectory), fileIndex)
		fileIndex++
	}

	for _, f := range files {
		result = h.appendIdBothDirEntry(result, &prevNextOffset, f.Name, f.IsDir, f.Size, f.Attributes, fileIndex)
		fileIndex++
	}

	return result
}

// appendIdBothDirEntry appends a FILE_ID_BOTH_DIR_INFORMATION entry
func (h *Handler) appendIdBothDirEntry(result []byte, prevNextOffset *int, name string, isDir bool, size int64, attrs uint32, fileID uint64) []byte {
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
	binary.LittleEndian.PutUint32(entry[4:8], 0) // FileIndex

	now := types.NowFiletime()
	binary.LittleEndian.PutUint64(entry[8:16], now)
	binary.LittleEndian.PutUint64(entry[16:24], now)
	binary.LittleEndian.PutUint64(entry[24:32], now)
	binary.LittleEndian.PutUint64(entry[32:40], now)
	binary.LittleEndian.PutUint64(entry[40:48], uint64(size))
	binary.LittleEndian.PutUint64(entry[48:56], uint64(size))
	binary.LittleEndian.PutUint32(entry[56:60], attrs)
	binary.LittleEndian.PutUint32(entry[60:64], uint32(len(nameBytes)))
	binary.LittleEndian.PutUint32(entry[64:68], 0) // EaSize
	entry[68] = 0                                  // ShortNameLength
	entry[69] = 0                                  // Reserved1
	// ShortName (24 bytes) - positions 70-93
	binary.LittleEndian.PutUint16(entry[94:96], 0)       // Reserved2
	binary.LittleEndian.PutUint64(entry[96:104], fileID) // FileId
	copy(entry[104:], nameBytes)

	if *prevNextOffset > 0 {
		binary.LittleEndian.PutUint32(result[*prevNextOffset:], uint32(len(result)-*prevNextOffset))
	}

	*prevNextOffset = len(result)
	return append(result, entry...)
}

// buildFileFullDirectoryInfo builds FILE_FULL_DIR_INFORMATION structures [MS-FSCC] 2.4.14
func (h *Handler) buildFileFullDirectoryInfo(files map[string]*MockFile, includeSpecial bool) []byte {
	var result []byte
	var prevNextOffset int

	if includeSpecial {
		result = h.appendFullDirEntry(result, &prevNextOffset, ".", true, 0, uint32(types.FileAttributeDirectory))
		result = h.appendFullDirEntry(result, &prevNextOffset, "..", true, 0, uint32(types.FileAttributeDirectory))
	}

	for _, f := range files {
		result = h.appendFullDirEntry(result, &prevNextOffset, f.Name, f.IsDir, f.Size, f.Attributes)
	}

	return result
}

// appendFullDirEntry appends a FILE_FULL_DIR_INFORMATION entry
func (h *Handler) appendFullDirEntry(result []byte, prevNextOffset *int, name string, isDir bool, size int64, attrs uint32) []byte {
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
	binary.LittleEndian.PutUint32(entry[4:8], 0)

	now := types.NowFiletime()
	binary.LittleEndian.PutUint64(entry[8:16], now)
	binary.LittleEndian.PutUint64(entry[16:24], now)
	binary.LittleEndian.PutUint64(entry[24:32], now)
	binary.LittleEndian.PutUint64(entry[32:40], now)
	binary.LittleEndian.PutUint64(entry[40:48], uint64(size))
	binary.LittleEndian.PutUint64(entry[48:56], uint64(size))
	binary.LittleEndian.PutUint32(entry[56:60], attrs)
	binary.LittleEndian.PutUint32(entry[60:64], uint32(len(nameBytes)))
	binary.LittleEndian.PutUint32(entry[64:68], 0) // EaSize
	copy(entry[68:], nameBytes)

	if *prevNextOffset > 0 {
		binary.LittleEndian.PutUint32(result[*prevNextOffset:], uint32(len(result)-*prevNextOffset))
	}

	*prevNextOffset = len(result)
	return append(result, entry...)
}

// buildFileDirectoryInfo builds FILE_DIRECTORY_INFORMATION structures [MS-FSCC] 2.4.10
func (h *Handler) buildFileDirectoryInfo(files map[string]*MockFile, includeSpecial bool) []byte {
	var result []byte
	var prevNextOffset int

	if includeSpecial {
		result = h.appendDirEntry(result, &prevNextOffset, ".", true, 0, uint32(types.FileAttributeDirectory))
		result = h.appendDirEntry(result, &prevNextOffset, "..", true, 0, uint32(types.FileAttributeDirectory))
	}

	for _, f := range files {
		result = h.appendDirEntry(result, &prevNextOffset, f.Name, f.IsDir, f.Size, f.Attributes)
	}

	return result
}

// appendDirEntry appends a FILE_DIRECTORY_INFORMATION entry
func (h *Handler) appendDirEntry(result []byte, prevNextOffset *int, name string, isDir bool, size int64, attrs uint32) []byte {
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
	binary.LittleEndian.PutUint32(entry[4:8], 0)

	now := types.NowFiletime()
	binary.LittleEndian.PutUint64(entry[8:16], now)
	binary.LittleEndian.PutUint64(entry[16:24], now)
	binary.LittleEndian.PutUint64(entry[24:32], now)
	binary.LittleEndian.PutUint64(entry[32:40], now)
	binary.LittleEndian.PutUint64(entry[40:48], uint64(size))
	binary.LittleEndian.PutUint64(entry[48:56], uint64(size))
	binary.LittleEndian.PutUint32(entry[56:60], attrs)
	binary.LittleEndian.PutUint32(entry[60:64], uint32(len(nameBytes)))
	copy(entry[64:], nameBytes)

	if *prevNextOffset > 0 {
		binary.LittleEndian.PutUint32(result[*prevNextOffset:], uint32(len(result)-*prevNextOffset))
	}

	*prevNextOffset = len(result)
	return append(result, entry...)
}

// buildFileNamesInfo builds FILE_NAMES_INFORMATION structures [MS-FSCC] 2.4.26
func (h *Handler) buildFileNamesInfo(files map[string]*MockFile, includeSpecial bool) []byte {
	var result []byte
	var prevNextOffset int

	if includeSpecial {
		result = h.appendNamesEntry(result, &prevNextOffset, ".")
		result = h.appendNamesEntry(result, &prevNextOffset, "..")
	}

	for _, f := range files {
		result = h.appendNamesEntry(result, &prevNextOffset, f.Name)
	}

	return result
}

// appendNamesEntry appends a FILE_NAMES_INFORMATION entry
func (h *Handler) appendNamesEntry(result []byte, prevNextOffset *int, name string) []byte {
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
	binary.LittleEndian.PutUint32(entry[4:8], 0) // FileIndex
	binary.LittleEndian.PutUint32(entry[8:12], uint32(len(nameBytes)))
	copy(entry[12:], nameBytes)

	if *prevNextOffset > 0 {
		binary.LittleEndian.PutUint32(result[*prevNextOffset:], uint32(len(result)-*prevNextOffset))
	}

	*prevNextOffset = len(result)
	return append(result, entry...)
}

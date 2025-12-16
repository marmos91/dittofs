// Package handlers provides SMB2 command handlers and session management.
package handlers

import (
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/internal/protocol/smb/types"
)

// DecodeCreateRequest parses an SMB2 CREATE request body [MS-SMB2] 2.2.13
// CREATE request structure (56 bytes fixed part):
//   - StructureSize (2 bytes) - always 57
//   - SecurityFlags (1 byte)
//   - RequestedOplockLevel (1 byte)
//   - ImpersonationLevel (4 bytes)
//   - SmbCreateFlags (8 bytes)
//   - Reserved (8 bytes)
//   - DesiredAccess (4 bytes)
//   - FileAttributes (4 bytes)
//   - ShareAccess (4 bytes)
//   - CreateDisposition (4 bytes)
//   - CreateOptions (4 bytes)
//   - NameOffset (2 bytes) - offset from header start to filename
//   - NameLength (2 bytes)
//   - CreateContextsOffset (4 bytes)
//   - CreateContextsLength (4 bytes)
//   - Buffer (variable) - contains Name (UTF-16LE) and CreateContexts
func DecodeCreateRequest(body []byte) (*CreateRequest, error) {
	if len(body) < 56 {
		return nil, fmt.Errorf("CREATE request too short: %d bytes", len(body))
	}

	req := &CreateRequest{
		OplockLevel:        body[3],
		ImpersonationLevel: binary.LittleEndian.Uint32(body[4:8]),
		DesiredAccess:      binary.LittleEndian.Uint32(body[24:28]),
		FileAttributes:     types.FileAttributes(binary.LittleEndian.Uint32(body[28:32])),
		ShareAccess:        binary.LittleEndian.Uint32(body[32:36]),
		CreateDisposition:  types.CreateDisposition(binary.LittleEndian.Uint32(body[36:40])),
		CreateOptions:      types.CreateOptions(binary.LittleEndian.Uint32(body[40:44])),
	}

	nameOffset := binary.LittleEndian.Uint16(body[44:46])
	nameLength := binary.LittleEndian.Uint16(body[46:48])

	// Extract filename (UTF-16LE encoded)
	// nameOffset is relative to the start of SMB2 header (64 bytes)
	// body starts after the header, so:
	//   body offset = nameOffset - 64
	// Typical nameOffset is 120 (64 header + 56 fixed part), giving body offset 56

	if nameLength > 0 {
		// Calculate where the name starts in our body buffer
		bodyOffset := int(nameOffset) - 64

		// Clamp to valid range (name can't start before the Buffer field at byte 56)
		if bodyOffset < 56 {
			bodyOffset = 56
		}

		// Extract the filename
		if bodyOffset+int(nameLength) <= len(body) {
			req.FileName = decodeUTF16LE(body[bodyOffset : bodyOffset+int(nameLength)])
		}
	}

	return req, nil
}

// EncodeCreateResponse builds an SMB2 CREATE response body [MS-SMB2] 2.2.14
func EncodeCreateResponse(resp *CreateResponse) ([]byte, error) {
	// Build response (89 bytes)
	buf := make([]byte, 89)
	binary.LittleEndian.PutUint16(buf[0:2], 89)                                          // StructureSize
	buf[2] = resp.OplockLevel                                                            // OplockLevel
	buf[3] = resp.Flags                                                                  // Flags
	binary.LittleEndian.PutUint32(buf[4:8], uint32(resp.CreateAction))                   // CreateAction
	binary.LittleEndian.PutUint64(buf[8:16], types.TimeToFiletime(resp.CreationTime))    // CreationTime
	binary.LittleEndian.PutUint64(buf[16:24], types.TimeToFiletime(resp.LastAccessTime)) // LastAccessTime
	binary.LittleEndian.PutUint64(buf[24:32], types.TimeToFiletime(resp.LastWriteTime))  // LastWriteTime
	binary.LittleEndian.PutUint64(buf[32:40], types.TimeToFiletime(resp.ChangeTime))     // ChangeTime
	binary.LittleEndian.PutUint64(buf[40:48], resp.AllocationSize)                       // AllocationSize
	binary.LittleEndian.PutUint64(buf[48:56], resp.EndOfFile)                            // EndOfFile
	binary.LittleEndian.PutUint32(buf[56:60], uint32(resp.FileAttributes))               // FileAttributes
	binary.LittleEndian.PutUint32(buf[60:64], 0)                                         // Reserved2
	copy(buf[64:80], resp.FileID[:])                                                     // FileId (persistent + volatile)
	binary.LittleEndian.PutUint32(buf[80:84], 0)                                         // CreateContextsOffset
	binary.LittleEndian.PutUint32(buf[84:88], 0)                                         // CreateContextsLength

	return buf, nil
}

// DecodeReadRequest parses an SMB2 READ request body [MS-SMB2] 2.2.19
func DecodeReadRequest(body []byte) (*ReadRequest, error) {
	if len(body) < 49 {
		return nil, fmt.Errorf("READ request too short: %d bytes", len(body))
	}

	req := &ReadRequest{
		Padding:        body[2],
		Flags:          body[3],
		Length:         binary.LittleEndian.Uint32(body[4:8]),
		Offset:         binary.LittleEndian.Uint64(body[8:16]),
		MinimumCount:   binary.LittleEndian.Uint32(body[32:36]),
		Channel:        binary.LittleEndian.Uint32(body[36:40]),
		RemainingBytes: binary.LittleEndian.Uint32(body[40:44]),
	}
	copy(req.FileID[:], body[16:32])

	return req, nil
}

// EncodeReadResponse builds an SMB2 READ response body [MS-SMB2] 2.2.20
func EncodeReadResponse(resp *ReadResponse) ([]byte, error) {
	// Response header is 17 bytes, data follows
	buf := make([]byte, 17+len(resp.Data))
	binary.LittleEndian.PutUint16(buf[0:2], 17)                     // StructureSize
	buf[2] = resp.DataOffset                                        // DataOffset (relative to header start)
	buf[3] = 0                                                      // Reserved
	binary.LittleEndian.PutUint32(buf[4:8], uint32(len(resp.Data))) // DataLength
	binary.LittleEndian.PutUint32(buf[8:12], resp.DataRemaining)    // DataRemaining
	binary.LittleEndian.PutUint32(buf[12:16], 0)                    // Reserved2
	copy(buf[17:], resp.Data)

	return buf, nil
}

// DecodeWriteRequest parses an SMB2 WRITE request body [MS-SMB2] 2.2.21
// The WRITE request structure is:
//   - StructureSize (2 bytes) - always 49
//   - DataOffset (2 bytes) - offset from SMB2 header start to write data
//   - Length (4 bytes) - length of data being written
//   - Offset (8 bytes) - offset in file to write
//   - FileId (16 bytes)
//   - Channel (4 bytes)
//   - RemainingBytes (4 bytes)
//   - WriteChannelInfoOffset (2 bytes)
//   - WriteChannelInfoLength (2 bytes)
//   - Flags (4 bytes)
//   - Buffer (variable) - padding + write data
func DecodeWriteRequest(body []byte) (*WriteRequest, error) {
	if len(body) < 49 {
		return nil, fmt.Errorf("WRITE request too short: %d bytes", len(body))
	}

	req := &WriteRequest{
		DataOffset:     binary.LittleEndian.Uint16(body[2:4]),
		Length:         binary.LittleEndian.Uint32(body[4:8]),
		Offset:         binary.LittleEndian.Uint64(body[8:16]),
		Channel:        binary.LittleEndian.Uint32(body[32:36]),
		RemainingBytes: binary.LittleEndian.Uint32(body[36:40]),
		Flags:          binary.LittleEndian.Uint32(body[44:48]),
	}
	copy(req.FileID[:], body[16:32])

	// Extract data
	// DataOffset is relative to the beginning of the SMB2 header (64 bytes)
	// Our body starts after the header, so we subtract 64
	// The fixed request structure is 48 bytes (StructureSize says 49 but that includes 1 byte of Buffer)
	// Data typically starts at offset 48 in the body (or wherever DataOffset-64 points)

	if req.Length > 0 {
		// Calculate where data starts in body
		dataStart := int(req.DataOffset) - 64

		// Clamp to valid range - data can't start before byte 48 (after fixed fields)
		if dataStart < 48 {
			dataStart = 48
		}

		// Try to extract data from calculated offset
		if dataStart+int(req.Length) <= len(body) {
			req.Data = body[dataStart : dataStart+int(req.Length)]
		} else if len(body) > 48 && int(req.Length) <= len(body)-48 {
			// Fallback: data might be right after the 48-byte fixed structure
			req.Data = body[48 : 48+int(req.Length)]
		} else if len(body) > 49 {
			// Last resort: take whatever data is available after fixed part
			req.Data = body[48:]
		}
	}

	return req, nil
}

// EncodeWriteResponse builds an SMB2 WRITE response body [MS-SMB2] 2.2.22
func EncodeWriteResponse(resp *WriteResponse) ([]byte, error) {
	buf := make([]byte, 17)
	binary.LittleEndian.PutUint16(buf[0:2], 17)              // StructureSize
	binary.LittleEndian.PutUint16(buf[2:4], 0)               // Reserved
	binary.LittleEndian.PutUint32(buf[4:8], resp.Count)      // Count
	binary.LittleEndian.PutUint32(buf[8:12], resp.Remaining) // Remaining
	binary.LittleEndian.PutUint16(buf[12:14], 0)             // WriteChannelInfoOffset
	binary.LittleEndian.PutUint16(buf[14:16], 0)             // WriteChannelInfoLength

	return buf, nil
}

// DecodeCloseRequest parses an SMB2 CLOSE request body [MS-SMB2] 2.2.15
func DecodeCloseRequest(body []byte) (*CloseRequest, error) {
	if len(body) < 24 {
		return nil, fmt.Errorf("CLOSE request too short: %d bytes", len(body))
	}

	req := &CloseRequest{
		Flags: binary.LittleEndian.Uint16(body[2:4]),
	}
	copy(req.FileID[:], body[8:24])

	return req, nil
}

// EncodeCloseResponse builds an SMB2 CLOSE response body [MS-SMB2] 2.2.16
func EncodeCloseResponse(resp *CloseResponse) ([]byte, error) {
	buf := make([]byte, 60)
	binary.LittleEndian.PutUint16(buf[0:2], 60)                                          // StructureSize
	binary.LittleEndian.PutUint16(buf[2:4], resp.Flags)                                  // Flags
	binary.LittleEndian.PutUint32(buf[4:8], 0)                                           // Reserved
	binary.LittleEndian.PutUint64(buf[8:16], types.TimeToFiletime(resp.CreationTime))    // CreationTime
	binary.LittleEndian.PutUint64(buf[16:24], types.TimeToFiletime(resp.LastAccessTime)) // LastAccessTime
	binary.LittleEndian.PutUint64(buf[24:32], types.TimeToFiletime(resp.LastWriteTime))  // LastWriteTime
	binary.LittleEndian.PutUint64(buf[32:40], types.TimeToFiletime(resp.ChangeTime))     // ChangeTime
	binary.LittleEndian.PutUint64(buf[40:48], resp.AllocationSize)                       // AllocationSize
	binary.LittleEndian.PutUint64(buf[48:56], resp.EndOfFile)                            // EndOfFile
	binary.LittleEndian.PutUint32(buf[56:60], uint32(resp.FileAttributes))               // FileAttributes

	return buf, nil
}

// DecodeQueryInfoRequest parses an SMB2 QUERY_INFO request body [MS-SMB2] 2.2.37
// Structure: StructureSize(2) + InfoType(1) + FileInfoClass(1) + OutputBufferLength(4) +
// InputBufferOffset(2) + Reserved(2) + InputBufferLength(4) + AdditionalInformation(4) +
// Flags(4) + FileId(16) = 40 bytes
func DecodeQueryInfoRequest(body []byte) (*QueryInfoRequest, error) {
	if len(body) < 40 {
		return nil, fmt.Errorf("QUERY_INFO request too short: %d bytes", len(body))
	}

	req := &QueryInfoRequest{
		InfoType:           body[2],
		FileInfoClass:      body[3],
		OutputBufferLength: binary.LittleEndian.Uint32(body[4:8]),
		InputBufferOffset:  binary.LittleEndian.Uint16(body[8:10]),
		InputBufferLength:  binary.LittleEndian.Uint32(body[12:16]),
		AdditionalInfo:     binary.LittleEndian.Uint32(body[16:20]),
		Flags:              binary.LittleEndian.Uint32(body[20:24]),
	}
	copy(req.FileID[:], body[24:40])

	return req, nil
}

// EncodeQueryInfoResponse builds an SMB2 QUERY_INFO response body [MS-SMB2] 2.2.38
func EncodeQueryInfoResponse(resp *QueryInfoResponse) ([]byte, error) {
	buf := make([]byte, 9+len(resp.Data))
	binary.LittleEndian.PutUint16(buf[0:2], 9)                      // StructureSize
	binary.LittleEndian.PutUint16(buf[2:4], uint16(64+9))           // OutputBufferOffset (after header + struct)
	binary.LittleEndian.PutUint32(buf[4:8], uint32(len(resp.Data))) // OutputBufferLength
	copy(buf[9:], resp.Data)

	return buf, nil
}

// DecodeSetInfoRequest parses an SMB2 SET_INFO request body [MS-SMB2] 2.2.39
// SET_INFO request structure (32 bytes fixed part):
//   - StructureSize (2 bytes) - always 33
//   - InfoType (1 byte)
//   - FileInfoClass (1 byte)
//   - BufferLength (4 bytes)
//   - BufferOffset (2 bytes) - offset from header start to buffer
//   - Reserved (2 bytes)
//   - AdditionalInfo (4 bytes)
//   - FileId (16 bytes)
//   - Buffer (variable)
func DecodeSetInfoRequest(body []byte) (*SetInfoRequest, error) {
	if len(body) < 32 {
		return nil, fmt.Errorf("SET_INFO request too short: %d bytes", len(body))
	}

	req := &SetInfoRequest{
		InfoType:       body[2],
		FileInfoClass:  body[3],
		BufferLength:   binary.LittleEndian.Uint32(body[4:8]),
		BufferOffset:   binary.LittleEndian.Uint16(body[8:10]),
		AdditionalInfo: binary.LittleEndian.Uint32(body[12:16]),
	}
	copy(req.FileID[:], body[16:32])

	// Extract buffer
	// BufferOffset is relative to the start of SMB2 header (64 bytes)
	// body starts after the header, so: body offset = BufferOffset - 64
	// Typical BufferOffset is 96 (64 header + 32 fixed part), giving body offset 32
	bufferStart := int(req.BufferOffset) - 64
	if bufferStart < 32 {
		bufferStart = 32 // Buffer can't start before the fixed part ends
	}
	if bufferStart+int(req.BufferLength) <= len(body) {
		req.Buffer = body[bufferStart : bufferStart+int(req.BufferLength)]
	}

	return req, nil
}

// EncodeSetInfoResponse builds an SMB2 SET_INFO response body [MS-SMB2] 2.2.40
func EncodeSetInfoResponse(_ *SetInfoResponse) ([]byte, error) {
	buf := make([]byte, 2)
	binary.LittleEndian.PutUint16(buf[0:2], 2) // StructureSize
	return buf, nil
}

// DecodeQueryDirectoryRequest parses an SMB2 QUERY_DIRECTORY request body [MS-SMB2] 2.2.33
// QUERY_DIRECTORY request structure (32 bytes fixed part):
//   - StructureSize (2 bytes) - always 33
//   - FileInfoClass (1 byte)
//   - Flags (1 byte)
//   - FileIndex (4 bytes)
//   - FileId (16 bytes)
//   - FileNameOffset (2 bytes) - offset from header start to filename
//   - FileNameLength (2 bytes)
//   - OutputBufferLength (4 bytes)
//   - Buffer (variable) - contains FileName (UTF-16LE)
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

// EncodeQueryDirectoryResponse builds an SMB2 QUERY_DIRECTORY response body [MS-SMB2] 2.2.34
// Structure:
//   - StructureSize (2 bytes) - always 9 (per spec, includes 1 byte of buffer conceptually)
//   - OutputBufferOffset (2 bytes) - offset from SMB2 header to buffer
//   - OutputBufferLength (4 bytes) - length of buffer data
//   - Buffer (variable) - directory entries starting at byte 8 of response body
func EncodeQueryDirectoryResponse(resp *QueryDirectoryResponse) ([]byte, error) {
	// Fixed response header is 8 bytes, data follows immediately
	buf := make([]byte, 8+len(resp.Data))
	binary.LittleEndian.PutUint16(buf[0:2], 9)                      // StructureSize (per spec, always 9)
	binary.LittleEndian.PutUint16(buf[2:4], uint16(64+8))           // OutputBufferOffset (header + 8 byte response)
	binary.LittleEndian.PutUint32(buf[4:8], uint32(len(resp.Data))) // OutputBufferLength
	copy(buf[8:], resp.Data)

	return buf, nil
}

// DecodeFlushRequest parses an SMB2 FLUSH request body [MS-SMB2] 2.2.17
func DecodeFlushRequest(body []byte) (*FlushRequest, error) {
	if len(body) < 24 {
		return nil, fmt.Errorf("FLUSH request too short: %d bytes", len(body))
	}

	req := &FlushRequest{}
	copy(req.FileID[:], body[8:24])

	return req, nil
}

// EncodeFlushResponse builds an SMB2 FLUSH response body [MS-SMB2] 2.2.18
func EncodeFlushResponse(_ *FlushResponse) ([]byte, error) {
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint16(buf[0:2], 4) // StructureSize
	binary.LittleEndian.PutUint16(buf[2:4], 0) // Reserved
	return buf, nil
}

// EncodeFileBasicInfo builds FILE_BASIC_INFORMATION [MS-FSCC] 2.4.7
func EncodeFileBasicInfo(info *FileBasicInfo) []byte {
	buf := make([]byte, 40)
	binary.LittleEndian.PutUint64(buf[0:8], types.TimeToFiletime(info.CreationTime))
	binary.LittleEndian.PutUint64(buf[8:16], types.TimeToFiletime(info.LastAccessTime))
	binary.LittleEndian.PutUint64(buf[16:24], types.TimeToFiletime(info.LastWriteTime))
	binary.LittleEndian.PutUint64(buf[24:32], types.TimeToFiletime(info.ChangeTime))
	binary.LittleEndian.PutUint32(buf[32:36], uint32(info.FileAttributes))
	// Reserved 4 bytes
	return buf
}

// DecodeFileBasicInfo parses FILE_BASIC_INFORMATION [MS-FSCC] 2.4.7
func DecodeFileBasicInfo(buf []byte) (*FileBasicInfo, error) {
	if len(buf) < 40 {
		return nil, fmt.Errorf("buffer too short for FILE_BASIC_INFORMATION: %d bytes", len(buf))
	}

	return &FileBasicInfo{
		CreationTime:   types.FiletimeToTime(binary.LittleEndian.Uint64(buf[0:8])),
		LastAccessTime: types.FiletimeToTime(binary.LittleEndian.Uint64(buf[8:16])),
		LastWriteTime:  types.FiletimeToTime(binary.LittleEndian.Uint64(buf[16:24])),
		ChangeTime:     types.FiletimeToTime(binary.LittleEndian.Uint64(buf[24:32])),
		FileAttributes: types.FileAttributes(binary.LittleEndian.Uint32(buf[32:36])),
	}, nil
}

// EncodeFileStandardInfo builds FILE_STANDARD_INFORMATION [MS-FSCC] 2.4.41
func EncodeFileStandardInfo(info *FileStandardInfo) []byte {
	buf := make([]byte, 24)
	binary.LittleEndian.PutUint64(buf[0:8], info.AllocationSize)
	binary.LittleEndian.PutUint64(buf[8:16], info.EndOfFile)
	binary.LittleEndian.PutUint32(buf[16:20], info.NumberOfLinks)
	if info.DeletePending {
		buf[20] = 1
	}
	if info.Directory {
		buf[21] = 1
	}
	// Reserved 2 bytes
	return buf
}

// EncodeFileNetworkOpenInfo builds FILE_NETWORK_OPEN_INFORMATION [MS-FSCC] 2.4.27
func EncodeFileNetworkOpenInfo(info *FileNetworkOpenInfo) []byte {
	buf := make([]byte, 56)
	binary.LittleEndian.PutUint64(buf[0:8], types.TimeToFiletime(info.CreationTime))
	binary.LittleEndian.PutUint64(buf[8:16], types.TimeToFiletime(info.LastAccessTime))
	binary.LittleEndian.PutUint64(buf[16:24], types.TimeToFiletime(info.LastWriteTime))
	binary.LittleEndian.PutUint64(buf[24:32], types.TimeToFiletime(info.ChangeTime))
	binary.LittleEndian.PutUint64(buf[32:40], info.AllocationSize)
	binary.LittleEndian.PutUint64(buf[40:48], info.EndOfFile)
	binary.LittleEndian.PutUint32(buf[48:52], uint32(info.FileAttributes))
	// Reserved 4 bytes
	return buf
}

// EncodeDirectoryEntry encodes a single directory entry for FILE_ID_BOTH_DIRECTORY_INFORMATION
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

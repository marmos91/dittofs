// Package handlers provides SMB2 command handlers and session management.
//
// This file implements the SMB2 QUERY_INFO command handler [MS-SMB2] 2.2.37, 2.2.38.
// QUERY_INFO retrieves file, filesystem, or security information about an open file.
package handlers

import (
	"context"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/smb/types"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// QueryInfoRequest represents an SMB2 QUERY_INFO request from a client [MS-SMB2] 2.2.37.
//
// QUERY_INFO retrieves metadata about a file, directory, filesystem, or security
// descriptor. The type of information returned depends on InfoType and FileInfoClass.
//
// **Wire format (40 bytes fixed):**
//
//	Offset  Size  Field              Description
//	0       2     StructureSize      Always 41 (includes 1 byte of buffer)
//	2       1     InfoType           Type of info: file (1), filesystem (2), security (3), quota (4)
//	3       1     FileInfoClass      Class of info within type
//	4       4     OutputBufferLength Max bytes to return
//	8       2     InputBufferOffset  Offset to input buffer (usually 0)
//	10      2     Reserved           Reserved (must be 0)
//	12      4     InputBufferLength  Length of input buffer (usually 0)
//	16      4     AdditionalInfo     Additional info for security queries
//	20      4     Flags              Query flags
//	24      16    FileId             SMB2 file identifier
//	40+     var   Buffer             Input buffer (if any)
//
// **Example:**
//
//	req := &QueryInfoRequest{
//	    InfoType:           types.SMB2InfoTypeFile,
//	    FileInfoClass:      FileBasicInformation,
//	    OutputBufferLength: 4096,
//	    FileID:             fileID,
//	}
type QueryInfoRequest struct {
	// InfoType specifies what type of information to query.
	// Valid values:
	//   - 1 (SMB2_0_INFO_FILE): File/directory information
	//   - 2 (SMB2_0_INFO_FILESYSTEM): Filesystem information
	//   - 3 (SMB2_0_INFO_SECURITY): Security information
	//   - 4 (SMB2_0_INFO_QUOTA): Quota information
	InfoType uint8

	// FileInfoClass specifies the specific information class within the type.
	// For InfoType=1 (file): FileBasicInformation (4), FileStandardInformation (5), etc.
	// For InfoType=2 (filesystem): FileFsVolumeInformation (1), FileFsSizeInformation (3), etc.
	// For InfoType=3 (security): Contains flags in AdditionalInfo instead.
	FileInfoClass uint8

	// OutputBufferLength is the maximum number of bytes to return.
	OutputBufferLength uint32

	// InputBufferOffset is the offset to the input buffer (if any).
	InputBufferOffset uint16

	// InputBufferLength is the length of the input buffer (if any).
	InputBufferLength uint32

	// AdditionalInfo contains additional info for security queries.
	// For security queries, this is a bit mask of OWNER_SECURITY_INFORMATION, etc.
	AdditionalInfo uint32

	// Flags contains query flags.
	Flags uint32

	// FileID is the SMB2 file identifier from CREATE response.
	FileID [16]byte
}

// QueryInfoResponse represents an SMB2 QUERY_INFO response to a client [MS-SMB2] 2.2.38.
//
// The response contains the requested information encoded in the Data field.
//
// **Wire format (8 bytes fixed + variable data):**
//
//	Offset  Size  Field              Description
//	0       2     StructureSize      Always 9 (includes 1 byte of buffer)
//	2       2     OutputBufferOffset Offset from header to data
//	4       4     OutputBufferLength Length of data
//	8+      var   Buffer             Query result data
type QueryInfoResponse struct {
	SMBResponseBase // Embeds Status field and GetStatus() method

	// Data contains the encoded query result.
	// Format depends on InfoType and FileInfoClass from the request.
	Data []byte
}

// ============================================================================
// Shared Info Types (used by multiple handlers)
// ============================================================================

// FileBasicInfo represents FILE_BASIC_INFORMATION [MS-FSCC] 2.4.7.
//
// This structure is used by both QUERY_INFO and SET_INFO to get/set
// timestamps and attributes.
//
// **Wire format (40 bytes):**
//
//	Offset  Size  Field              Description
//	0       8     CreationTime       FILETIME
//	8       8     LastAccessTime     FILETIME
//	16      8     LastWriteTime      FILETIME
//	24      8     ChangeTime         FILETIME
//	32      4     FileAttributes     File attribute flags
//	36      4     Reserved           Reserved
type FileBasicInfo struct {
	CreationTime   time.Time
	LastAccessTime time.Time
	LastWriteTime  time.Time
	ChangeTime     time.Time
	FileAttributes types.FileAttributes
}

// FileStandardInfo represents FILE_STANDARD_INFORMATION [MS-FSCC] 2.4.41.
//
// This structure is used by QUERY_INFO to return file size and metadata.
//
// **Wire format (24 bytes):**
//
//	Offset  Size  Field              Description
//	0       8     AllocationSize     Allocated size (cluster-aligned)
//	8       8     EndOfFile          Actual file size
//	16      4     NumberOfLinks      Hard link count
//	20      1     DeletePending      File marked for deletion
//	21      1     Directory          True if directory
//	22      2     Reserved           Reserved
type FileStandardInfo struct {
	AllocationSize uint64
	EndOfFile      uint64
	NumberOfLinks  uint32
	DeletePending  bool
	Directory      bool
}

// FileNetworkOpenInfo represents FILE_NETWORK_OPEN_INFORMATION [MS-FSCC] 2.4.27.
//
// This structure is optimized for network access and combines timestamps,
// sizes, and attributes into one response.
//
// **Wire format (56 bytes):**
//
//	Offset  Size  Field              Description
//	0       8     CreationTime       FILETIME
//	8       8     LastAccessTime     FILETIME
//	16      8     LastWriteTime      FILETIME
//	24      8     ChangeTime         FILETIME
//	32      8     AllocationSize     Allocated size
//	40      8     EndOfFile          Actual size
//	48      4     FileAttributes     File attributes
//	52      4     Reserved           Reserved
type FileNetworkOpenInfo struct {
	CreationTime   time.Time
	LastAccessTime time.Time
	LastWriteTime  time.Time
	ChangeTime     time.Time
	AllocationSize uint64
	EndOfFile      uint64
	FileAttributes types.FileAttributes
}

// FileAllInfo represents FILE_ALL_INFORMATION [MS-FSCC] 2.4.2.
//
// This structure combines multiple info classes into one response.
// It's a convenience structure that provides all commonly-needed file information.
type FileAllInfo struct {
	BasicInfo     FileBasicInfo
	StandardInfo  FileStandardInfo
	InternalInfo  uint64 // FileIndex
	EaInfo        uint32 // EaSize
	AccessInfo    uint32 // AccessFlags
	PositionInfo  uint64 // CurrentByteOffset
	ModeInfo      uint32 // Mode
	AlignmentInfo uint32 // AlignmentRequirement
	NameInfo      string // FileName
}

// ============================================================================
// Encoding/Decoding Functions
// ============================================================================

// DecodeQueryInfoRequest parses an SMB2 QUERY_INFO request body [MS-SMB2] 2.2.37.
//
// **Parameters:**
//   - body: Request body starting after the SMB2 header (64 bytes)
//
// **Returns:**
//   - *QueryInfoRequest: Parsed request structure
//   - error: Decoding error if body is malformed
//
// **Example:**
//
//	req, err := DecodeQueryInfoRequest(body)
//	if err != nil {
//	    return NewErrorResult(types.StatusInvalidParameter), nil
//	}
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

// Encode serializes the QueryInfoResponse into SMB2 wire format [MS-SMB2] 2.2.38.
//
// **Returns:**
//   - []byte: Response body with 8-byte header + data
//   - error: Encoding error (currently always nil)
func (resp *QueryInfoResponse) Encode() ([]byte, error) {
	buf := make([]byte, 9+len(resp.Data))
	binary.LittleEndian.PutUint16(buf[0:2], 9)                      // StructureSize
	binary.LittleEndian.PutUint16(buf[2:4], uint16(64+9))           // OutputBufferOffset (after header + struct)
	binary.LittleEndian.PutUint32(buf[4:8], uint32(len(resp.Data))) // OutputBufferLength
	copy(buf[9:], resp.Data)

	return buf, nil
}

// EncodeFileBasicInfo builds FILE_BASIC_INFORMATION [MS-FSCC] 2.4.7.
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

// DecodeFileBasicInfo parses FILE_BASIC_INFORMATION [MS-FSCC] 2.4.7.
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

// EncodeFileStandardInfo builds FILE_STANDARD_INFORMATION [MS-FSCC] 2.4.41.
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

// EncodeFileNetworkOpenInfo builds FILE_NETWORK_OPEN_INFORMATION [MS-FSCC] 2.4.27.
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

// ============================================================================
// Protocol Handler
// ============================================================================

// QueryInfo handles SMB2 QUERY_INFO command [MS-SMB2] 2.2.37, 2.2.38.
//
// QUERY_INFO retrieves metadata about an open file handle. This includes
// file timestamps, sizes, attributes, filesystem information, and security
// descriptors.
//
// **Purpose:**
//
// The QUERY_INFO command allows clients to:
//   - Get file timestamps and attributes (FileBasicInformation)
//   - Get file size and allocation (FileStandardInformation)
//   - Get combined information (FileAllInformation, FileNetworkOpenInformation)
//   - Get filesystem space and capabilities (FileFsSizeInformation, etc.)
//   - Get security descriptors for access control
//
// **Process:**
//
//  1. Decode and validate the request
//  2. Look up the open file by FileID
//  3. Get the file metadata from the metadata store
//  4. Build the response based on InfoType and FileInfoClass:
//     - InfoType=1 (File): Build file information
//     - InfoType=2 (Filesystem): Build filesystem information
//     - InfoType=3 (Security): Build security descriptor
//  5. Truncate response if larger than OutputBufferLength
//  6. Return the encoded response
//
// **Error Handling:**
//
// Returns appropriate SMB status codes:
//   - StatusInvalidParameter: Malformed request
//   - StatusInvalidHandle: Invalid FileID
//   - StatusBadNetworkName: Share not found
//   - StatusNotSupported: Unsupported info class
//   - StatusBufferOverflow: Response truncated (partial success)
//
// **Parameters:**
//   - ctx: SMB handler context with session information
//   - req: Parsed QUERY_INFO request
//
// **Returns:**
//   - *QueryInfoResponse: Response with requested information
//   - error: Internal error (rare)
func (h *Handler) QueryInfo(ctx *SMBHandlerContext, req *QueryInfoRequest) (*QueryInfoResponse, error) {
	logger.Debug("QUERY_INFO request",
		"infoType", req.InfoType,
		"fileInfoClass", req.FileInfoClass,
		"fileID", fmt.Sprintf("%x", req.FileID))

	// ========================================================================
	// Step 1: Get OpenFile by FileID
	// ========================================================================

	openFile, ok := h.GetOpenFile(req.FileID)
	if !ok {
		logger.Debug("QUERY_INFO: invalid file ID", "fileID", fmt.Sprintf("%x", req.FileID))
		return &QueryInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidHandle}}, nil
	}

	// ========================================================================
	// Step 2: Get metadata store and file attributes
	// ========================================================================

	metadataStore, err := h.Registry.GetMetadataStoreForShare(openFile.ShareName)
	if err != nil {
		logger.Warn("QUERY_INFO: failed to get metadata store", "share", openFile.ShareName, "error", err)
		return &QueryInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusBadNetworkName}}, nil
	}

	file, err := metadataStore.GetFile(ctx.Context, openFile.MetadataHandle)
	if err != nil {
		logger.Debug("QUERY_INFO: failed to get file", "path", openFile.Path, "error", err)
		return &QueryInfoResponse{SMBResponseBase: SMBResponseBase{Status: MetadataErrorToSMBStatus(err)}}, nil
	}

	// ========================================================================
	// Step 3: Build info based on type and class
	// ========================================================================

	var info []byte

	switch req.InfoType {
	case types.SMB2InfoTypeFile:
		info, err = h.buildFileInfoFromStore(file, types.FileInfoClass(req.FileInfoClass))
	case types.SMB2InfoTypeFilesystem:
		info, err = h.buildFilesystemInfo(ctx.Context, types.FileInfoClass(req.FileInfoClass), metadataStore, openFile.MetadataHandle)
	case types.SMB2InfoTypeSecurity:
		info, err = h.buildSecurityInfo()
	default:
		return &QueryInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInvalidParameter}}, nil
	}

	if err != nil {
		return &QueryInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusNotSupported}}, nil
	}

	// Truncate if necessary
	// Note: We return STATUS_SUCCESS instead of STATUS_BUFFER_OVERFLOW because
	// Linux kernel CIFS treats STATUS_BUFFER_OVERFLOW as an error, causing I/O failures.
	// The truncated data is still valid and useful for the client.
	if uint32(len(info)) > req.OutputBufferLength {
		info = info[:req.OutputBufferLength]
	}

	// ========================================================================
	// Step 4: Build success response
	// ========================================================================

	return &QueryInfoResponse{
		SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
		Data:            info,
	}, nil
}

// ============================================================================
// Helper Functions
// ============================================================================

// buildFileInfoFromStore builds file information based on class using metadata store.
func (h *Handler) buildFileInfoFromStore(file *metadata.File, class types.FileInfoClass) ([]byte, error) {
	switch class {
	case types.FileBasicInformation:
		basicInfo := FileAttrToFileBasicInfo(&file.FileAttr)
		return EncodeFileBasicInfo(basicInfo), nil

	case types.FileStandardInformation:
		standardInfo := FileAttrToFileStandardInfo(&file.FileAttr, false)
		return EncodeFileStandardInfo(standardInfo), nil

	case types.FileInternalInformation:
		// FILE_INTERNAL_INFORMATION [MS-FSCC] 2.4.20 (8 bytes)
		info := make([]byte, 8)
		// Convert UUID to uint64 by using first 8 bytes
		fileIndex := binary.LittleEndian.Uint64(file.ID[:8])
		binary.LittleEndian.PutUint64(info[0:8], fileIndex) // IndexNumber (unique file ID)
		return info, nil

	case types.FileEaInformation:
		// FILE_EA_INFORMATION [MS-FSCC] 2.4.12 (4 bytes)
		info := make([]byte, 4)
		binary.LittleEndian.PutUint32(info[0:4], 0) // EaSize
		return info, nil

	case types.FileAccessInformation:
		// FILE_ACCESS_INFORMATION [MS-FSCC] 2.4.1 (4 bytes)
		info := make([]byte, 4)
		binary.LittleEndian.PutUint32(info[0:4], 0x001F01FF) // AccessFlags (full access)
		return info, nil

	case types.FileNetworkOpenInformation:
		networkInfo := FileAttrToFileNetworkOpenInfo(&file.FileAttr)
		return EncodeFileNetworkOpenInfo(networkInfo), nil

	case types.FileAllInformation:
		return h.buildFileAllInformationFromStore(file), nil

	default:
		return nil, types.ErrNotSupported
	}
}

// buildFileAllInformationFromStore builds FILE_ALL_INFORMATION from metadata.
func (h *Handler) buildFileAllInformationFromStore(file *metadata.File) []byte {
	// FILE_ALL_INFORMATION [MS-FSCC] 2.4.2 (varies)
	// Basic (40) + Standard (24) + Internal (8) + EA (4) + Access (4) + Position (8) + Mode (4) + Alignment (4) + Name (variable)
	// Linux kernel's smb2_file_all_info struct requires minimum 101 bytes (100 fixed + 1 byte for FileName)
	// We allocate 104 bytes to include FileNameLength (4) + minimum padding for FileName
	info := make([]byte, 104)

	basicInfo := FileAttrToFileBasicInfo(&file.FileAttr)
	standardInfo := FileAttrToFileStandardInfo(&file.FileAttr, false)

	// BasicInformation (40 bytes)
	basicBytes := EncodeFileBasicInfo(basicInfo)
	copy(info[0:40], basicBytes)

	// StandardInformation (24 bytes) starting at offset 40
	standardBytes := EncodeFileStandardInfo(standardInfo)
	copy(info[40:64], standardBytes)

	// InternalInformation (8 bytes) starting at offset 64
	fileIndex := binary.LittleEndian.Uint64(file.ID[:8])
	binary.LittleEndian.PutUint64(info[64:72], fileIndex)

	// EaInformation (4 bytes) starting at offset 72
	binary.LittleEndian.PutUint32(info[72:76], 0)

	// AccessInformation (4 bytes) starting at offset 76
	binary.LittleEndian.PutUint32(info[76:80], 0x001F01FF)

	// PositionInformation (8 bytes) starting at offset 80
	binary.LittleEndian.PutUint64(info[80:88], 0)

	// ModeInformation (4 bytes) starting at offset 88
	binary.LittleEndian.PutUint32(info[88:92], 0)

	// AlignmentInformation (4 bytes) starting at offset 92
	binary.LittleEndian.PutUint32(info[92:96], 0)

	// NameInformation (4 bytes for length) starting at offset 96
	binary.LittleEndian.PutUint32(info[96:100], 0)

	return info
}

// buildFilesystemInfo builds filesystem information [MS-FSCC] 2.5.
func (h *Handler) buildFilesystemInfo(ctx context.Context, class types.FileInfoClass, metadataStore metadata.MetadataStore, handle metadata.FileHandle) ([]byte, error) {
	switch class {
	case 1: // FileFsVolumeInformation [MS-FSCC] 2.5.9
		label := []byte{'D', 0, 'i', 0, 't', 0, 't', 0, 'o', 0, 'F', 0, 'S', 0} // "DittoFS" in UTF-16LE
		info := make([]byte, 18+len(label))
		binary.LittleEndian.PutUint64(info[0:8], types.NowFiletime())
		binary.LittleEndian.PutUint32(info[8:12], 0x12345678) // VolumeSerialNumber
		binary.LittleEndian.PutUint32(info[12:16], uint32(len(label)))
		info[16] = 0 // SupportsObjects
		info[17] = 0 // Reserved
		copy(info[18:], label)
		return info, nil

	case 2: // FileFsLabelInformation [MS-FSCC] 2.5.5 - used for volume label
		label := []byte{'D', 0, 'i', 0, 't', 0, 't', 0, 'o', 0, 'F', 0, 'S', 0}
		info := make([]byte, 4+len(label))
		binary.LittleEndian.PutUint32(info[0:4], uint32(len(label)))
		copy(info[4:], label)
		return info, nil

	case 3: // FileFsSizeInformation [MS-FSCC] 2.5.8
		// Try to get real filesystem stats
		blockSize := uint64(4096)
		stats, err := metadataStore.GetFilesystemStatistics(ctx, handle)
		if err == nil {
			totalBlocks := stats.TotalBytes / blockSize
			availBlocks := stats.AvailableBytes / blockSize
			info := make([]byte, 24)
			binary.LittleEndian.PutUint64(info[0:8], totalBlocks)
			binary.LittleEndian.PutUint64(info[8:16], availBlocks)
			binary.LittleEndian.PutUint32(info[16:20], 1)
			binary.LittleEndian.PutUint32(info[20:24], uint32(blockSize))
			return info, nil
		}
		// Fallback to hardcoded values
		info := make([]byte, 24)
		binary.LittleEndian.PutUint64(info[0:8], 1000000)
		binary.LittleEndian.PutUint64(info[8:16], 500000)
		binary.LittleEndian.PutUint32(info[16:20], 1)
		binary.LittleEndian.PutUint32(info[20:24], 4096)
		return info, nil

	case 4: // FileFsDeviceInformation [MS-FSCC] 2.5.9
		// DeviceType (4 bytes) + Characteristics (4 bytes) = 8 bytes
		info := make([]byte, 8)
		binary.LittleEndian.PutUint32(info[0:4], 0x00000007) // FILE_DEVICE_DISK
		binary.LittleEndian.PutUint32(info[4:8], 0x00000000) // No special characteristics
		return info, nil

	case 5: // FileFsAttributeInformation [MS-FSCC] 2.5.1
		fsName := []byte{'N', 0, 'T', 0, 'F', 0, 'S', 0} // "NTFS" in UTF-16LE
		info := make([]byte, 12+len(fsName))
		binary.LittleEndian.PutUint32(info[0:4], 0x00000003) // FILE_CASE_SENSITIVE_SEARCH | FILE_CASE_PRESERVED_NAMES
		binary.LittleEndian.PutUint32(info[4:8], 255)
		binary.LittleEndian.PutUint32(info[8:12], uint32(len(fsName)))
		copy(info[12:], fsName)
		return info, nil

	case 7: // FileFsFullSizeInformation [MS-FSCC] 2.5.4
		blockSize := uint64(4096)
		stats, err := metadataStore.GetFilesystemStatistics(ctx, handle)
		if err == nil {
			totalBlocks := stats.TotalBytes / blockSize
			availBlocks := stats.AvailableBytes / blockSize
			info := make([]byte, 32)
			binary.LittleEndian.PutUint64(info[0:8], totalBlocks)
			binary.LittleEndian.PutUint64(info[8:16], availBlocks)
			binary.LittleEndian.PutUint64(info[16:24], availBlocks)
			binary.LittleEndian.PutUint32(info[24:28], 1)
			binary.LittleEndian.PutUint32(info[28:32], uint32(blockSize))
			return info, nil
		}
		// Fallback
		info := make([]byte, 32)
		binary.LittleEndian.PutUint64(info[0:8], 1000000)
		binary.LittleEndian.PutUint64(info[8:16], 500000)
		binary.LittleEndian.PutUint64(info[16:24], 500000)
		binary.LittleEndian.PutUint32(info[24:28], 1)
		binary.LittleEndian.PutUint32(info[28:32], 4096)
		return info, nil

	case 8: // FileFsObjectIdInformation [MS-FSCC] 2.5.6
		// Returns the object ID for the file system volume
		// Structure: ObjectId (16 bytes GUID) + ExtendedInfo (48 bytes)
		info := make([]byte, 64)
		// Use handler's ServerGUID as the volume ObjectId
		copy(info[0:16], h.ServerGUID[:])
		// ExtendedInfo is left as zeros (not required)
		return info, nil

	case 11: // FileFsSectorSizeInformation [MS-FSCC] 2.5.8
		// 28 bytes structure (matching Samba's implementation)
		info := make([]byte, 28)
		bps := uint32(512)                                     // bytes per sector
		binary.LittleEndian.PutUint32(info[0:4], bps)          // LogicalBytesPerSector
		binary.LittleEndian.PutUint32(info[4:8], bps)          // PhysicalBytesPerSectorForAtomicity
		binary.LittleEndian.PutUint32(info[8:12], bps)         // PhysicalBytesPerSectorForPerformance
		binary.LittleEndian.PutUint32(info[12:16], bps)        // FileSystemEffectivePhysicalBytesPerSectorForAtomicity
		binary.LittleEndian.PutUint32(info[16:20], 0x00000003) // Flags: ALIGNED_DEVICE | PARTITION_ALIGNED
		binary.LittleEndian.PutUint32(info[20:24], 0)          // ByteOffsetForSectorAlignment
		binary.LittleEndian.PutUint32(info[24:28], 0)          // ByteOffsetForPartitionAlignment
		return info, nil

	default:
		return nil, types.ErrNotSupported
	}
}

// buildSecurityInfo builds security information [MS-DTYP] 2.4.6.
func (h *Handler) buildSecurityInfo() ([]byte, error) {
	// Return minimal security descriptor that grants everyone access
	info := make([]byte, 20)
	info[0] = 1                                      // Revision
	info[1] = 0                                      // Sbz1
	binary.LittleEndian.PutUint16(info[2:4], 0x8004) // Control (SE_SELF_RELATIVE | SE_DACL_PRESENT)
	binary.LittleEndian.PutUint32(info[4:8], 0)      // OffsetOwner
	binary.LittleEndian.PutUint32(info[8:12], 0)     // OffsetGroup
	binary.LittleEndian.PutUint32(info[12:16], 0)    // OffsetSacl
	binary.LittleEndian.PutUint32(info[16:20], 0)    // OffsetDacl

	return info, nil
}

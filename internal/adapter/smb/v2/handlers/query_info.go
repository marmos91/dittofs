package handlers

import (
	"context"
	"encoding/binary"
	"fmt"
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Request and Response Structures
// ============================================================================

// QueryInfoRequest represents an SMB2 QUERY_INFO request from a client [MS-SMB2] 2.2.37.
// QUERY_INFO retrieves metadata about a file, directory, filesystem, or security
// descriptor. The type of information returned depends on InfoType and FileInfoClass.
// The fixed wire format is 40 bytes.
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
// The response contains the requested information encoded in the Data field.
// The fixed wire format is 8 bytes plus variable-length data.
type QueryInfoResponse struct {
	SMBResponseBase // Embeds Status field and GetStatus() method

	// Data contains the encoded query result.
	// Format depends on InfoType and FileInfoClass from the request.
	Data []byte
}

// ============================================================================
// Shared Info Types (used by multiple handlers)
// ============================================================================

// FileBasicInfo represents FILE_BASIC_INFORMATION [MS-FSCC] 2.4.7 (40 bytes).
// Used by both QUERY_INFO and SET_INFO to get/set timestamps and attributes.
type FileBasicInfo struct {
	CreationTime   time.Time
	LastAccessTime time.Time
	LastWriteTime  time.Time
	ChangeTime     time.Time
	FileAttributes types.FileAttributes
}

// FileStandardInfo represents FILE_STANDARD_INFORMATION [MS-FSCC] 2.4.41 (24 bytes).
// Used by QUERY_INFO to return file size, link count, and deletion status.
type FileStandardInfo struct {
	AllocationSize uint64
	EndOfFile      uint64
	NumberOfLinks  uint32
	DeletePending  bool
	Directory      bool
}

// FileNetworkOpenInfo represents FILE_NETWORK_OPEN_INFORMATION [MS-FSCC] 2.4.27 (56 bytes).
// Optimized for network access, combining timestamps, sizes, and attributes.
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
// Returns an error if the body is less than 40 bytes.
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
// Per MS-SMB2 convention, StructureSize is 9 but the actual fixed part is 8 bytes
// (StructureSize(2) + OutputBufferOffset(2) + OutputBufferLength(4)). The variable
// buffer starts at offset 8 from the body, which is 64+8=72 from the header.
func (resp *QueryInfoResponse) Encode() ([]byte, error) {
	buf := make([]byte, 8+len(resp.Data))
	binary.LittleEndian.PutUint16(buf[0:2], 9)                      // StructureSize (per spec, always 9)
	binary.LittleEndian.PutUint16(buf[2:4], uint16(64+8))           // OutputBufferOffset (header + fixed part)
	binary.LittleEndian.PutUint32(buf[4:8], uint32(len(resp.Data))) // OutputBufferLength
	copy(buf[8:], resp.Data)

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
// QUERY_INFO retrieves metadata about an open file handle including file
// timestamps, sizes, attributes, filesystem information, and security
// descriptors. The response format depends on InfoType and FileInfoClass.
// Results are truncated to OutputBufferLength if necessary.
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
	// Step 2: Handle named pipe (IPC$) queries
	// ========================================================================

	// Named pipes (e.g., srvsvc, lsarpc) have no metadata handle.
	// Return synthetic attributes so Windows Explorer does not get
	// STATUS_INTERNAL_ERROR when it queries the IPC$ tree.
	if openFile.IsPipe {
		return h.handlePipeQueryInfo(req, openFile)
	}

	// ========================================================================
	// Step 3: Get metadata store and file attributes
	// ========================================================================

	metaSvc := h.Registry.GetMetadataService()

	file, err := metaSvc.GetFile(ctx.Context, openFile.MetadataHandle)
	if err != nil {
		logger.Debug("QUERY_INFO: failed to get file", "path", openFile.Path, "error", err)
		return &QueryInfoResponse{SMBResponseBase: SMBResponseBase{Status: MetadataErrorToSMBStatus(err)}}, nil
	}

	// Per MS-FSA 2.1.5.14.2: Apply frozen timestamp overrides.
	// When SET_INFO(-1) freezes a timestamp, subsequent operations (WRITE,
	// child CREATE/DELETE for directories, truncate) may update the store
	// or pending state. Override the returned values with frozen values so
	// QUERY_INFO returns the timestamp as it was at freeze time.
	applyFrozenTimestamps(openFile, file)

	// ========================================================================
	// Step 3: Validate OutputBufferLength for fixed-size info classes
	// ========================================================================

	// Per MS-FSCC, if the OutputBufferLength is smaller than the minimum
	// required for a fixed-size information class, return STATUS_INFO_LENGTH_MISMATCH.
	if req.InfoType == types.SMB2InfoTypeFile {
		minSize := fileInfoClassMinSize(types.FileInfoClass(req.FileInfoClass))
		if minSize > 0 && req.OutputBufferLength < minSize {
			return &QueryInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusInfoLengthMismatch}}, nil
		}
	}

	// ========================================================================
	// Step 4: Build info based on type and class
	// ========================================================================

	var info []byte

	switch req.InfoType {
	case types.SMB2InfoTypeFile:
		info, err = h.buildFileInfoFromStore(file, openFile, types.FileInfoClass(req.FileInfoClass))
	case types.SMB2InfoTypeFilesystem:
		info, err = h.buildFilesystemInfo(ctx.Context, types.FileInfoClass(req.FileInfoClass), metaSvc, openFile.MetadataHandle)
	case types.SMB2InfoTypeSecurity:
		info, err = BuildSecurityDescriptor(file, req.AdditionalInfo)
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

		// For FILE_ALL_INFORMATION, the FileNameLength field at offset 96
		// must be updated to reflect the actual available bytes after truncation.
		// Otherwise the client will try to read more FileName bytes than exist.
		if req.InfoType == types.SMB2InfoTypeFile &&
			types.FileInfoClass(req.FileInfoClass) == types.FileAllInformation &&
			len(info) >= 100 {
			actualNameLen := len(info) - 100
			binary.LittleEndian.PutUint32(info[96:100], uint32(actualNameLen))
		}
	}

	// ========================================================================
	// Step 5: Build success response
	// ========================================================================

	return &QueryInfoResponse{
		SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
		Data:            info,
	}, nil
}

// ============================================================================
// Helper Functions
// ============================================================================

// handlePipeQueryInfo returns synthetic information for named pipe handles.
// Named pipes on IPC$ have no backing metadata store entry, so we fabricate
// the minimum attributes that Windows expects. Most info classes return
// STATUS_NOT_SUPPORTED since pipes have no filesystem semantics.
func (h *Handler) handlePipeQueryInfo(req *QueryInfoRequest, openFile *OpenFile) (*QueryInfoResponse, error) {
	logger.Debug("QUERY_INFO on named pipe",
		"pipeName", openFile.PipeName,
		"infoType", req.InfoType,
		"fileInfoClass", req.FileInfoClass)

	switch req.InfoType {
	case types.SMB2InfoTypeFile:
		return h.handlePipeFileInfo(req, openFile)
	default:
		// Security, filesystem, and quota info are not applicable to named pipes.
		return &QueryInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusNotSupported}}, nil
	}
}

// handlePipeFileInfo returns synthetic file information for named pipe handles.
func (h *Handler) handlePipeFileInfo(req *QueryInfoRequest, openFile *OpenFile) (*QueryInfoResponse, error) {
	now := time.Now()
	class := types.FileInfoClass(req.FileInfoClass)

	switch class {
	case types.FileBasicInformation:
		info := EncodeFileBasicInfo(&FileBasicInfo{
			CreationTime:   now,
			LastAccessTime: now,
			LastWriteTime:  now,
			ChangeTime:     now,
			FileAttributes: types.FileAttributeNormal,
		})
		return &QueryInfoResponse{
			SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
			Data:            info,
		}, nil

	case types.FileStandardInformation:
		info := EncodeFileStandardInfo(&FileStandardInfo{
			AllocationSize: 0,
			EndOfFile:      0,
			NumberOfLinks:  1,
			DeletePending:  false,
			Directory:      false,
		})
		return &QueryInfoResponse{
			SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
			Data:            info,
		}, nil

	case types.FileInternalInformation:
		// 8 bytes, zero index for pipes.
		return &QueryInfoResponse{
			SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
			Data:            make([]byte, 8),
		}, nil

	case types.FileEaInformation:
		// 4 bytes, EaSize = 0.
		return &QueryInfoResponse{
			SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
			Data:            make([]byte, 4),
		}, nil

	case types.FileAccessInformation:
		info := make([]byte, 4)
		binary.LittleEndian.PutUint32(info[0:4], 0x001F01FF) // Full access
		return &QueryInfoResponse{
			SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
			Data:            info,
		}, nil

	case types.FileNameInformation:
		nameBytes := encodeUTF16LE("\\" + openFile.PipeName)
		info := make([]byte, 4+len(nameBytes))
		binary.LittleEndian.PutUint32(info[0:4], uint32(len(nameBytes)))
		copy(info[4:], nameBytes)
		return &QueryInfoResponse{
			SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
			Data:            info,
		}, nil

	case types.FilePositionInformation:
		return &QueryInfoResponse{
			SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
			Data:            make([]byte, 8),
		}, nil

	case types.FileModeInformation:
		return &QueryInfoResponse{
			SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
			Data:            make([]byte, 4),
		}, nil

	case types.FileAlignmentInformation:
		return &QueryInfoResponse{
			SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
			Data:            make([]byte, 4),
		}, nil

	case types.FileAllInformation:
		// Build a minimal FILE_ALL_INFORMATION for the pipe.
		nameBytes := encodeUTF16LE("\\" + openFile.PipeName)
		fixedSize := 100
		info := make([]byte, fixedSize+len(nameBytes))
		// BasicInformation (40 bytes)
		basicBytes := EncodeFileBasicInfo(&FileBasicInfo{
			CreationTime:   now,
			LastAccessTime: now,
			LastWriteTime:  now,
			ChangeTime:     now,
			FileAttributes: types.FileAttributeNormal,
		})
		copy(info[0:40], basicBytes)
		// StandardInformation (24 bytes) at offset 40
		stdBytes := EncodeFileStandardInfo(&FileStandardInfo{
			NumberOfLinks: 1,
		})
		copy(info[40:64], stdBytes)
		// InternalInformation (8 bytes) at offset 64 - zeros
		// EaInformation (4 bytes) at offset 72 - zero
		// AccessInformation (4 bytes) at offset 76
		binary.LittleEndian.PutUint32(info[76:80], 0x001F01FF)
		// PositionInformation (8 bytes) at offset 80 - zero
		// ModeInformation (4 bytes) at offset 88 - zero
		// AlignmentInformation (4 bytes) at offset 92 - zero
		// NameInformation: length (4 bytes) at offset 96 + name data
		binary.LittleEndian.PutUint32(info[96:100], uint32(len(nameBytes)))
		copy(info[100:], nameBytes)

		return &QueryInfoResponse{
			SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
			Data:            info,
		}, nil

	default:
		return &QueryInfoResponse{SMBResponseBase: SMBResponseBase{Status: types.StatusNotSupported}}, nil
	}
}

// buildFileInfoFromStore builds file information based on class using metadata store.
func (h *Handler) buildFileInfoFromStore(file *metadata.File, openFile *OpenFile, class types.FileInfoClass) ([]byte, error) {
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
		return make([]byte, 4), nil // EaSize = 0

	case types.FileAccessInformation:
		// FILE_ACCESS_INFORMATION [MS-FSCC] 2.4.1 (4 bytes)
		info := make([]byte, 4)
		binary.LittleEndian.PutUint32(info[0:4], 0x001F01FF) // AccessFlags (full access)
		return info, nil

	case types.FileStreamInformation:
		// FileStreamInformation [MS-FSCC] 2.4.44
		// Return the default unnamed data stream (::$DATA)
		streamName := []byte{':', 0, ':', 0, '$', 0, 'D', 0, 'A', 0, 'T', 0, 'A', 0} // "::$DATA" UTF-16LE
		size := getSMBSize(&file.FileAttr)
		alloc := calculateAllocationSize(size)
		info := make([]byte, 24+len(streamName))
		binary.LittleEndian.PutUint32(info[0:4], 0)                       // NextEntryOffset (last entry)
		binary.LittleEndian.PutUint32(info[4:8], uint32(len(streamName))) // StreamNameLength
		binary.LittleEndian.PutUint64(info[8:16], size)                   // StreamSize
		binary.LittleEndian.PutUint64(info[16:24], alloc)                 // StreamAllocationSize
		copy(info[24:], streamName)
		return info, nil

	case types.FileNetworkOpenInformation:
		networkInfo := FileAttrToFileNetworkOpenInfo(&file.FileAttr)
		return EncodeFileNetworkOpenInfo(networkInfo), nil

	case types.FilePositionInformation:
		// FILE_POSITION_INFORMATION [MS-FSCC] 2.4.32 (8 bytes)
		return make([]byte, 8), nil // CurrentByteOffset = 0 (server doesn't track position)

	case types.FileModeInformation:
		// FILE_MODE_INFORMATION [MS-FSCC] 2.4.24 (4 bytes)
		// Mode is derived from CreateOptions passed during the CREATE request.
		// Per MS-FSCC, the Mode field is a combination of:
		//   FILE_WRITE_THROUGH (0x02), FILE_SEQUENTIAL_ONLY (0x04),
		//   FILE_NO_INTERMEDIATE_BUFFERING (0x08), FILE_SYNCHRONOUS_IO_ALERT (0x10),
		//   FILE_SYNCHRONOUS_IO_NONALERT (0x20), FILE_DELETE_ON_CLOSE (0x1000)
		info := make([]byte, 4)
		modeMask := types.FileWriteThrough | types.FileSequentialOnly |
			types.FileNoIntermediateBuffering | types.FileSynchronousIoAlert |
			types.FileSynchronousIoNonalert | types.FileDeleteOnClose
		mode := openFile.CreateOptions & modeMask
		binary.LittleEndian.PutUint32(info[0:4], uint32(mode))
		return info, nil

	case types.FileAlignmentInformation:
		// FILE_ALIGNMENT_INFORMATION [MS-FSCC] 2.4.3 (4 bytes)
		return make([]byte, 4), nil // AlignmentRequirement = 0 (byte-aligned)

	case types.FileNameInformation:
		// FILE_NAME_INFORMATION [MS-FSCC] 2.4.26 (4 bytes + variable)
		nameBytes := encodeUTF16LE(toSMBPath(openFile.Path))
		info := make([]byte, 4+len(nameBytes))
		binary.LittleEndian.PutUint32(info[0:4], uint32(len(nameBytes))) // FileNameLength
		copy(info[4:], nameBytes)
		return info, nil

	case types.FileAlternateNameInformation:
		// FILE_ALTERNATE_NAME_INFORMATION [MS-FSCC] 2.4.5 (4 bytes + variable)
		// Returns the 8.3 short name
		shortNameBytes := generate83ShortName(openFile.FileName)
		if shortNameBytes == nil {
			// For root or entries without a short name, use the filename itself
			shortNameBytes = encodeUTF16LE(openFile.FileName)
		}
		info := make([]byte, 4+len(shortNameBytes))
		binary.LittleEndian.PutUint32(info[0:4], uint32(len(shortNameBytes))) // FileNameLength
		copy(info[4:], shortNameBytes)
		return info, nil

	case types.FileNormalizedNameInformation:
		// FILE_NORMALIZED_NAME_INFORMATION [MS-FSCC] 2.4.28 (4 bytes + variable)
		// Returns the normalized name relative to the share root (no leading backslash).
		filePath := openFile.Path
		if filePath == "" {
			filePath = "\\"
		} else {
			filePath = strings.ReplaceAll(filePath, "/", "\\")
		}
		nameBytes := encodeUTF16LE(filePath)
		info := make([]byte, 4+len(nameBytes))
		binary.LittleEndian.PutUint32(info[0:4], uint32(len(nameBytes))) // FileNameLength
		copy(info[4:], nameBytes)
		return info, nil

	case types.FileIdInformation:
		// FILE_ID_INFORMATION [MS-FSCC] 2.4.46 (24 bytes)
		// VolumeSerialNumber (8 bytes) + FileId (16 bytes)
		info := make([]byte, 24)
		binary.LittleEndian.PutUint64(info[0:8], ntfsVolumeSerialNumber) // VolumeSerialNumber
		copy(info[8:24], file.ID[:16])                                   // FileId (128-bit)
		return info, nil

	case types.FileCompressionInformation:
		// FILE_COMPRESSION_INFORMATION [MS-FSCC] 2.4.9 (16 bytes)
		// CompressedFileSize (8) + CompressionFormat (2) + CompressionUnitShift (1) +
		// ChunkShift (1) + ClusterShift (1) + Reserved (3)
		info := make([]byte, 16)
		size := getSMBSize(&file.FileAttr)
		binary.LittleEndian.PutUint64(info[0:8], size) // CompressedFileSize = EndOfFile
		// CompressionFormat(2) + shifts(3) + Reserved(3) all zero = COMPRESSION_FORMAT_NONE
		return info, nil

	case types.FileAttributeTagInformation:
		// FILE_ATTRIBUTE_TAG_INFORMATION [MS-FSCC] 2.4.6 (8 bytes)
		// FileAttributes (4) + ReparseTag (4)
		info := make([]byte, 8)
		attrs := FileAttrToSMBAttributes(&file.FileAttr)
		binary.LittleEndian.PutUint32(info[0:4], uint32(attrs))
		// ReparseTag = 0 for non-reparse points
		return info, nil

	case types.FileAllInformation:
		return h.buildFileAllInformationFromStore(file, openFile), nil

	default:
		return nil, types.ErrNotSupported
	}
}

// buildFileAllInformationFromStore builds FILE_ALL_INFORMATION from metadata.
func (h *Handler) buildFileAllInformationFromStore(file *metadata.File, openFile *OpenFile) []byte {
	// FILE_ALL_INFORMATION [MS-FSCC] 2.4.2 (varies)
	// Basic (40) + Standard (24) + Internal (8) + EA (4) + Access (4) + Position (8) + Mode (4) + Alignment (4) + Name (variable)

	basicInfo := FileAttrToFileBasicInfo(&file.FileAttr)
	standardInfo := FileAttrToFileStandardInfo(&file.FileAttr, false)
	nameBytes := encodeUTF16LE(toSMBPath(openFile.Path))

	// Fixed part: 96 bytes + NameInformation header (4 bytes for length) + name data
	// Minimum total per Linux kernel requirement: 104 bytes (100 fixed + 4 for FileNameLength)
	fixedSize := 100 // 40+24+8+4+4+8+4+4+4 = 100
	info := make([]byte, fixedSize+len(nameBytes))

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

	// NameInformation (4 bytes for length + variable name) starting at offset 96
	binary.LittleEndian.PutUint32(info[96:100], uint32(len(nameBytes)))
	copy(info[100:], nameBytes)

	return info
}

// buildFilesystemInfo builds filesystem information [MS-FSCC] 2.5.
func (h *Handler) buildFilesystemInfo(ctx context.Context, class types.FileInfoClass, metaSvc *metadata.MetadataService, handle metadata.FileHandle) ([]byte, error) {
	switch class {
	case 1: // FileFsVolumeInformation [MS-FSCC] 2.5.9
		label := encodeUTF16LE("DittoFS")
		info := make([]byte, 18+len(label))
		binary.LittleEndian.PutUint64(info[0:8], types.NowFiletime())
		binary.LittleEndian.PutUint32(info[8:12], uint32(ntfsVolumeSerialNumber)) // VolumeSerialNumber
		binary.LittleEndian.PutUint32(info[12:16], uint32(len(label)))
		// SupportsObjects (1 byte at 16) and Reserved (1 byte at 17) are zero
		copy(info[18:], label)
		return info, nil

	case 2: // FileFsLabelInformation [MS-FSCC] 2.5.5
		label := encodeUTF16LE("DittoFS")
		info := make([]byte, 4+len(label))
		binary.LittleEndian.PutUint32(info[0:4], uint32(len(label)))
		copy(info[4:], label)
		return info, nil

	case 3: // FileFsSizeInformation [MS-FSCC] 2.5.8
		stats, err := metaSvc.GetFilesystemStatistics(ctx, handle)
		if err == nil {
			totalBlocks := stats.TotalBytes / clusterSize
			availBlocks := stats.AvailableBytes / clusterSize
			info := make([]byte, 24)
			binary.LittleEndian.PutUint64(info[0:8], totalBlocks)
			binary.LittleEndian.PutUint64(info[8:16], availBlocks)
			binary.LittleEndian.PutUint32(info[16:20], sectorsPerUnit)
			binary.LittleEndian.PutUint32(info[20:24], bytesPerSector)
			return info, nil
		}
		// Fallback to hardcoded values
		info := make([]byte, 24)
		binary.LittleEndian.PutUint64(info[0:8], 1000000)
		binary.LittleEndian.PutUint64(info[8:16], 500000)
		binary.LittleEndian.PutUint32(info[16:20], sectorsPerUnit)
		binary.LittleEndian.PutUint32(info[20:24], bytesPerSector)
		return info, nil

	case 4: // FileFsDeviceInformation [MS-FSCC] 2.5.9
		// DeviceType (4 bytes) + Characteristics (4 bytes) = 8 bytes
		info := make([]byte, 8)
		binary.LittleEndian.PutUint32(info[0:4], 0x00000007) // FILE_DEVICE_DISK
		binary.LittleEndian.PutUint32(info[4:8], 0x00000000) // No special characteristics
		return info, nil

	case 5: // FileFsAttributeInformation [MS-FSCC] 2.5.1
		fsName := encodeUTF16LE("NTFS")
		info := make([]byte, 12+len(fsName))
		binary.LittleEndian.PutUint32(info[0:4], 0x000000CF) // FILE_CASE_SENSITIVE_SEARCH | FILE_CASE_PRESERVED_NAMES | FILE_UNICODE_ON_DISK | FILE_PERSISTENT_ACLS | FILE_SUPPORTS_SPARSE_FILES | FILE_SUPPORTS_REPARSE_POINTS
		binary.LittleEndian.PutUint32(info[4:8], 255)
		binary.LittleEndian.PutUint32(info[8:12], uint32(len(fsName)))
		copy(info[12:], fsName)
		return info, nil

	case 7: // FileFsFullSizeInformation [MS-FSCC] 2.5.4
		stats, err := metaSvc.GetFilesystemStatistics(ctx, handle)
		if err == nil {
			totalBlocks := stats.TotalBytes / clusterSize
			availBlocks := stats.AvailableBytes / clusterSize
			info := make([]byte, 32)
			binary.LittleEndian.PutUint64(info[0:8], totalBlocks)
			binary.LittleEndian.PutUint64(info[8:16], availBlocks)
			binary.LittleEndian.PutUint64(info[16:24], availBlocks)
			binary.LittleEndian.PutUint32(info[24:28], sectorsPerUnit)
			binary.LittleEndian.PutUint32(info[28:32], bytesPerSector)
			return info, nil
		}
		// Fallback
		info := make([]byte, 32)
		binary.LittleEndian.PutUint64(info[0:8], 1000000)
		binary.LittleEndian.PutUint64(info[8:16], 500000)
		binary.LittleEndian.PutUint64(info[16:24], 500000)
		binary.LittleEndian.PutUint32(info[24:28], sectorsPerUnit)
		binary.LittleEndian.PutUint32(info[28:32], bytesPerSector)
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

// fileInfoClassMinSize returns the minimum output buffer size required for a
// fixed-size file information class. Returns 0 for variable-length classes
// (which may be truncated instead of rejected).
func fileInfoClassMinSize(class types.FileInfoClass) uint32 {
	switch class {
	case types.FileBasicInformation:
		return 40
	case types.FileStandardInformation:
		return 24
	case types.FileInternalInformation, types.FilePositionInformation:
		return 8
	case types.FileEaInformation, types.FileAccessInformation,
		types.FileModeInformation, types.FileAlignmentInformation:
		return 4
	case types.FileCompressionInformation:
		return 16
	case types.FileNetworkOpenInformation:
		return 56
	case types.FileAttributeTagInformation:
		return 8
	default:
		return 0 // Variable-length or unknown; allow truncation
	}
}

// toSMBPath converts a forward-slash share-relative path to SMB backslash format
// with a leading backslash. An empty path (share root) returns "\".
func toSMBPath(path string) string {
	if path == "" {
		return "\\"
	}
	return "\\" + strings.ReplaceAll(path, "/", "\\")
}

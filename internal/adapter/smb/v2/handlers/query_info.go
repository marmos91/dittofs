package handlers

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
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

	r := smbenc.NewReader(body)
	_ = r.ReadUint16()                   // StructureSize (always 41)
	infoType := r.ReadUint8()            // InfoType
	fileInfoClass := r.ReadUint8()       // FileInfoClass
	outputBufferLength := r.ReadUint32() // OutputBufferLength
	inputBufferOffset := r.ReadUint16()  // InputBufferOffset
	_ = r.ReadUint16()                   // Reserved
	inputBufferLength := r.ReadUint32()  // InputBufferLength
	additionalInfo := r.ReadUint32()     // AdditionalInfo
	flags := r.ReadUint32()              // Flags
	fileID := r.ReadBytes(16)            // FileID

	req := &QueryInfoRequest{
		InfoType:           infoType,
		FileInfoClass:      fileInfoClass,
		OutputBufferLength: outputBufferLength,
		InputBufferOffset:  inputBufferOffset,
		InputBufferLength:  inputBufferLength,
		AdditionalInfo:     additionalInfo,
		Flags:              flags,
	}
	if fileID != nil {
		copy(req.FileID[:], fileID)
	}

	return req, nil
}

// Encode serializes the QueryInfoResponse into SMB2 wire format [MS-SMB2] 2.2.38.
//
// Per MS-SMB2 convention, StructureSize is 9 but the actual fixed part is 8 bytes
// (StructureSize(2) + OutputBufferOffset(2) + OutputBufferLength(4)). The variable
// buffer starts at offset 8 from the body, which is 64+8=72 from the header.
func (resp *QueryInfoResponse) Encode() ([]byte, error) {
	w := smbenc.NewWriter(8 + len(resp.Data))
	w.WriteUint16(9)                      // StructureSize (per spec, always 9)
	w.WriteUint16(uint16(64 + 8))         // OutputBufferOffset (header + fixed part)
	w.WriteUint32(uint32(len(resp.Data))) // OutputBufferLength
	w.WriteBytes(resp.Data)

	return w.Bytes(), nil
}

// EncodeFileBasicInfo builds FILE_BASIC_INFORMATION [MS-FSCC] 2.4.7.
func EncodeFileBasicInfo(info *FileBasicInfo) []byte {
	w := smbenc.NewWriter(40)
	w.WriteUint64(types.TimeToFiletime(info.CreationTime))
	w.WriteUint64(types.TimeToFiletime(info.LastAccessTime))
	w.WriteUint64(types.TimeToFiletime(info.LastWriteTime))
	w.WriteUint64(types.TimeToFiletime(info.ChangeTime))
	w.WriteUint32(uint32(info.FileAttributes))
	w.WriteZeros(4) // Reserved
	return w.Bytes()
}

// DecodeFileBasicInfo parses FILE_BASIC_INFORMATION [MS-FSCC] 2.4.7.
func DecodeFileBasicInfo(buf []byte) (*FileBasicInfo, error) {
	if len(buf) < 40 {
		return nil, fmt.Errorf("buffer too short for FILE_BASIC_INFORMATION: %d bytes", len(buf))
	}

	r := smbenc.NewReader(buf)
	return &FileBasicInfo{
		CreationTime:   types.FiletimeToTime(r.ReadUint64()),
		LastAccessTime: types.FiletimeToTime(r.ReadUint64()),
		LastWriteTime:  types.FiletimeToTime(r.ReadUint64()),
		ChangeTime:     types.FiletimeToTime(r.ReadUint64()),
		FileAttributes: types.FileAttributes(r.ReadUint32()),
	}, nil
}

// EncodeFileStandardInfo builds FILE_STANDARD_INFORMATION [MS-FSCC] 2.4.41.
func EncodeFileStandardInfo(info *FileStandardInfo) []byte {
	w := smbenc.NewWriter(24)
	w.WriteUint64(info.AllocationSize)
	w.WriteUint64(info.EndOfFile)
	w.WriteUint32(info.NumberOfLinks)
	var deletePending, directory uint8
	if info.DeletePending {
		deletePending = 1
	}
	if info.Directory {
		directory = 1
	}
	w.WriteUint8(deletePending)
	w.WriteUint8(directory)
	w.WriteZeros(2) // Reserved
	return w.Bytes()
}

// EncodeFileNetworkOpenInfo builds FILE_NETWORK_OPEN_INFORMATION [MS-FSCC] 2.4.27.
func EncodeFileNetworkOpenInfo(info *FileNetworkOpenInfo) []byte {
	w := smbenc.NewWriter(56)
	w.WriteUint64(types.TimeToFiletime(info.CreationTime))
	w.WriteUint64(types.TimeToFiletime(info.LastAccessTime))
	w.WriteUint64(types.TimeToFiletime(info.LastWriteTime))
	w.WriteUint64(types.TimeToFiletime(info.ChangeTime))
	w.WriteUint64(info.AllocationSize)
	w.WriteUint64(info.EndOfFile)
	w.WriteUint32(uint32(info.FileAttributes))
	w.WriteZeros(4) // Reserved
	return w.Bytes()
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
			wp := smbenc.NewWriter(4)
			wp.WriteUint32(uint32(actualNameLen))
			copy(info[96:100], wp.Bytes())
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
		w := smbenc.NewWriter(4)
		w.WriteUint32(0x001F01FF) // Full access
		return &QueryInfoResponse{
			SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
			Data:            w.Bytes(),
		}, nil

	case types.FileNameInformation:
		nameBytes := encodeUTF16LE("\\" + openFile.PipeName)
		w := smbenc.NewWriter(4 + len(nameBytes))
		w.WriteUint32(uint32(len(nameBytes)))
		w.WriteBytes(nameBytes)
		return &QueryInfoResponse{
			SMBResponseBase: SMBResponseBase{Status: types.StatusSuccess},
			Data:            w.Bytes(),
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
		wAccess := smbenc.NewWriter(4)
		wAccess.WriteUint32(0x001F01FF)
		copy(info[76:80], wAccess.Bytes())
		// PositionInformation (8 bytes) at offset 80 - zero
		// ModeInformation (4 bytes) at offset 88 - zero
		// AlignmentInformation (4 bytes) at offset 92 - zero
		// NameInformation: length (4 bytes) at offset 96 + name data
		wName := smbenc.NewWriter(4)
		wName.WriteUint32(uint32(len(nameBytes)))
		copy(info[96:100], wName.Bytes())
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
		// Convert UUID to uint64 by using first 8 bytes
		r := smbenc.NewReader(file.ID[:8])
		fileIndex := r.ReadUint64()
		w := smbenc.NewWriter(8)
		w.WriteUint64(fileIndex) // IndexNumber (unique file ID)
		return w.Bytes(), nil

	case types.FileEaInformation:
		// FILE_EA_INFORMATION [MS-FSCC] 2.4.12 (4 bytes)
		return make([]byte, 4), nil // EaSize = 0

	case types.FileAccessInformation:
		// FILE_ACCESS_INFORMATION [MS-FSCC] 2.4.1 (4 bytes)
		w := smbenc.NewWriter(4)
		w.WriteUint32(0x001F01FF) // AccessFlags (full access)
		return w.Bytes(), nil

	case types.FileStreamInformation:
		// FileStreamInformation [MS-FSCC] 2.4.44
		// Return the default unnamed data stream (::$DATA)
		streamName := []byte{':', 0, ':', 0, '$', 0, 'D', 0, 'A', 0, 'T', 0, 'A', 0} // "::$DATA" UTF-16LE
		size := getSMBSize(&file.FileAttr)
		alloc := calculateAllocationSize(size)
		w := smbenc.NewWriter(24 + len(streamName))
		w.WriteUint32(0)                       // NextEntryOffset (last entry)
		w.WriteUint32(uint32(len(streamName))) // StreamNameLength
		w.WriteUint64(size)                    // StreamSize
		w.WriteUint64(alloc)                   // StreamAllocationSize
		w.WriteBytes(streamName)
		return w.Bytes(), nil

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
		modeMask := types.FileWriteThrough | types.FileSequentialOnly |
			types.FileNoIntermediateBuffering | types.FileSynchronousIoAlert |
			types.FileSynchronousIoNonalert | types.FileDeleteOnClose
		mode := openFile.CreateOptions & modeMask
		w := smbenc.NewWriter(4)
		w.WriteUint32(uint32(mode))
		return w.Bytes(), nil

	case types.FileAlignmentInformation:
		// FILE_ALIGNMENT_INFORMATION [MS-FSCC] 2.4.3 (4 bytes)
		return make([]byte, 4), nil // AlignmentRequirement = 0 (byte-aligned)

	case types.FileNameInformation:
		// FILE_NAME_INFORMATION [MS-FSCC] 2.4.26 (4 bytes + variable)
		nameBytes := encodeUTF16LE(toSMBPath(openFile.Path))
		w := smbenc.NewWriter(4 + len(nameBytes))
		w.WriteUint32(uint32(len(nameBytes))) // FileNameLength
		w.WriteBytes(nameBytes)
		return w.Bytes(), nil

	case types.FileAlternateNameInformation:
		// FILE_ALTERNATE_NAME_INFORMATION [MS-FSCC] 2.4.5 (4 bytes + variable)
		// Returns the 8.3 short name
		shortNameBytes := generate83ShortName(openFile.FileName)
		if shortNameBytes == nil {
			// For root or entries without a short name, use the filename itself
			shortNameBytes = encodeUTF16LE(openFile.FileName)
		}
		w := smbenc.NewWriter(4 + len(shortNameBytes))
		w.WriteUint32(uint32(len(shortNameBytes))) // FileNameLength
		w.WriteBytes(shortNameBytes)
		return w.Bytes(), nil

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
		w := smbenc.NewWriter(4 + len(nameBytes))
		w.WriteUint32(uint32(len(nameBytes))) // FileNameLength
		w.WriteBytes(nameBytes)
		return w.Bytes(), nil

	case types.FileIdInformation:
		// FILE_ID_INFORMATION [MS-FSCC] 2.4.46 (24 bytes)
		// VolumeSerialNumber (8 bytes) + FileId (16 bytes)
		w := smbenc.NewWriter(24)
		w.WriteUint64(ntfsVolumeSerialNumber) // VolumeSerialNumber
		w.WriteBytes(file.ID[:16])            // FileId (128-bit)
		return w.Bytes(), nil

	case types.FileCompressionInformation:
		// FILE_COMPRESSION_INFORMATION [MS-FSCC] 2.4.9 (16 bytes)
		// CompressedFileSize (8) + CompressionFormat (2) + CompressionUnitShift (1) +
		// ChunkShift (1) + ClusterShift (1) + Reserved (3)
		size := getSMBSize(&file.FileAttr)
		w := smbenc.NewWriter(16)
		w.WriteUint64(size) // CompressedFileSize = EndOfFile
		w.WriteZeros(8)     // CompressionFormat(2) + shifts(3) + Reserved(3) all zero = COMPRESSION_FORMAT_NONE
		return w.Bytes(), nil

	case types.FileAttributeTagInformation:
		// FILE_ATTRIBUTE_TAG_INFORMATION [MS-FSCC] 2.4.6 (8 bytes)
		// FileAttributes (4) + ReparseTag (4)
		attrs := FileAttrToSMBAttributes(&file.FileAttr)
		w := smbenc.NewWriter(8)
		w.WriteUint32(uint32(attrs))
		w.WriteUint32(0) // ReparseTag = 0 for non-reparse points
		return w.Bytes(), nil

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

	// Build remaining fields sequentially using smbenc Writer
	r := smbenc.NewReader(file.ID[:8])
	fileIndex := r.ReadUint64()

	w := smbenc.NewWriter(36)
	w.WriteUint64(fileIndex)              // InternalInformation (8 bytes) at offset 64
	w.WriteUint32(0)                      // EaInformation (4 bytes) at offset 72
	w.WriteUint32(0x001F01FF)             // AccessInformation (4 bytes) at offset 76
	w.WriteUint64(0)                      // PositionInformation (8 bytes) at offset 80
	w.WriteUint32(0)                      // ModeInformation (4 bytes) at offset 88
	w.WriteUint32(0)                      // AlignmentInformation (4 bytes) at offset 92
	w.WriteUint32(uint32(len(nameBytes))) // NameInformation length at offset 96
	copy(info[64:100], w.Bytes())

	copy(info[100:], nameBytes)

	return info
}

// buildFilesystemInfo builds filesystem information [MS-FSCC] 2.5.
func (h *Handler) buildFilesystemInfo(ctx context.Context, class types.FileInfoClass, metaSvc *metadata.MetadataService, handle metadata.FileHandle) ([]byte, error) {
	switch class {
	case 1: // FileFsVolumeInformation [MS-FSCC] 2.5.9
		label := encodeUTF16LE("DittoFS")
		w := smbenc.NewWriter(18 + len(label))
		w.WriteUint64(types.NowFiletime())
		w.WriteUint32(uint32(ntfsVolumeSerialNumber)) // VolumeSerialNumber
		w.WriteUint32(uint32(len(label)))
		w.WriteUint8(0) // SupportsObjects
		w.WriteUint8(0) // Reserved
		w.WriteBytes(label)
		return w.Bytes(), nil

	case 2: // FileFsLabelInformation [MS-FSCC] 2.5.5
		label := encodeUTF16LE("DittoFS")
		w := smbenc.NewWriter(4 + len(label))
		w.WriteUint32(uint32(len(label)))
		w.WriteBytes(label)
		return w.Bytes(), nil

	case 3: // FileFsSizeInformation [MS-FSCC] 2.5.8
		stats, err := metaSvc.GetFilesystemStatistics(ctx, handle)
		if err == nil {
			totalBlocks := stats.TotalBytes / clusterSize
			availBlocks := stats.AvailableBytes / clusterSize
			w := smbenc.NewWriter(24)
			w.WriteUint64(totalBlocks)
			w.WriteUint64(availBlocks)
			w.WriteUint32(sectorsPerUnit)
			w.WriteUint32(bytesPerSector)
			return w.Bytes(), nil
		}
		// Fallback to hardcoded values
		w := smbenc.NewWriter(24)
		w.WriteUint64(1000000)
		w.WriteUint64(500000)
		w.WriteUint32(sectorsPerUnit)
		w.WriteUint32(bytesPerSector)
		return w.Bytes(), nil

	case 4: // FileFsDeviceInformation [MS-FSCC] 2.5.9
		// DeviceType (4 bytes) + Characteristics (4 bytes) = 8 bytes
		w := smbenc.NewWriter(8)
		w.WriteUint32(0x00000007) // FILE_DEVICE_DISK
		w.WriteUint32(0x00000000) // No special characteristics
		return w.Bytes(), nil

	case 5: // FileFsAttributeInformation [MS-FSCC] 2.5.1
		fsName := encodeUTF16LE("NTFS")
		w := smbenc.NewWriter(12 + len(fsName))
		w.WriteUint32(0x000000CF) // FILE_CASE_SENSITIVE_SEARCH | FILE_CASE_PRESERVED_NAMES | FILE_UNICODE_ON_DISK | FILE_PERSISTENT_ACLS | FILE_SUPPORTS_SPARSE_FILES | FILE_SUPPORTS_REPARSE_POINTS
		w.WriteUint32(255)
		w.WriteUint32(uint32(len(fsName)))
		w.WriteBytes(fsName)
		return w.Bytes(), nil

	case 7: // FileFsFullSizeInformation [MS-FSCC] 2.5.4
		stats, err := metaSvc.GetFilesystemStatistics(ctx, handle)
		if err == nil {
			totalBlocks := stats.TotalBytes / clusterSize
			availBlocks := stats.AvailableBytes / clusterSize
			w := smbenc.NewWriter(32)
			w.WriteUint64(totalBlocks)
			w.WriteUint64(availBlocks)
			w.WriteUint64(availBlocks)
			w.WriteUint32(sectorsPerUnit)
			w.WriteUint32(bytesPerSector)
			return w.Bytes(), nil
		}
		// Fallback
		w := smbenc.NewWriter(32)
		w.WriteUint64(1000000)
		w.WriteUint64(500000)
		w.WriteUint64(500000)
		w.WriteUint32(sectorsPerUnit)
		w.WriteUint32(bytesPerSector)
		return w.Bytes(), nil

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
		bps := uint32(512) // bytes per sector
		w := smbenc.NewWriter(28)
		w.WriteUint32(bps)        // LogicalBytesPerSector
		w.WriteUint32(bps)        // PhysicalBytesPerSectorForAtomicity
		w.WriteUint32(bps)        // PhysicalBytesPerSectorForPerformance
		w.WriteUint32(bps)        // FileSystemEffectivePhysicalBytesPerSectorForAtomicity
		w.WriteUint32(0x00000003) // Flags: ALIGNED_DEVICE | PARTITION_ALIGNED
		w.WriteUint32(0)          // ByteOffsetForSectorAlignment
		w.WriteUint32(0)          // ByteOffsetForPartitionAlignment
		return w.Bytes(), nil

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

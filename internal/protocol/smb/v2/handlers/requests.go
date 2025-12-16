// Package handlers provides SMB2 command handlers and session management.
package handlers

import (
	"time"

	"github.com/marmos91/dittofs/internal/protocol/smb/types"
)

// CreateRequest represents a parsed SMB2 CREATE request [MS-SMB2] 2.2.13
type CreateRequest struct {
	OplockLevel        uint8
	ImpersonationLevel uint32
	DesiredAccess      uint32
	FileAttributes     types.FileAttributes
	ShareAccess        uint32
	CreateDisposition  types.CreateDisposition
	CreateOptions      types.CreateOptions
	FileName           string
	CreateContexts     []CreateContext
}

// CreateContext represents an SMB2 Create Context
type CreateContext struct {
	Name string
	Data []byte
}

// CreateResponse represents an SMB2 CREATE response [MS-SMB2] 2.2.14
type CreateResponse struct {
	OplockLevel    uint8
	Flags          uint8
	CreateAction   types.CreateAction
	CreationTime   time.Time
	LastAccessTime time.Time
	LastWriteTime  time.Time
	ChangeTime     time.Time
	AllocationSize uint64
	EndOfFile      uint64
	FileAttributes types.FileAttributes
	FileID         [16]byte
	CreateContexts []CreateContext
}

// ReadRequest represents a parsed SMB2 READ request [MS-SMB2] 2.2.19
type ReadRequest struct {
	Padding            uint8
	Flags              uint8
	Length             uint32
	Offset             uint64
	FileID             [16]byte
	MinimumCount       uint32
	Channel            uint32
	RemainingBytes     uint32
	ReadChannelInfoBuf []byte
}

// ReadResponse represents an SMB2 READ response [MS-SMB2] 2.2.20
type ReadResponse struct {
	DataOffset    uint8
	DataLength    uint32
	DataRemaining uint32
	Data          []byte
}

// WriteRequest represents a parsed SMB2 WRITE request [MS-SMB2] 2.2.21
type WriteRequest struct {
	DataOffset     uint16
	Length         uint32
	Offset         uint64
	FileID         [16]byte
	Channel        uint32
	RemainingBytes uint32
	Flags          uint32
	Data           []byte
}

// WriteResponse represents an SMB2 WRITE response [MS-SMB2] 2.2.22
type WriteResponse struct {
	Count     uint32
	Remaining uint32
}

// CloseRequest represents a parsed SMB2 CLOSE request [MS-SMB2] 2.2.15
type CloseRequest struct {
	Flags  uint16
	FileID [16]byte
}

// CloseResponse represents an SMB2 CLOSE response [MS-SMB2] 2.2.16
type CloseResponse struct {
	Flags          uint16
	CreationTime   time.Time
	LastAccessTime time.Time
	LastWriteTime  time.Time
	ChangeTime     time.Time
	AllocationSize uint64
	EndOfFile      uint64
	FileAttributes types.FileAttributes
}

// QueryInfoRequest represents a parsed SMB2 QUERY_INFO request [MS-SMB2] 2.2.37
type QueryInfoRequest struct {
	InfoType           uint8
	FileInfoClass      uint8
	OutputBufferLength uint32
	InputBufferOffset  uint16
	InputBufferLength  uint32
	AdditionalInfo     uint32
	Flags              uint32
	FileID             [16]byte
}

// QueryInfoResponse represents an SMB2 QUERY_INFO response [MS-SMB2] 2.2.38
type QueryInfoResponse struct {
	OutputBufferOffset uint16
	OutputBufferLength uint32
	Data               []byte
}

// SetInfoRequest represents a parsed SMB2 SET_INFO request [MS-SMB2] 2.2.39
type SetInfoRequest struct {
	InfoType       uint8
	FileInfoClass  uint8
	BufferLength   uint32
	BufferOffset   uint16
	AdditionalInfo uint32
	FileID         [16]byte
	Buffer         []byte
}

// SetInfoResponse represents an SMB2 SET_INFO response [MS-SMB2] 2.2.40
// Note: SET_INFO response is just a status with no body
type SetInfoResponse struct{}

// QueryDirectoryRequest represents a parsed SMB2 QUERY_DIRECTORY request [MS-SMB2] 2.2.33
type QueryDirectoryRequest struct {
	FileInfoClass      uint8
	Flags              uint8
	FileIndex          uint32
	FileID             [16]byte
	FileNameOffset     uint16
	FileNameLength     uint16
	OutputBufferLength uint32
	FileName           string
}

// QueryDirectoryResponse represents an SMB2 QUERY_DIRECTORY response [MS-SMB2] 2.2.34
type QueryDirectoryResponse struct {
	OutputBufferOffset uint16
	OutputBufferLength uint32
	Data               []byte
}

// FlushRequest represents a parsed SMB2 FLUSH request [MS-SMB2] 2.2.17
type FlushRequest struct {
	FileID [16]byte
}

// FlushResponse represents an SMB2 FLUSH response [MS-SMB2] 2.2.18
// Note: FLUSH response is just a status with no body
type FlushResponse struct{}

// File info classes for QUERY_INFO
const (
	FileBasicInformation           uint8 = 4
	FileStandardInformation        uint8 = 5
	FileInternalInformation        uint8 = 6
	FileEaInformation              uint8 = 7
	FileAccessInformation          uint8 = 8
	FileNameInformation            uint8 = 9
	FileRenameInformation          uint8 = 10
	FileDispositionInformation     uint8 = 13
	FilePositionInformation        uint8 = 14
	FileModeInformation            uint8 = 16
	FileAlignmentInformation       uint8 = 17
	FileAllInformation             uint8 = 18
	FileAllocationInformation      uint8 = 19
	FileEndOfFileInformation       uint8 = 20
	FileAttributeTagInformation    uint8 = 35
	FileNetworkOpenInformation     uint8 = 34
	FileIdBothDirectoryInformation uint8 = 37
	FileIdFullDirectoryInformation uint8 = 38
)

// Info types for QUERY_INFO
const (
	SMB2InfoFile       uint8 = 1
	SMB2InfoFilesystem uint8 = 2
	SMB2InfoSecurity   uint8 = 3
	SMB2InfoQuota      uint8 = 4
)

// FileBasicInfo represents FILE_BASIC_INFORMATION [MS-FSCC] 2.4.7
type FileBasicInfo struct {
	CreationTime   time.Time
	LastAccessTime time.Time
	LastWriteTime  time.Time
	ChangeTime     time.Time
	FileAttributes types.FileAttributes
}

// FileStandardInfo represents FILE_STANDARD_INFORMATION [MS-FSCC] 2.4.41
type FileStandardInfo struct {
	AllocationSize uint64
	EndOfFile      uint64
	NumberOfLinks  uint32
	DeletePending  bool
	Directory      bool
}

// FileAllInfo represents FILE_ALL_INFORMATION [MS-FSCC] 2.4.2
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

// FileNetworkOpenInfo represents FILE_NETWORK_OPEN_INFORMATION [MS-FSCC] 2.4.27
type FileNetworkOpenInfo struct {
	CreationTime   time.Time
	LastAccessTime time.Time
	LastWriteTime  time.Time
	ChangeTime     time.Time
	AllocationSize uint64
	EndOfFile      uint64
	FileAttributes types.FileAttributes
}

// DirectoryEntry represents a file entry in directory listing
type DirectoryEntry struct {
	FileName       string
	FileIndex      uint64
	CreationTime   time.Time
	LastAccessTime time.Time
	LastWriteTime  time.Time
	ChangeTime     time.Time
	EndOfFile      uint64
	AllocationSize uint64
	FileAttributes types.FileAttributes
	EaSize         uint32
	FileID         uint64
	ShortName      string
}

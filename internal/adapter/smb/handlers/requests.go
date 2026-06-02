// Package handlers provides SMB2 command handlers and session management.
//
// This file contains shared constants used by multiple SMB2 handlers.
//
// All request/response types have been moved to individual handler files:
//   - FlushRequest, FlushResponse -> flush.go
//   - CloseRequest, CloseResponse -> close.go
//   - ReadRequest, ReadResponse -> read.go
//   - WriteRequest, WriteResponse -> write.go
//   - CreateRequest, CreateResponse, CreateContext -> create.go
//   - QueryInfoRequest, QueryInfoResponse -> query_info.go
//   - FileBasicInfo, FileStandardInfo, FileAllInfo, FileNetworkOpenInfo -> query_info.go
//   - SetInfoRequest, SetInfoResponse, FileRenameInfo -> set_info.go
//   - QueryDirectoryRequest, QueryDirectoryResponse, DirectoryEntry -> query_directory.go
//   - LogoffRequest, LogoffResponse -> logoff.go
//   - EchoRequest, EchoResponse -> echo.go
package handlers

// ============================================================================
// Shared Constants - File Info Classes [MS-FSCC] Section 2.4
// ============================================================================

// File info classes for QUERY_INFO and SET_INFO commands [MS-FSCC] Section 2.4.
//
// These constants identify which information structure is being
// requested or set via QUERY_INFO and SET_INFO commands. They are
// used by multiple handlers.
const (
	// FileBasicInformation returns timestamps and attributes [MS-FSCC] 2.4.7.
	FileBasicInformation uint8 = 4

	// FileStandardInformation returns file size and link count [MS-FSCC] 2.4.41.
	FileStandardInformation uint8 = 5

	// FileInternalInformation returns the file index [MS-FSCC] 2.4.20.
	FileInternalInformation uint8 = 6

	// FileEaInformation returns extended attributes size [MS-FSCC] 2.4.12.
	FileEaInformation uint8 = 7

	// FileAccessInformation returns access flags [MS-FSCC] 2.4.1.
	FileAccessInformation uint8 = 8

	// FileNameInformation returns the file name [MS-FSCC] 2.4.24.
	FileNameInformation uint8 = 9

	// FileRenameInformation is used to rename files [MS-FSCC] 2.4.34.
	FileRenameInformation uint8 = 10

	// FileDispositionInformation is used to delete files [MS-FSCC] 2.4.11.
	FileDispositionInformation uint8 = 13

	// FilePositionInformation returns current position [MS-FSCC] 2.4.32.
	FilePositionInformation uint8 = 14

	// FileModeInformation returns file mode [MS-FSCC] 2.4.24.
	FileModeInformation uint8 = 16

	// FileAlignmentInformation returns alignment requirement [MS-FSCC] 2.4.3.
	FileAlignmentInformation uint8 = 17

	// FileAllInformation returns all basic info [MS-FSCC] 2.4.2.
	FileAllInformation uint8 = 18

	// FileAllocationInformation is used to set allocation size [MS-FSCC] 2.4.4.
	FileAllocationInformation uint8 = 19

	// FileEndOfFileInformation is used to set file size [MS-FSCC] 2.4.13.
	FileEndOfFileInformation uint8 = 20

	// FileAttributeTagInformation returns reparse point info [MS-FSCC] 2.4.6.
	FileAttributeTagInformation uint8 = 35

	// FileNetworkOpenInformation returns network open info [MS-FSCC] 2.4.27.
	FileNetworkOpenInformation uint8 = 34

	// FileIdBothDirectoryInformation for directory listings [MS-FSCC] 2.4.17.
	FileIdBothDirectoryInformation uint8 = 37

	// FileIdFullDirectoryInformation for directory listings [MS-FSCC] 2.4.18.
	FileIdFullDirectoryInformation uint8 = 38
)

// ============================================================================
// Shared Constants - Info Types [MS-SMB2] 2.2.37
// ============================================================================

// Info types for QUERY_INFO command [MS-SMB2] 2.2.37.
//
// These constants identify which type of information is being requested.
const (
	// SMB2InfoFile queries file/directory information.
	SMB2InfoFile uint8 = 1

	// SMB2InfoFilesystem queries filesystem information.
	SMB2InfoFilesystem uint8 = 2

	// SMB2InfoSecurity queries security descriptor.
	SMB2InfoSecurity uint8 = 3

	// SMB2InfoQuota queries quota information.
	SMB2InfoQuota uint8 = 4
)

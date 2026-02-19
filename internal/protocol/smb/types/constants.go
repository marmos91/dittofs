// Package types contains SMB2 protocol constants, types, and error codes.
//
// This package provides type-safe definitions for SMB2 protocol elements including:
//   - Command codes (NEGOTIATE, SESSION_SETUP, CREATE, READ, WRITE, etc.)
//   - Header flags (response, async, signed, related operations)
//   - Dialects (SMB 2.0.2, 2.1, 3.0, 3.0.2, 3.1.1)
//   - File attributes, access masks, and create options
//
// All types use explicit Go types (e.g., Command, HeaderFlags) to enable
// IDE autocomplete and prevent mixing incompatible values.
//
// Reference: [MS-SMB2] - Server Message Block (SMB) Protocol Versions 2 and 3
// https://docs.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/
package types

// =============================================================================
// Protocol Identifiers
// =============================================================================

// SMB1ProtocolID is the SMB1 protocol identifier (little-endian: 0xFF 'S' 'M' 'B')
// Used to detect legacy SMB1 connections that need protocol upgrade.
const SMB1ProtocolID uint32 = 0x424D53FF

// SMB2ProtocolID is the SMB2 protocol identifier (little-endian: 0xFE 'S' 'M' 'B')
// All SMB2/3 messages begin with this 4-byte signature.
const SMB2ProtocolID uint32 = 0x424D53FE

// =============================================================================
// Command Codes
// =============================================================================

// Command represents an SMB2 command code.
// [MS-SMB2] Section 2.2.1
type Command uint16

const (
	// CommandNegotiate initiates protocol negotiation between client and server.
	// This is always the first command sent by the client.
	CommandNegotiate Command = 0x0000

	// CommandSessionSetup authenticates the client and establishes a session.
	// Typically uses NTLM or Kerberos via SPNEGO.
	CommandSessionSetup Command = 0x0001

	// CommandLogoff terminates a session established by SESSION_SETUP.
	CommandLogoff Command = 0x0002

	// CommandTreeConnect connects to a share (e.g., \\server\share).
	CommandTreeConnect Command = 0x0003

	// CommandTreeDisconnect disconnects from a share.
	CommandTreeDisconnect Command = 0x0004

	// CommandCreate opens or creates a file/directory.
	// This is the primary command for accessing filesystem objects.
	CommandCreate Command = 0x0005

	// CommandClose closes a file handle opened by CREATE.
	CommandClose Command = 0x0006

	// CommandFlush flushes cached data for a file to stable storage.
	CommandFlush Command = 0x0007

	// CommandRead reads data from a file.
	CommandRead Command = 0x0008

	// CommandWrite writes data to a file.
	CommandWrite Command = 0x0009

	// CommandLock requests byte-range locks on a file.
	CommandLock Command = 0x000A

	// CommandIoctl sends a control code to a device or filesystem.
	CommandIoctl Command = 0x000B

	// CommandCancel cancels a pending request.
	CommandCancel Command = 0x000C

	// CommandEcho tests connectivity (ping/pong).
	CommandEcho Command = 0x000D

	// CommandQueryDirectory enumerates directory contents.
	CommandQueryDirectory Command = 0x000E

	// CommandChangeNotify registers for directory change notifications.
	CommandChangeNotify Command = 0x000F

	// CommandQueryInfo retrieves file/directory/filesystem information.
	CommandQueryInfo Command = 0x0010

	// CommandSetInfo sets file/directory/filesystem information.
	CommandSetInfo Command = 0x0011

	// CommandOplockBreak handles oplock break notifications.
	CommandOplockBreak Command = 0x0012
)

// String returns the human-readable name of the command.
func (c Command) String() string {
	switch c {
	case CommandNegotiate:
		return "NEGOTIATE"
	case CommandSessionSetup:
		return "SESSION_SETUP"
	case CommandLogoff:
		return "LOGOFF"
	case CommandTreeConnect:
		return "TREE_CONNECT"
	case CommandTreeDisconnect:
		return "TREE_DISCONNECT"
	case CommandCreate:
		return "CREATE"
	case CommandClose:
		return "CLOSE"
	case CommandFlush:
		return "FLUSH"
	case CommandRead:
		return "READ"
	case CommandWrite:
		return "WRITE"
	case CommandLock:
		return "LOCK"
	case CommandIoctl:
		return "IOCTL"
	case CommandCancel:
		return "CANCEL"
	case CommandEcho:
		return "ECHO"
	case CommandQueryDirectory:
		return "QUERY_DIRECTORY"
	case CommandChangeNotify:
		return "CHANGE_NOTIFY"
	case CommandQueryInfo:
		return "QUERY_INFO"
	case CommandSetInfo:
		return "SET_INFO"
	case CommandOplockBreak:
		return "OPLOCK_BREAK"
	default:
		return "UNKNOWN"
	}
}

// Legacy aliases for backward compatibility
const (
	SMB2Negotiate      = CommandNegotiate
	SMB2SessionSetup   = CommandSessionSetup
	SMB2Logoff         = CommandLogoff
	SMB2TreeConnect    = CommandTreeConnect
	SMB2TreeDisconnect = CommandTreeDisconnect
	SMB2Create         = CommandCreate
	SMB2Close          = CommandClose
	SMB2Flush          = CommandFlush
	SMB2Read           = CommandRead
	SMB2Write          = CommandWrite
	SMB2Lock           = CommandLock
	SMB2Ioctl          = CommandIoctl
	SMB2Cancel         = CommandCancel
	SMB2Echo           = CommandEcho
	SMB2QueryDirectory = CommandQueryDirectory
	SMB2ChangeNotify   = CommandChangeNotify
	SMB2QueryInfo      = CommandQueryInfo
	SMB2SetInfo        = CommandSetInfo
	SMB2OplockBreak    = CommandOplockBreak
)

// =============================================================================
// Header Flags
// =============================================================================

// HeaderFlags represents SMB2 header flags.
// [MS-SMB2] Section 2.2.1.1
type HeaderFlags uint32

const (
	// FlagResponse indicates this is a response from server to client.
	// Set by the server; clients should never set this flag.
	FlagResponse HeaderFlags = 0x00000001

	// FlagAsync indicates an asynchronous operation.
	// When set, the header contains AsyncId instead of ProcessId/TreeId.
	FlagAsync HeaderFlags = 0x00000002

	// FlagRelated indicates a related operation in a compound request.
	// The FileId from the previous operation is used if this flag is set.
	FlagRelated HeaderFlags = 0x00000004

	// FlagSigned indicates the message is signed.
	// The Signature field contains a valid signature.
	FlagSigned HeaderFlags = 0x00000008

	// FlagPriorityMask is a mask for the priority value (bits 4-6).
	// Used for I/O priority in SMB 3.1.1.
	FlagPriorityMask HeaderFlags = 0x00000070

	// FlagDFS indicates DFS (Distributed File System) operations.
	FlagDFS HeaderFlags = 0x10000000

	// FlagReplay indicates a replay operation.
	// Used for idempotent request replay in SMB 3.x.
	FlagReplay HeaderFlags = 0x20000000
)

// Has returns true if the flags contain the specified flag.
func (f HeaderFlags) Has(flag HeaderFlags) bool {
	return f&flag != 0
}

// IsResponse returns true if this is a response message.
func (f HeaderFlags) IsResponse() bool {
	return f.Has(FlagResponse)
}

// IsAsync returns true if this is an async message.
func (f HeaderFlags) IsAsync() bool {
	return f.Has(FlagAsync)
}

// IsRelated returns true if this is a related operation.
func (f HeaderFlags) IsRelated() bool {
	return f.Has(FlagRelated)
}

// IsSigned returns true if the message is signed.
func (f HeaderFlags) IsSigned() bool {
	return f.Has(FlagSigned)
}

// Legacy aliases for backward compatibility
const (
	SMB2FlagsServerToRedir   = uint32(FlagResponse)
	SMB2FlagsAsyncCommand    = uint32(FlagAsync)
	SMB2FlagsRelatedOps      = uint32(FlagRelated)
	SMB2FlagsSigned          = uint32(FlagSigned)
	SMB2FlagsPriorityMask    = uint32(FlagPriorityMask)
	SMB2FlagsDfsOperations   = uint32(FlagDFS)
	SMB2FlagsReplayOperation = uint32(FlagReplay)
)

// =============================================================================
// Dialects
// =============================================================================

// Dialect represents an SMB2/3 protocol dialect.
// [MS-SMB2] Section 2.2.3
type Dialect uint16

const (
	// Dialect0202 is SMB 2.0.2 (Windows Vista/Server 2008).
	// This is the minimum dialect for SMB2 support.
	Dialect0202 Dialect = 0x0202

	// Dialect0210 is SMB 2.1 (Windows 7/Server 2008 R2).
	// Adds leasing and large MTU support.
	Dialect0210 Dialect = 0x0210

	// Dialect0300 is SMB 3.0 (Windows 8/Server 2012).
	// Adds multichannel, encryption, and persistent handles.
	Dialect0300 Dialect = 0x0300

	// Dialect0302 is SMB 3.0.2 (Windows 8.1/Server 2012 R2).
	// Adds signing/encryption improvements.
	Dialect0302 Dialect = 0x0302

	// Dialect0311 is SMB 3.1.1 (Windows 10/Server 2016+).
	// Adds pre-auth integrity and AES-128-GCM encryption.
	Dialect0311 Dialect = 0x0311

	// DialectWildcard indicates the client supports multiple dialects.
	// Server selects the highest mutually supported dialect.
	DialectWildcard Dialect = 0x02FF
)

// String returns a human-readable dialect name.
func (d Dialect) String() string {
	switch d {
	case Dialect0202:
		return "SMB 2.0.2"
	case Dialect0210:
		return "SMB 2.1"
	case Dialect0300:
		return "SMB 3.0"
	case Dialect0302:
		return "SMB 3.0.2"
	case Dialect0311:
		return "SMB 3.1.1"
	case DialectWildcard:
		return "SMB 2.x (wildcard)"
	default:
		return "Unknown"
	}
}

// Legacy aliases for backward compatibility
const (
	SMB2Dialect0202 = Dialect0202
	SMB2Dialect0210 = Dialect0210
	SMB2Dialect0300 = Dialect0300
	SMB2Dialect0302 = Dialect0302
	SMB2Dialect0311 = Dialect0311
	SMB2DialectWild = DialectWildcard
)

// =============================================================================
// Server Capabilities
// =============================================================================

// Capabilities represents SMB2 server capabilities.
// [MS-SMB2] Section 2.2.3
type Capabilities uint32

const (
	// CapDFS indicates DFS (Distributed File System) support.
	CapDFS Capabilities = 0x00000001

	// CapLeasing indicates file leasing support (SMB 2.1+).
	CapLeasing Capabilities = 0x00000002

	// CapLargeMTU indicates large MTU support (SMB 2.1+).
	// Allows read/write operations larger than 64KB.
	CapLargeMTU Capabilities = 0x00000004

	// CapMultiChannel indicates multichannel support (SMB 3.0+).
	CapMultiChannel Capabilities = 0x00000008

	// CapPersistentHandles indicates persistent handle support (SMB 3.0+).
	CapPersistentHandles Capabilities = 0x00000010

	// CapDirectoryLeasing indicates directory leasing support (SMB 3.0+).
	CapDirectoryLeasing Capabilities = 0x00000020

	// CapEncryption indicates encryption support (SMB 3.0+).
	CapEncryption Capabilities = 0x00000040
)

// Has returns true if the capabilities contain the specified capability.
func (c Capabilities) Has(cap Capabilities) bool {
	return c&cap != 0
}

// Legacy aliases for backward compatibility
const (
	SMB2CapDFS               = uint32(CapDFS)
	SMB2CapLeasing           = uint32(CapLeasing)
	SMB2CapLargeMTU          = uint32(CapLargeMTU)
	SMB2CapMultiChannel      = uint32(CapMultiChannel)
	SMB2CapPersistentHandles = uint32(CapPersistentHandles)
	SMB2CapDirectoryLeasing  = uint32(CapDirectoryLeasing)
	SMB2CapEncryption        = uint32(CapEncryption)
)

// =============================================================================
// Session Flags
// =============================================================================

// SessionFlags represents SMB2 session flags.
// [MS-SMB2] Section 2.2.6
type SessionFlags uint16

const (
	// SessionFlagIsGuest indicates the session is a guest session.
	SessionFlagIsGuest SessionFlags = 0x0001

	// SessionFlagIsNull indicates the session is a null/anonymous session.
	SessionFlagIsNull SessionFlags = 0x0002

	// SessionFlagEncryptData indicates the session requires encryption (SMB 3.x).
	SessionFlagEncryptData SessionFlags = 0x0004
)

// Legacy aliases for backward compatibility
const (
	SMB2SessionFlagIsGuest     = uint16(SessionFlagIsGuest)
	SMB2SessionFlagIsNull      = uint16(SessionFlagIsNull)
	SMB2SessionFlagEncryptData = uint16(SessionFlagEncryptData)
)

// =============================================================================
// Share Types
// =============================================================================

// ShareType represents the type of shared resource.
// [MS-SMB2] Section 2.2.10
type ShareType uint8

const (
	// ShareTypeDisk indicates a disk share (file share).
	ShareTypeDisk ShareType = 0x01

	// ShareTypePipe indicates a named pipe share (IPC$).
	ShareTypePipe ShareType = 0x02

	// ShareTypePrint indicates a print share.
	ShareTypePrint ShareType = 0x03
)

// Legacy aliases for backward compatibility
const (
	SMB2ShareTypeDisk  = uint8(ShareTypeDisk)
	SMB2ShareTypePipe  = uint8(ShareTypePipe)
	SMB2ShareTypePrint = uint8(ShareTypePrint)
)

// =============================================================================
// Create Disposition
// =============================================================================

// CreateDisposition specifies the action to take if a file exists or not.
// [MS-SMB2] Section 2.2.13
type CreateDisposition uint32

const (
	// FileSupersede replaces the file if it exists, creates if not.
	FileSupersede CreateDisposition = 0x00000000

	// FileOpen opens the file if it exists, fails if not.
	FileOpen CreateDisposition = 0x00000001

	// FileCreate creates the file if it doesn't exist, fails if it does.
	FileCreate CreateDisposition = 0x00000002

	// FileOpenIf opens if exists, creates if not.
	FileOpenIf CreateDisposition = 0x00000003

	// FileOverwrite overwrites if exists, fails if not.
	FileOverwrite CreateDisposition = 0x00000004

	// FileOverwriteIf overwrites if exists, creates if not.
	FileOverwriteIf CreateDisposition = 0x00000005
)

// =============================================================================
// Create Action (Response)
// =============================================================================

// CreateAction indicates the action taken by the server.
// [MS-SMB2] Section 2.2.14
type CreateAction uint32

const (
	// FileSuperseded indicates the file was replaced.
	FileSuperseded CreateAction = 0x00000000

	// FileOpened indicates the file was opened.
	FileOpened CreateAction = 0x00000001

	// FileCreated indicates the file was created.
	FileCreated CreateAction = 0x00000002

	// FileOverwritten indicates the file was overwritten.
	FileOverwritten CreateAction = 0x00000003
)

// =============================================================================
// File Attributes
// =============================================================================

// FileAttributes represents Windows file attributes.
// [MS-FSCC] Section 2.6
type FileAttributes uint32

const (
	FileAttributeReadonly          FileAttributes = 0x00000001
	FileAttributeHidden            FileAttributes = 0x00000002
	FileAttributeSystem            FileAttributes = 0x00000004
	FileAttributeDirectory         FileAttributes = 0x00000010
	FileAttributeArchive           FileAttributes = 0x00000020
	FileAttributeNormal            FileAttributes = 0x00000080
	FileAttributeTemporary         FileAttributes = 0x00000100
	FileAttributeSparseFile        FileAttributes = 0x00000200
	FileAttributeReparsePoint      FileAttributes = 0x00000400
	FileAttributeCompressed        FileAttributes = 0x00000800
	FileAttributeNotContentIndexed FileAttributes = 0x00002000
	FileAttributeEncrypted         FileAttributes = 0x00004000
)

// IsDirectory returns true if the attributes indicate a directory.
func (a FileAttributes) IsDirectory() bool {
	return a&FileAttributeDirectory != 0
}

// =============================================================================
// File Information Classes
// =============================================================================

// FileInfoClass specifies the type of file information.
// [MS-FSCC] Section 2.4
type FileInfoClass uint8

const (
	FileDirectoryInformation       FileInfoClass = 1
	FileFullDirectoryInformation   FileInfoClass = 2
	FileBothDirectoryInformation   FileInfoClass = 3
	FileBasicInformation           FileInfoClass = 4
	FileStandardInformation        FileInfoClass = 5
	FileInternalInformation        FileInfoClass = 6
	FileEaInformation              FileInfoClass = 7
	FileAccessInformation          FileInfoClass = 8
	FileNameInformation            FileInfoClass = 9
	FileRenameInformation          FileInfoClass = 10
	FileNamesInformation           FileInfoClass = 12
	FileDispositionInformation     FileInfoClass = 13
	FileAllInformation             FileInfoClass = 18
	FileEndOfFileInformation       FileInfoClass = 20
	FileStreamInformation          FileInfoClass = 22
	FileNetworkOpenInformation     FileInfoClass = 34
	FileIdBothDirectoryInformation FileInfoClass = 37
	FileIdFullDirectoryInformation FileInfoClass = 38
	FileDispositionInformationEx   FileInfoClass = 64
)

// =============================================================================
// Info Type for QUERY_INFO
// =============================================================================

// InfoType specifies the type of information to query.
// [MS-SMB2] Section 2.2.37
type InfoType uint8

const (
	// InfoTypeFile queries file information.
	InfoTypeFile InfoType = 0x01

	// InfoTypeFilesystem queries filesystem information.
	InfoTypeFilesystem InfoType = 0x02

	// InfoTypeSecurity queries security information (ACLs).
	InfoTypeSecurity InfoType = 0x03

	// InfoTypeQuota queries quota information.
	InfoTypeQuota InfoType = 0x04
)

// Legacy aliases for backward compatibility
const (
	SMB2InfoTypeFile       = uint8(InfoTypeFile)
	SMB2InfoTypeFilesystem = uint8(InfoTypeFilesystem)
	SMB2InfoTypeSecurity   = uint8(InfoTypeSecurity)
	SMB2InfoTypeQuota      = uint8(InfoTypeQuota)
)

// =============================================================================
// Access Mask
// =============================================================================

// AccessMask specifies the type of access requested.
// [MS-SMB2] Section 2.2.13.1
type AccessMask uint32

const (
	FileReadData         AccessMask = 0x00000001
	FileWriteData        AccessMask = 0x00000002
	FileAppendData       AccessMask = 0x00000004
	FileReadEA           AccessMask = 0x00000008
	FileWriteEA          AccessMask = 0x00000010
	FileExecute          AccessMask = 0x00000020
	FileDeleteChild      AccessMask = 0x00000040
	FileReadAttributes   AccessMask = 0x00000080
	FileWriteAttributes  AccessMask = 0x00000100
	Delete               AccessMask = 0x00010000
	ReadControl          AccessMask = 0x00020000
	WriteDac             AccessMask = 0x00040000
	WriteOwner           AccessMask = 0x00080000
	Synchronize          AccessMask = 0x00100000
	AccessSystemSecurity AccessMask = 0x01000000
	MaximumAllowed       AccessMask = 0x02000000
	GenericAll           AccessMask = 0x10000000
	GenericExecute       AccessMask = 0x20000000
	GenericWrite         AccessMask = 0x40000000
	GenericRead          AccessMask = 0x80000000
)

// =============================================================================
// Share Access
// =============================================================================

// ShareAccess specifies how the file can be shared.
// [MS-SMB2] Section 2.2.13
type ShareAccess uint32

const (
	FileShareRead   ShareAccess = 0x00000001
	FileShareWrite  ShareAccess = 0x00000002
	FileShareDelete ShareAccess = 0x00000004
)

// =============================================================================
// Create Options
// =============================================================================

// CreateOptions specifies options for file creation.
// [MS-SMB2] Section 2.2.13
type CreateOptions uint32

const (
	FileDirectoryFile           CreateOptions = 0x00000001
	FileWriteThrough            CreateOptions = 0x00000002
	FileSequentialOnly          CreateOptions = 0x00000004
	FileNoIntermediateBuffering CreateOptions = 0x00000008
	FileSynchronousIoAlert      CreateOptions = 0x00000010
	FileSynchronousIoNonalert   CreateOptions = 0x00000020
	FileNonDirectoryFile        CreateOptions = 0x00000040
	FileCompleteIfOplocked      CreateOptions = 0x00000100
	FileNoEaKnowledge           CreateOptions = 0x00000200
	FileRandomAccess            CreateOptions = 0x00000800
	FileDeleteOnClose           CreateOptions = 0x00001000
	FileOpenByFileId            CreateOptions = 0x00002000
	FileOpenForBackupIntent     CreateOptions = 0x00004000
	FileNoCompression           CreateOptions = 0x00008000
	FileOpenReparsePoint        CreateOptions = 0x00200000
	FileOpenNoRecall            CreateOptions = 0x00400000
)

// =============================================================================
// Query Directory Flags
// =============================================================================

// QueryDirectoryFlags controls directory enumeration behavior.
// [MS-SMB2] Section 2.2.33
type QueryDirectoryFlags uint8

const (
	SMB2RestartScans      QueryDirectoryFlags = 0x01
	SMB2ReturnSingleEntry QueryDirectoryFlags = 0x02
	SMB2IndexSpecified    QueryDirectoryFlags = 0x04
	SMB2Reopen            QueryDirectoryFlags = 0x10
)

// =============================================================================
// Close Flags
// =============================================================================

// CloseFlags controls close behavior.
// [MS-SMB2] Section 2.2.15
type CloseFlags uint16

const (
	SMB2ClosePostQueryAttrib CloseFlags = 0x0001
)

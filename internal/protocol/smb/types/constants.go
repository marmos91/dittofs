// Package types contains SMB2 protocol constants, types, and error codes.
// Reference: [MS-SMB2] - Server Message Block (SMB) Protocol Versions 2 and 3
package types

// SMB1ProtocolID is the SMB1 protocol identifier (little-endian: 0xFF 'S' 'M' 'B')
const SMB1ProtocolID uint32 = 0x424D53FF

// SMB2ProtocolID is the SMB2 protocol identifier (little-endian: 0xFE 'S' 'M' 'B')
const SMB2ProtocolID uint32 = 0x424D53FE

// SMB2 Command codes [MS-SMB2] 2.2.1
const (
	SMB2Negotiate      uint16 = 0x0000
	SMB2SessionSetup   uint16 = 0x0001
	SMB2Logoff         uint16 = 0x0002
	SMB2TreeConnect    uint16 = 0x0003
	SMB2TreeDisconnect uint16 = 0x0004
	SMB2Create         uint16 = 0x0005
	SMB2Close          uint16 = 0x0006
	SMB2Flush          uint16 = 0x0007
	SMB2Read           uint16 = 0x0008
	SMB2Write          uint16 = 0x0009
	SMB2Lock           uint16 = 0x000A
	SMB2Ioctl          uint16 = 0x000B
	SMB2Cancel         uint16 = 0x000C
	SMB2Echo           uint16 = 0x000D
	SMB2QueryDirectory uint16 = 0x000E
	SMB2ChangeNotify   uint16 = 0x000F
	SMB2QueryInfo      uint16 = 0x0010
	SMB2SetInfo        uint16 = 0x0011
	SMB2OplockBreak    uint16 = 0x0012
)

// SMB2 Header Flags [MS-SMB2] 2.2.1.1
const (
	SMB2FlagsServerToRedir   uint32 = 0x00000001 // Response flag
	SMB2FlagsAsyncCommand    uint32 = 0x00000002
	SMB2FlagsRelatedOps      uint32 = 0x00000004
	SMB2FlagsSigned          uint32 = 0x00000008
	SMB2FlagsPriorityMask    uint32 = 0x00000070
	SMB2FlagsDfsOperations   uint32 = 0x10000000
	SMB2FlagsReplayOperation uint32 = 0x20000000
)

// SMB2 Dialects [MS-SMB2] 2.2.3
const (
	SMB2Dialect0202 uint16 = 0x0202 // SMB 2.0.2
	SMB2Dialect0210 uint16 = 0x0210 // SMB 2.1
	SMB2Dialect0300 uint16 = 0x0300 // SMB 3.0
	SMB2Dialect0302 uint16 = 0x0302 // SMB 3.0.2
	SMB2Dialect0311 uint16 = 0x0311 // SMB 3.1.1
	SMB2DialectWild uint16 = 0x02FF // Wildcard
)

// SMB2 Capabilities [MS-SMB2] 2.2.3
const (
	SMB2CapDFS               uint32 = 0x00000001
	SMB2CapLeasing           uint32 = 0x00000002
	SMB2CapLargeMTU          uint32 = 0x00000004
	SMB2CapMultiChannel      uint32 = 0x00000008
	SMB2CapPersistentHandles uint32 = 0x00000010
	SMB2CapDirectoryLeasing  uint32 = 0x00000020
	SMB2CapEncryption        uint32 = 0x00000040
)

// Session Flags [MS-SMB2] 2.2.6
const (
	SMB2SessionFlagIsGuest     uint16 = 0x0001
	SMB2SessionFlagIsNull      uint16 = 0x0002
	SMB2SessionFlagEncryptData uint16 = 0x0004
)

// Share Types [MS-SMB2] 2.2.10
const (
	SMB2ShareTypeDisk  uint8 = 0x01
	SMB2ShareTypePipe  uint8 = 0x02
	SMB2ShareTypePrint uint8 = 0x03
)

// Create Disposition [MS-SMB2] 2.2.13
const (
	FileSupersede   uint32 = 0x00000000 // Replace if exists
	FileOpen        uint32 = 0x00000001 // Open existing
	FileCreate      uint32 = 0x00000002 // Create new (fail if exists)
	FileOpenIf      uint32 = 0x00000003 // Open or create
	FileOverwrite   uint32 = 0x00000004 // Overwrite existing
	FileOverwriteIf uint32 = 0x00000005 // Overwrite or create
)

// Create Action (response) [MS-SMB2] 2.2.14
const (
	FileSuperseded  uint32 = 0x00000000
	FileOpened      uint32 = 0x00000001
	FileCreated     uint32 = 0x00000002
	FileOverwritten uint32 = 0x00000003
)

// File Attributes [MS-FSCC] 2.6
const (
	FileAttributeReadonly          uint32 = 0x00000001
	FileAttributeHidden            uint32 = 0x00000002
	FileAttributeSystem            uint32 = 0x00000004
	FileAttributeDirectory         uint32 = 0x00000010
	FileAttributeArchive           uint32 = 0x00000020
	FileAttributeNormal            uint32 = 0x00000080
	FileAttributeTemporary         uint32 = 0x00000100
	FileAttributeSparseFile        uint32 = 0x00000200
	FileAttributeReparsePoint      uint32 = 0x00000400
	FileAttributeCompressed        uint32 = 0x00000800
	FileAttributeNotContentIndexed uint32 = 0x00002000
	FileAttributeEncrypted         uint32 = 0x00004000
)

// File Information Classes [MS-FSCC] 2.4
const (
	FileDirectoryInformation       uint8 = 1
	FileFullDirectoryInformation   uint8 = 2
	FileBothDirectoryInformation   uint8 = 3
	FileBasicInformation           uint8 = 4
	FileStandardInformation        uint8 = 5
	FileInternalInformation        uint8 = 6
	FileEaInformation              uint8 = 7
	FileAccessInformation          uint8 = 8
	FileNameInformation            uint8 = 9
	FileRenameInformation          uint8 = 10
	FileNamesInformation           uint8 = 12
	FileAllInformation             uint8 = 18
	FileNetworkOpenInformation     uint8 = 34
	FileIdBothDirectoryInformation uint8 = 37
	FileIdFullDirectoryInformation uint8 = 38
)

// InfoType for QUERY_INFO [MS-SMB2] 2.2.37
const (
	SMB2InfoTypeFile       uint8 = 0x01
	SMB2InfoTypeFilesystem uint8 = 0x02
	SMB2InfoTypeSecurity   uint8 = 0x03
	SMB2InfoTypeQuota      uint8 = 0x04
)

// Access Mask constants [MS-SMB2] 2.2.13.1
const (
	FileReadData         uint32 = 0x00000001
	FileWriteData        uint32 = 0x00000002
	FileAppendData       uint32 = 0x00000004
	FileReadEA           uint32 = 0x00000008
	FileWriteEA          uint32 = 0x00000010
	FileExecute          uint32 = 0x00000020
	FileDeleteChild      uint32 = 0x00000040
	FileReadAttributes   uint32 = 0x00000080
	FileWriteAttributes  uint32 = 0x00000100
	Delete               uint32 = 0x00010000
	ReadControl          uint32 = 0x00020000
	WriteDac             uint32 = 0x00040000
	WriteOwner           uint32 = 0x00080000
	Synchronize          uint32 = 0x00100000
	AccessSystemSecurity uint32 = 0x01000000
	MaximumAllowed       uint32 = 0x02000000
	GenericAll           uint32 = 0x10000000
	GenericExecute       uint32 = 0x20000000
	GenericWrite         uint32 = 0x40000000
	GenericRead          uint32 = 0x80000000
)

// Share Access constants [MS-SMB2] 2.2.13
const (
	FileShareRead   uint32 = 0x00000001
	FileShareWrite  uint32 = 0x00000002
	FileShareDelete uint32 = 0x00000004
)

// Create Options constants [MS-SMB2] 2.2.13
const (
	FileDirectoryFile           uint32 = 0x00000001
	FileWriteThrough            uint32 = 0x00000002
	FileSequentialOnly          uint32 = 0x00000004
	FileNoIntermediateBuffering uint32 = 0x00000008
	FileSynchronousIoAlert      uint32 = 0x00000010
	FileSynchronousIoNonalert   uint32 = 0x00000020
	FileNonDirectoryFile        uint32 = 0x00000040
	FileCompleteIfOplocked      uint32 = 0x00000100
	FileNoEaKnowledge           uint32 = 0x00000200
	FileRandomAccess            uint32 = 0x00000800
	FileDeleteOnClose           uint32 = 0x00001000
	FileOpenByFileId            uint32 = 0x00002000
	FileOpenForBackupIntent     uint32 = 0x00004000
	FileNoCompression           uint32 = 0x00008000
	FileOpenReparsePoint        uint32 = 0x00200000
	FileOpenNoRecall            uint32 = 0x00400000
)

// QueryDirectory Flags [MS-SMB2] 2.2.33
const (
	SMB2RestartScans      uint8 = 0x01
	SMB2ReturnSingleEntry uint8 = 0x02
	SMB2IndexSpecified    uint8 = 0x04
	SMB2Reopen            uint8 = 0x10
)

// Close Flags [MS-SMB2] 2.2.15
const (
	SMB2ClosePostQueryAttrib uint16 = 0x0001
)

// CommandName returns the string name of the command
func CommandName(cmd uint16) string {
	switch cmd {
	case SMB2Negotiate:
		return "NEGOTIATE"
	case SMB2SessionSetup:
		return "SESSION_SETUP"
	case SMB2Logoff:
		return "LOGOFF"
	case SMB2TreeConnect:
		return "TREE_CONNECT"
	case SMB2TreeDisconnect:
		return "TREE_DISCONNECT"
	case SMB2Create:
		return "CREATE"
	case SMB2Close:
		return "CLOSE"
	case SMB2Flush:
		return "FLUSH"
	case SMB2Read:
		return "READ"
	case SMB2Write:
		return "WRITE"
	case SMB2Lock:
		return "LOCK"
	case SMB2Ioctl:
		return "IOCTL"
	case SMB2Cancel:
		return "CANCEL"
	case SMB2Echo:
		return "ECHO"
	case SMB2QueryDirectory:
		return "QUERY_DIRECTORY"
	case SMB2ChangeNotify:
		return "CHANGE_NOTIFY"
	case SMB2QueryInfo:
		return "QUERY_INFO"
	case SMB2SetInfo:
		return "SET_INFO"
	case SMB2OplockBreak:
		return "OPLOCK_BREAK"
	default:
		return "UNKNOWN"
	}
}

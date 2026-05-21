package acl

// Generic access-mask bits (MS-DTYP §2.4.3 ACCESS_MASK). These must be
// expanded into file-object-specific rights before the mask is stored in an
// ACE or evaluated against a security descriptor (MS-DTYP §2.5.3,
// MS-FSA §2.1.5.1.2.1).
const (
	genericRead    uint32 = 0x80000000
	genericWrite   uint32 = 0x40000000
	genericExecute uint32 = 0x20000000
	genericAll     uint32 = 0x10000000
)

// File-object specific rights and standard rights used by the expansion
// table. Values match the NFSv4 ACE4_* and Windows file-object masks (which
// share bit positions for the rights covered here).
const (
	fileReadData        uint32 = 0x00000001
	fileWriteData       uint32 = 0x00000002
	fileAppendData      uint32 = 0x00000004
	fileReadEA          uint32 = 0x00000008
	fileWriteEA         uint32 = 0x00000010
	fileExecute         uint32 = 0x00000020
	fileReadAttributes  uint32 = 0x00000080
	fileWriteAttributes uint32 = 0x00000100

	// Standard rights (MS-DTYP §2.4.3). For file objects:
	//   STANDARD_RIGHTS_READ    = READ_CONTROL
	//   STANDARD_RIGHTS_WRITE   = READ_CONTROL
	//   STANDARD_RIGHTS_EXECUTE = READ_CONTROL
	standardRightsRead    uint32 = 0x00020000 // READ_CONTROL
	standardRightsWrite   uint32 = 0x00020000 // READ_CONTROL
	standardRightsExecute uint32 = 0x00020000 // READ_CONTROL

	synchronize uint32 = 0x00100000

	// FILE_ALL_ACCESS = STANDARD_RIGHTS_REQUIRED (0x000F0000) |
	//                   SYNCHRONIZE (0x00100000) |
	//                   all file-specific rights (0x000001FF)
	fileAllAccess uint32 = 0x001F01FF
)

// Expanded file-object masks per MS-DTYP §2.4.3 GenericMapping (the table
// used by NTFS / SMB for file objects; matches the smbtorture
// smb2.acls.GENERIC expectation and Samba's mapping).
const (
	genericReadFile = fileReadData |
		fileReadAttributes |
		fileReadEA |
		standardRightsRead |
		synchronize

	genericWriteFile = fileWriteData |
		fileWriteAttributes |
		fileWriteEA |
		fileAppendData |
		standardRightsWrite |
		synchronize

	genericExecuteFile = fileReadAttributes |
		fileExecute |
		standardRightsExecute |
		synchronize

	genericAllFile = fileAllAccess
)

// ExpandGenericMask expands MS-DTYP GENERIC_* access bits to their
// file-object-specific equivalents per MS-DTYP §2.4.3 GenericMapping.
//
// Mapping (file objects):
//
//	GENERIC_READ (0x80000000)    -> FILE_READ_DATA | FILE_READ_ATTRIBUTES |
//	                                FILE_READ_EA | STANDARD_RIGHTS_READ |
//	                                SYNCHRONIZE                              (0x00120089)
//	GENERIC_WRITE (0x40000000)   -> FILE_WRITE_DATA | FILE_WRITE_ATTRIBUTES |
//	                                FILE_WRITE_EA | FILE_APPEND_DATA |
//	                                STANDARD_RIGHTS_WRITE | SYNCHRONIZE      (0x00120116)
//	GENERIC_EXECUTE (0x20000000) -> FILE_READ_ATTRIBUTES | FILE_EXECUTE |
//	                                STANDARD_RIGHTS_EXECUTE | SYNCHRONIZE    (0x001200A0)
//	GENERIC_ALL (0x10000000)     -> FILE_ALL_ACCESS                          (0x001F01FF)
//
// Generic bits are stripped from the returned mask after expansion: a
// post-mapping ACCESS_MASK must contain only specific rights (MS-DTYP
// §2.5.3). Bits unrelated to the generic set pass through unchanged.
//
// Per MS-DTYP §2.5.3 / MS-FSA §2.1.5.1.2.1, this expansion MUST be applied:
//   - at SET_INFO Security on each ACE AccessMask before persisting;
//   - on SMB CREATE DesiredAccess before access-check evaluation;
//   - on the probe set and result of any MaximalAccess computation.
func ExpandGenericMask(mask uint32) uint32 {
	expanded := mask

	if expanded&genericRead != 0 {
		expanded |= genericReadFile
	}
	if expanded&genericWrite != 0 {
		expanded |= genericWriteFile
	}
	if expanded&genericExecute != 0 {
		expanded |= genericExecuteFile
	}
	if expanded&genericAll != 0 {
		expanded |= genericAllFile
	}

	// Strip the generic bits — only specific rights remain (MS-DTYP §2.5.3).
	expanded &^= genericRead | genericWrite | genericExecute | genericAll

	return expanded
}

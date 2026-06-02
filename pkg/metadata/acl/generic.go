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

// Expanded file-object masks per MS-DTYP §2.4.3 GenericMapping (the table
// used by NTFS / SMB for file objects; matches the smbtorture
// smb2.acls.GENERIC expectation and Samba's mapping).
//
// File-object specific rights and standard rights share bit positions with
// the NFSv4 ACE4_* constants from types.go (MS-DTYP and NFSv4 deliberately
// align here), so the mapping is expressed directly in ACE4_* terms.
//
// For file objects the three standard-rights aliases collapse to READ_CONTROL
// (= ACE4_READ_ACL, 0x00020000), which is why GENERIC_READ/WRITE/EXECUTE all
// contribute the same READ_CONTROL bit.
const (
	// GENERIC_READ -> FILE_READ_DATA | FILE_READ_EA | FILE_READ_ATTRIBUTES |
	//                 READ_CONTROL  | SYNCHRONIZE                 (0x00120089)
	genericReadFile = ACE4_READ_DATA |
		ACE4_READ_NAMED_ATTRS |
		ACE4_READ_ATTRIBUTES |
		ACE4_READ_ACL | // STANDARD_RIGHTS_READ == READ_CONTROL
		ACE4_SYNCHRONIZE

	// GENERIC_WRITE -> FILE_WRITE_DATA | FILE_APPEND_DATA | FILE_WRITE_EA |
	//                  FILE_WRITE_ATTRIBUTES | READ_CONTROL | SYNCHRONIZE
	//                                                                (0x00120116)
	genericWriteFile = ACE4_WRITE_DATA |
		ACE4_APPEND_DATA |
		ACE4_WRITE_NAMED_ATTRS |
		ACE4_WRITE_ATTRIBUTES |
		ACE4_READ_ACL | // STANDARD_RIGHTS_WRITE == READ_CONTROL
		ACE4_SYNCHRONIZE

	// GENERIC_EXECUTE -> FILE_READ_ATTRIBUTES | FILE_EXECUTE |
	//                    READ_CONTROL | SYNCHRONIZE              (0x001200A0)
	genericExecuteFile = ACE4_READ_ATTRIBUTES |
		ACE4_EXECUTE |
		ACE4_READ_ACL | // STANDARD_RIGHTS_EXECUTE == READ_CONTROL
		ACE4_SYNCHRONIZE

	// GENERIC_ALL -> FILE_ALL_ACCESS = STANDARD_RIGHTS_REQUIRED (0x000F0000) |
	//                SYNCHRONIZE (0x00100000) |
	//                all file-specific rights (0x000001FF)         (0x001F01FF)
	//
	// STANDARD_RIGHTS_REQUIRED = DELETE | READ_CONTROL | WRITE_DAC | WRITE_OWNER.
	// File-specific bits 0x000001FF cover READ_DATA, WRITE_DATA, APPEND_DATA,
	// READ_NAMED_ATTRS, WRITE_NAMED_ATTRS, EXECUTE, DELETE_CHILD,
	// READ_ATTRIBUTES, WRITE_ATTRIBUTES.
	genericAllFile = ACE4_DELETE |
		ACE4_READ_ACL |
		ACE4_WRITE_ACL |
		ACE4_WRITE_OWNER |
		ACE4_SYNCHRONIZE |
		ACE4_READ_DATA |
		ACE4_WRITE_DATA |
		ACE4_APPEND_DATA |
		ACE4_READ_NAMED_ATTRS |
		ACE4_WRITE_NAMED_ATTRS |
		ACE4_EXECUTE |
		ACE4_DELETE_CHILD |
		ACE4_READ_ATTRIBUTES |
		ACE4_WRITE_ATTRIBUTES
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

// GenericDerivedBits returns the file-specific rights introduced by the
// best-effort GENERIC_* bits in mask — GENERIC_READ and GENERIC_EXECUTE only.
//
// This matters under SEC_FLAG_MAXIMUM_ALLOWED. Samba's max_allowed.c ok_mask is
//
//	SEC_RIGHTS_FILE_READ | SEC_GENERIC_READ | SEC_GENERIC_EXECUTE |
//	SEC_STD_DELETE | SEC_STD_WRITE_DAC
//
// so a MAX open naming GENERIC_READ or GENERIC_EXECUTE succeeds even when the
// DACL does not grant every mapped specific right (e.g. GENERIC_EXECUTE maps to
// FILE_EXECUTE, which a read-only DACL lacks, yet the open is OK —
// smb2.maximum_allowed.maximum_allowed). GENERIC_WRITE and GENERIC_ALL are NOT
// in ok_mask: a MAX open naming them must still be DENIED when their mapped
// specific rights aren't granted, so their expansion is left under the strict
// gate and is NOT reported here.
//
// Directly-named specific rights are likewise never reported — those remain
// strictly enforced (smb2.acls.MXAC-NOT-GRANTED, #564). Callers subtract this
// set from the explicit mask before the MAX strict-enforcement check.
func GenericDerivedBits(mask uint32) uint32 {
	bestEffortGeneric := mask & (genericRead | genericExecute)
	if bestEffortGeneric == 0 {
		return 0
	}
	named := mask &^ (genericRead | genericWrite | genericExecute | genericAll)
	// Only the rights that the best-effort generics introduce beyond what was
	// named directly (and beyond what GENERIC_WRITE/ALL would contribute, which
	// stay enforced).
	return ExpandGenericMask(bestEffortGeneric) &^ named
}

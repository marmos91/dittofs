package acl

// NFSv4FlagsToWindowsFlags translates NFSv4 ACE flags (uint32) to Windows
// ACE flags (uint8). The critical translation is INHERITED_ACE which uses
// bit 0x80 in NFSv4 but bit 0x10 in Windows. A naive truncation
// uint8(flags & 0xFF) would incorrectly map 0x80 to 0x80, corrupting the
// Windows ACE header.
//
// Bit mapping:
//
//	NFSv4 0x01 (FILE_INHERIT_ACE)         -> Windows 0x01 (OI)
//	NFSv4 0x02 (DIRECTORY_INHERIT_ACE)    -> Windows 0x02 (CI)
//	NFSv4 0x04 (NO_PROPAGATE_INHERIT_ACE) -> Windows 0x04 (NP)
//	NFSv4 0x08 (INHERIT_ONLY_ACE)         -> Windows 0x08 (IO)
//	NFSv4 0x80 (INHERITED_ACE)            -> Windows 0x10 (INHERITED)
func NFSv4FlagsToWindowsFlags(nfsFlags uint32) uint8 {
	var winFlags uint8
	if nfsFlags&ACE4_FILE_INHERIT_ACE != 0 {
		winFlags |= 0x01 // OBJECT_INHERIT_ACE (OI / file inherit)
	}
	if nfsFlags&ACE4_DIRECTORY_INHERIT_ACE != 0 {
		winFlags |= 0x02 // CONTAINER_INHERIT_ACE (CI / directory inherit)
	}
	if nfsFlags&ACE4_NO_PROPAGATE_INHERIT_ACE != 0 {
		winFlags |= 0x04 // NO_PROPAGATE_INHERIT_ACE
	}
	if nfsFlags&ACE4_INHERIT_ONLY_ACE != 0 {
		winFlags |= 0x08 // INHERIT_ONLY_ACE
	}
	if nfsFlags&ACE4_INHERITED_ACE != 0 {
		winFlags |= 0x10 // INHERITED_ACE (0x80 -> 0x10)
	}
	return winFlags
}

// WindowsFlagsToNFSv4Flags translates Windows ACE flags (uint8) to NFSv4
// ACE flags (uint32). This is the inverse of NFSv4FlagsToWindowsFlags.
//
// Bit mapping:
//
//	Windows 0x01 (CI)        -> NFSv4 0x01 (FILE_INHERIT_ACE)
//	Windows 0x02 (OI)        -> NFSv4 0x02 (DIRECTORY_INHERIT_ACE)
//	Windows 0x04 (NP)        -> NFSv4 0x04 (NO_PROPAGATE_INHERIT_ACE)
//	Windows 0x08 (IO)        -> NFSv4 0x08 (INHERIT_ONLY_ACE)
//	Windows 0x10 (INHERITED) -> NFSv4 0x80 (INHERITED_ACE)
func WindowsFlagsToNFSv4Flags(winFlags uint8) uint32 {
	var nfsFlags uint32
	if winFlags&0x01 != 0 {
		nfsFlags |= ACE4_FILE_INHERIT_ACE
	}
	if winFlags&0x02 != 0 {
		nfsFlags |= ACE4_DIRECTORY_INHERIT_ACE
	}
	if winFlags&0x04 != 0 {
		nfsFlags |= ACE4_NO_PROPAGATE_INHERIT_ACE
	}
	if winFlags&0x08 != 0 {
		nfsFlags |= ACE4_INHERIT_ONLY_ACE
	}
	if winFlags&0x10 != 0 {
		nfsFlags |= ACE4_INHERITED_ACE // 0x10 -> 0x80
	}
	return nfsFlags
}

// Package handlers provides SMB2 command handlers and session management.
//
// This file implements SMB Security Descriptor encoding and decoding for
// ACL interoperability between NFSv4 ACEs and Windows DACL ACEs.
//
// The encoding follows MS-DTYP Section 2.4.6 (Security Descriptor),
// Section 2.4.2 (SID), Section 2.4.5 (ACL header), and Section 2.4.4.2
// (ACE format). All binary formats use self-relative layout.
//
// NFSv4 ACL mask bits are intentionally identical to Windows ACCESS_MASK
// bit positions (by design per RFC 7530), so no mask translation is needed.
// Only the principal format differs: NFSv4 uses "user@domain" strings
// while SMB uses binary SIDs.
//
// SID types, encoding, and identity mapping are provided by the shared
// pkg/auth/sid/ package (usable by both SMB and NFS adapters).
package handlers

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/marmos91/dittofs/pkg/auth/sid"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
)

// Security information flags for AdditionalInfo field in QUERY_INFO/SET_INFO.
const (
	OwnerSecurityInformation = 0x00000001
	GroupSecurityInformation = 0x00000002
	DACLSecurityInformation  = 0x00000004
	SACLSecurityInformation  = 0x00000008
)

// Security Descriptor control flags per MS-DTYP Section 2.4.6.
const (
	seSelfRelative      = 0x8000
	seDACLPresent       = 0x0004
	seSACLPresent       = 0x0010
	seDACLAutoInherited = 0x0400
	seDACLProtected     = 0x1000
)

// ACE type constants for Windows ACEs per MS-DTYP Section 2.4.4.1.
const (
	accessAllowedACEType = 0x00
	accessDeniedACEType  = 0x01
	systemAuditACEType   = 0x02
)

// nfsToWindowsACEType maps an NFSv4 ACE type to a Windows ACE type.
// Returns false for unrecognized types (e.g., ALARM).
func nfsToWindowsACEType(nfsType uint32) (uint8, bool) {
	switch nfsType {
	case acl.ACE4_ACCESS_ALLOWED_ACE_TYPE:
		return accessAllowedACEType, true
	case acl.ACE4_ACCESS_DENIED_ACE_TYPE:
		return accessDeniedACEType, true
	case acl.ACE4_SYSTEM_AUDIT_ACE_TYPE:
		return systemAuditACEType, true
	default:
		return 0, false
	}
}

// windowsToNFSACEType maps a Windows ACE type to an NFSv4 ACE type.
// Returns false for unrecognized types.
func windowsToNFSACEType(winType uint8) (uint32, bool) {
	switch winType {
	case accessAllowedACEType:
		return acl.ACE4_ACCESS_ALLOWED_ACE_TYPE, true
	case accessDeniedACEType:
		return acl.ACE4_ACCESS_DENIED_ACE_TYPE, true
	case systemAuditACEType:
		return acl.ACE4_SYSTEM_AUDIT_ACE_TYPE, true
	default:
		return 0, false
	}
}

// sdHeaderSize is the fixed size of a self-relative Security Descriptor header.
const sdHeaderSize = 20

// aclHeaderSize is the fixed size of an ACL header per MS-DTYP Section 2.4.5.
const aclHeaderSize = 8

// aceHeaderSize is the fixed part of an ACE (type + flags + size + mask).
const aceHeaderSize = 8

// ============================================================================
// SID Mapper (delegates to pkg/auth/sid/)
// ============================================================================

// defaultSIDMapper is the package-level SIDMapper used for principal-to-SID
// mapping in security descriptor operations. It is initialized by
// SetSIDMapper during server startup (before any connections are accepted).
//
// A fallback mapper with zeroed sub-authorities is used if SetSIDMapper
// is not called (e.g., in unit tests). This maintains backward compatibility
// with the old behavior of S-1-5-21-0-0-0-{RID} SIDs.
var defaultSIDMapper = sid.NewSIDMapper(0, 0, 0)

// SetSIDMapper sets the package-level SIDMapper used for all security
// descriptor operations. Must be called before any SMB connections are
// accepted to avoid race conditions.
func SetSIDMapper(m *sid.SIDMapper) {
	if m != nil {
		defaultSIDMapper = m
	}
}

// GetSIDMapper returns the current package-level SIDMapper.
// Useful for tests that need to inspect the mapper state.
func GetSIDMapper() *sid.SIDMapper {
	return defaultSIDMapper
}

// ============================================================================
// Security Descriptor Building
// ============================================================================

// BuildSecurityDescriptor constructs a self-relative Security Descriptor
// from file metadata per MS-DTYP Section 2.4.6.
//
// The SD contains:
//   - Owner SID from file UID (if OwnerSecurityInformation requested)
//   - Group SID from file GID (if GroupSecurityInformation requested)
//   - DACL translated from file ACL (if DACLSecurityInformation requested)
//   - SACL empty stub (if SACLSecurityInformation requested)
//
// If additionalSecInfo is 0, all sections (owner, group, DACL) are included.
//
// The binary field ordering follows Windows convention: SACL, DACL, Owner, Group.
// This matches byte-level comparison expectations in smbtorture and Windows.
//
// Parameters:
//   - file: File metadata containing UID, GID, and ACL
//   - additionalSecInfo: Bitmask controlling which sections are included
//
// Returns the binary Security Descriptor or an error.
func BuildSecurityDescriptor(file *metadata.File, additionalSecInfo uint32) ([]byte, error) {
	// Default: include everything if no specific flags
	if additionalSecInfo == 0 {
		additionalSecInfo = OwnerSecurityInformation | GroupSecurityInformation | DACLSecurityInformation
	}

	includeOwner := (additionalSecInfo & OwnerSecurityInformation) != 0
	includeGroup := (additionalSecInfo & GroupSecurityInformation) != 0
	includeDACL := (additionalSecInfo & DACLSecurityInformation) != 0
	includeSACL := (additionalSecInfo & SACLSecurityInformation) != 0

	// Build SIDs using the shared mapper
	ownerSID := defaultSIDMapper.UserSID(file.UID)
	groupSID := defaultSIDMapper.GroupSID(file.GID)

	// Build DACL
	var daclBuf bytes.Buffer
	var fileACL *acl.ACL
	if includeDACL {
		fileACL = buildDACL(&daclBuf, file)
	}

	// Build SACL (empty stub if requested)
	var saclBuf bytes.Buffer
	if includeSACL {
		buildEmptySACL(&saclBuf)
	}

	// Compute SD control flags dynamically
	control := uint16(seSelfRelative)
	if includeDACL {
		control |= seDACLPresent
	}
	if includeSACL {
		control |= seSACLPresent
	}

	// Check for auto-inherited flag: if any ACE has INHERITED_ACE
	if fileACL != nil {
		for _, ace := range fileACL.ACEs {
			if ace.Flag&acl.ACE4_INHERITED_ACE != 0 {
				control |= seDACLAutoInherited
				break
			}
		}
		if fileACL.Protected {
			control |= seDACLProtected
		}
	}

	// Compute offsets following Windows convention: SACL, DACL, Owner, Group
	var saclOffset, daclOffset, ownerOffset, groupOffset uint32
	currentOffset := uint32(sdHeaderSize)

	if includeSACL {
		saclOffset = currentOffset
		currentOffset += uint32(saclBuf.Len())
	}

	if includeDACL {
		daclOffset = currentOffset
		currentOffset += uint32(daclBuf.Len())
	}

	if includeOwner {
		ownerOffset = currentOffset
		currentOffset += uint32(sid.SIDSize(ownerSID))
		currentOffset = alignTo4(currentOffset)
	}

	if includeGroup {
		groupOffset = currentOffset
		_ = currentOffset // suppress unused warning
	}

	// Build the complete Security Descriptor
	var buf bytes.Buffer

	// Header (20 bytes)
	buf.WriteByte(1) // Revision
	buf.WriteByte(0) // Sbz1
	_ = binary.Write(&buf, binary.LittleEndian, control)
	_ = binary.Write(&buf, binary.LittleEndian, ownerOffset)
	_ = binary.Write(&buf, binary.LittleEndian, groupOffset)
	_ = binary.Write(&buf, binary.LittleEndian, saclOffset)
	_ = binary.Write(&buf, binary.LittleEndian, daclOffset)

	// Body in Windows convention order: SACL, DACL, Owner, Group

	// SACL
	if includeSACL {
		buf.Write(saclBuf.Bytes())
	}

	// DACL
	if includeDACL {
		buf.Write(daclBuf.Bytes())
	}

	// Owner SID
	if includeOwner {
		sid.EncodeSID(&buf, ownerSID)
		padTo4(&buf)
	}

	// Group SID
	if includeGroup {
		sid.EncodeSID(&buf, groupSID)
		padTo4(&buf)
	}

	return buf.Bytes(), nil
}

// windowsACE is an internal representation of a Windows ACE for encoding.
type windowsACE struct {
	aceType    uint8
	aceFlags   uint8
	accessMask uint32
	sid        *sid.SID
}

// principalToSID maps an NFSv4 ACE principal identifier to a binary SID.
// Handles special identifiers (OWNER@, GROUP@, EVERYONE@, SYSTEM@,
// ADMINISTRATORS@) and falls back to SIDMapper.PrincipalToSID for others.
func principalToSID(who string, fileUID, fileGID uint32) *sid.SID {
	switch who {
	case acl.SpecialOwner:
		return defaultSIDMapper.UserSID(fileUID)
	case acl.SpecialGroup:
		return defaultSIDMapper.GroupSID(fileGID)
	case acl.SpecialEveryone:
		return sid.WellKnownEveryone
	case acl.SpecialSystem:
		return sid.WellKnownSystem
	case acl.SpecialAdministrators:
		return sid.WellKnownAdministrators
	default:
		return defaultSIDMapper.PrincipalToSID(who, fileUID, fileGID)
	}
}

// buildDACL constructs a DACL (Discretionary Access Control List) from the file's ACL.
// If the file has no ACL, a DACL is synthesized from POSIX mode bits using
// acl.SynthesizeFromMode. Returns the source ACL used for SD control flag computation.
func buildDACL(buf *bytes.Buffer, file *metadata.File) *acl.ACL {
	var aces []windowsACE
	var fileACL *acl.ACL

	if file.ACL != nil && len(file.ACL.ACEs) > 0 {
		fileACL = file.ACL
	} else {
		// No ACL: synthesize DACL from POSIX mode bits
		isDir := file.Type == metadata.FileTypeDirectory
		fileACL = acl.SynthesizeFromMode(file.Mode, file.UID, file.GID, isDir)
	}

	aces = make([]windowsACE, 0, len(fileACL.ACEs))
	for _, ace := range fileACL.ACEs {
		aceType, ok := nfsToWindowsACEType(ace.Type)
		if !ok {
			continue
		}

		aces = append(aces, windowsACE{
			aceType:    aceType,
			aceFlags:   acl.NFSv4FlagsToWindowsFlags(ace.Flag),
			accessMask: ace.AccessMask,
			sid:        principalToSID(ace.Who, file.UID, file.GID),
		})
	}

	// Compute total ACL size
	totalACLSize := aclHeaderSize
	for i := range aces {
		totalACLSize += aceHeaderSize + sid.SIDSize(aces[i].sid)
	}

	// Write ACL header (8 bytes) per MS-DTYP Section 2.4.5
	buf.WriteByte(2) // AclRevision (2 = standard)
	buf.WriteByte(0) // Sbz1
	_ = binary.Write(buf, binary.LittleEndian, uint16(totalACLSize))
	_ = binary.Write(buf, binary.LittleEndian, uint16(len(aces)))
	_ = binary.Write(buf, binary.LittleEndian, uint16(0)) // Sbz2

	// Write each ACE per MS-DTYP Section 2.4.4.2
	for i := range aces {
		ace := &aces[i]
		aceSize := uint16(aceHeaderSize + sid.SIDSize(ace.sid))

		buf.WriteByte(ace.aceType)  // AceType
		buf.WriteByte(ace.aceFlags) // AceFlags
		_ = binary.Write(buf, binary.LittleEndian, aceSize)
		_ = binary.Write(buf, binary.LittleEndian, ace.accessMask)
		sid.EncodeSID(buf, ace.sid)
	}

	return fileACL
}

// buildEmptySACL writes a valid empty SACL to buf.
// The SACL has revision=2, count=0, and total size=8 bytes.
func buildEmptySACL(buf *bytes.Buffer) {
	buf.WriteByte(2) // AclRevision
	buf.WriteByte(0) // Sbz1
	_ = binary.Write(buf, binary.LittleEndian, uint16(aclHeaderSize)) // AclSize = 8
	_ = binary.Write(buf, binary.LittleEndian, uint16(0))             // AceCount = 0
	_ = binary.Write(buf, binary.LittleEndian, uint16(0))             // Sbz2
}

// ============================================================================
// Security Descriptor Parsing
// ============================================================================

// ParseSecurityDescriptor parses a self-relative Security Descriptor and extracts
// the owner UID, group GID, and NFSv4 ACL.
//
// Returns pointers to allow callers to detect which sections were present.
// A nil pointer means that section was not present in the SD.
func ParseSecurityDescriptor(data []byte) (ownerUID *uint32, ownerGID *uint32, fileACL *acl.ACL, err error) {
	if len(data) < sdHeaderSize {
		return nil, nil, nil, fmt.Errorf("security descriptor too short: %d bytes", len(data))
	}

	// Parse header
	// revision := data[0]
	// sbz1 := data[1]
	// control := binary.LittleEndian.Uint16(data[2:4])
	offsetOwner := binary.LittleEndian.Uint32(data[4:8])
	offsetGroup := binary.LittleEndian.Uint32(data[8:12])
	// offsetSACL := binary.LittleEndian.Uint32(data[12:16])
	offsetDACL := binary.LittleEndian.Uint32(data[16:20])

	// Parse Owner SID
	if offsetOwner > 0 && int(offsetOwner) < len(data) {
		s, _, err := sid.DecodeSID(data[offsetOwner:])
		if err == nil {
			uid := sidToUID(s)
			ownerUID = &uid
		}
	}

	// Parse Group SID
	if offsetGroup > 0 && int(offsetGroup) < len(data) {
		s, _, err := sid.DecodeSID(data[offsetGroup:])
		if err == nil {
			gid := sidToGID(s)
			ownerGID = &gid
		}
	}

	// Parse DACL
	if offsetDACL > 0 && int(offsetDACL)+aclHeaderSize <= len(data) {
		daclData := data[offsetDACL:]
		fileACL, err = parseDACL(daclData)
		if err != nil {
			return ownerUID, ownerGID, nil, fmt.Errorf("failed to parse DACL: %w", err)
		}
	}

	return ownerUID, ownerGID, fileACL, nil
}

// parseDACL parses a DACL and returns an NFSv4 ACL.
// ACLs parsed from SET_INFO are marked with Source: ACLSourceSMBExplicit.
func parseDACL(data []byte) (*acl.ACL, error) {
	if len(data) < aclHeaderSize {
		return nil, fmt.Errorf("DACL too short: %d bytes", len(data))
	}

	// Parse ACL header
	// aclRevision := data[0]
	// sbz1 := data[1]
	// aclSize := binary.LittleEndian.Uint16(data[2:4])
	aceCount := binary.LittleEndian.Uint16(data[4:6])
	// sbz2 := binary.LittleEndian.Uint16(data[6:8])

	if aceCount > acl.MaxACECount {
		return nil, fmt.Errorf("DACL has %d ACEs, exceeds maximum %d", aceCount, acl.MaxACECount)
	}

	aces := make([]acl.ACE, 0, aceCount)
	offset := aclHeaderSize

	for i := 0; i < int(aceCount); i++ {
		if offset+aceHeaderSize > len(data) {
			return nil, fmt.Errorf("ACE %d extends beyond DACL data", i)
		}

		aceType := data[offset]
		aceFlags := data[offset+1]
		aceSize := binary.LittleEndian.Uint16(data[offset+2 : offset+4])
		accessMask := binary.LittleEndian.Uint32(data[offset+4 : offset+8])

		if offset+int(aceSize) > len(data) {
			return nil, fmt.Errorf("ACE %d size %d extends beyond DACL data", i, aceSize)
		}

		// Parse SID from remaining ACE data
		s, _, err := sid.DecodeSID(data[offset+aceHeaderSize : offset+int(aceSize)])
		if err != nil {
			return nil, fmt.Errorf("failed to decode SID in ACE %d: %w", i, err)
		}

		// Map Windows ACE type to NFSv4 ACE type
		nfsACEType, ok := windowsToNFSACEType(aceType)
		if !ok {
			offset += int(aceSize)
			continue
		}

		// Convert SID to NFSv4 principal
		who := defaultSIDMapper.SIDToPrincipal(s)

		aces = append(aces, acl.ACE{
			Type:       nfsACEType,
			Flag:       acl.WindowsFlagsToNFSv4Flags(aceFlags),
			AccessMask: accessMask,
			Who:        who,
		})

		offset += int(aceSize)
	}

	return &acl.ACL{
		ACEs:   aces,
		Source: acl.ACLSourceSMBExplicit,
	}, nil
}

// sidToUID extracts a UID from a SID using the default mapper.
// For domain user SIDs, returns the mapped UID.
// For non-domain SIDs, returns 65534 (nobody).
func sidToUID(s *sid.SID) uint32 {
	if uid, ok := defaultSIDMapper.UIDFromSID(s); ok {
		return uid
	}
	return 65534 // nobody
}

// sidToGID extracts a GID from a SID using the default mapper.
// Checks group SIDs first, then falls back to user SIDs for backward compat
// (old SIDs used the same format for both user and group).
// For non-domain SIDs, returns 65534 (nobody).
func sidToGID(s *sid.SID) uint32 {
	if gid, ok := defaultSIDMapper.GIDFromSID(s); ok {
		return gid
	}
	// Backward compat: old SIDs used user RID format for groups too
	if uid, ok := defaultSIDMapper.UIDFromSID(s); ok {
		return uid
	}
	return 65534 // nobody
}

// ============================================================================
// Alignment Helpers
// ============================================================================

// alignTo4 rounds up a value to the next 4-byte boundary.
func alignTo4(n uint32) uint32 {
	return (n + 3) &^ 3
}

// padTo4 pads a buffer to a 4-byte boundary with zeros.
func padTo4(buf *bytes.Buffer) {
	rem := buf.Len() % 4
	if rem != 0 {
		padding := 4 - rem
		for range padding {
			buf.WriteByte(0)
		}
	}
}

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
package handlers

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"

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
	seSelfRelative = 0x8000
	seDACLPresent  = 0x0004
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
// SID Types and Helpers
// ============================================================================

// SID represents a Windows Security Identifier per MS-DTYP Section 2.4.2.
//
// A SID uniquely identifies a security principal (user, group, or computer).
// The binary encoding is: Revision(1) + SubAuthorityCount(1) +
// IdentifierAuthority(6, big-endian) + SubAuthorities(4*N, little-endian).
type SID struct {
	// Revision is always 1.
	Revision uint8

	// SubAuthorityCount is the number of sub-authority values.
	SubAuthorityCount uint8

	// IdentifierAuthority is the top-level authority (6 bytes, big-endian).
	IdentifierAuthority [6]byte

	// SubAuthorities contains the sub-authority values.
	SubAuthorities []uint32
}

// SIDSize returns the binary size of a SID in bytes.
func SIDSize(sid *SID) int {
	return 8 + 4*int(sid.SubAuthorityCount)
}

// EncodeSID writes a binary SID to buf per MS-DTYP Section 2.4.2.
func EncodeSID(buf *bytes.Buffer, sid *SID) {
	buf.WriteByte(sid.Revision)
	buf.WriteByte(sid.SubAuthorityCount)
	buf.Write(sid.IdentifierAuthority[:])
	for _, sa := range sid.SubAuthorities {
		_ = binary.Write(buf, binary.LittleEndian, sa)
	}
}

// DecodeSID parses a binary SID from data per MS-DTYP Section 2.4.2.
// Returns the parsed SID and number of bytes consumed, or an error.
func DecodeSID(data []byte) (*SID, int, error) {
	if len(data) < 8 {
		return nil, 0, fmt.Errorf("SID too short: %d bytes", len(data))
	}

	sid := &SID{
		Revision:          data[0],
		SubAuthorityCount: data[1],
	}
	copy(sid.IdentifierAuthority[:], data[2:8])

	size := 8 + 4*int(sid.SubAuthorityCount)
	if len(data) < size {
		return nil, 0, fmt.Errorf("SID data too short for %d sub-authorities: have %d, need %d", sid.SubAuthorityCount, len(data), size)
	}

	sid.SubAuthorities = make([]uint32, sid.SubAuthorityCount)
	for i := 0; i < int(sid.SubAuthorityCount); i++ {
		offset := 8 + 4*i
		sid.SubAuthorities[i] = binary.LittleEndian.Uint32(data[offset : offset+4])
	}

	return sid, size, nil
}

// FormatSID formats a SID as a string in "S-1-5-21-..." format.
func FormatSID(sid *SID) string {
	// Compute the 48-bit authority value from big-endian 6 bytes
	var authority uint64
	for i := range 6 {
		authority = (authority << 8) | uint64(sid.IdentifierAuthority[i])
	}

	var b strings.Builder
	fmt.Fprintf(&b, "S-%d-%d", sid.Revision, authority)
	for _, sa := range sid.SubAuthorities {
		fmt.Fprintf(&b, "-%d", sa)
	}
	return b.String()
}

// ParseSIDString parses a SID string in "S-1-5-21-..." format.
func ParseSIDString(s string) (*SID, error) {
	if !strings.HasPrefix(s, "S-") {
		return nil, fmt.Errorf("invalid SID format: must start with S-")
	}

	parts := strings.Split(s[2:], "-")
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid SID format: need at least revision and authority")
	}

	revision, err := strconv.ParseUint(parts[0], 10, 8)
	if err != nil {
		return nil, fmt.Errorf("invalid SID revision: %w", err)
	}

	authority, err := strconv.ParseUint(parts[1], 10, 48)
	if err != nil {
		return nil, fmt.Errorf("invalid SID authority: %w", err)
	}

	sid := &SID{
		Revision:          uint8(revision),
		SubAuthorityCount: uint8(len(parts) - 2),
	}

	// Encode authority as big-endian 6 bytes
	for i := 5; i >= 0; i-- {
		sid.IdentifierAuthority[i] = byte(authority & 0xFF)
		authority >>= 8
	}

	sid.SubAuthorities = make([]uint32, sid.SubAuthorityCount)
	for i := 0; i < int(sid.SubAuthorityCount); i++ {
		val, err := strconv.ParseUint(parts[i+2], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid SID sub-authority %d: %w", i, err)
		}
		sid.SubAuthorities[i] = uint32(val)
	}

	return sid, nil
}

// parseSIDMust parses a SID string and panics on error. Used for well-known SIDs.
func parseSIDMust(s string) *SID {
	sid, err := ParseSIDString(s)
	if err != nil {
		panic(fmt.Sprintf("invalid well-known SID %q: %v", s, err))
	}
	return sid
}

// ============================================================================
// Well-Known SID Mapping
// ============================================================================

// Well-known SIDs for NFSv4 special identifiers and common Windows principals.
var (
	// sidEveryone is the "Everyone" (World) SID: S-1-1-0
	sidEveryone = parseSIDMust("S-1-1-0")
)

// wellKnownSIDToPrincipal maps well-known SID strings to NFSv4 principals.
var wellKnownSIDToPrincipal = map[string]string{
	"S-1-1-0": acl.SpecialEveryone, // Everyone
	"S-1-3-0": acl.SpecialOwner,    // CREATOR OWNER -> OWNER@
	"S-1-3-1": acl.SpecialGroup,    // CREATOR GROUP -> GROUP@
}

// makeDittoFSUserSID constructs a local user SID: S-1-5-21-0-0-0-{uid}
func makeDittoFSUserSID(uid uint32) *SID {
	return &SID{
		Revision:            1,
		SubAuthorityCount:   5,
		IdentifierAuthority: [6]byte{0, 0, 0, 0, 0, 5}, // SECURITY_NT_AUTHORITY
		SubAuthorities:      []uint32{21, 0, 0, 0, uid},
	}
}

// makeDittoFSGroupSID constructs a local group SID: S-1-5-21-0-0-0-{gid}
func makeDittoFSGroupSID(gid uint32) *SID {
	return makeDittoFSUserSID(gid) // Same format, GID used as RID
}

// PrincipalToSID converts an NFSv4 principal to a Windows SID.
//
// Mapping rules:
//   - "OWNER@": constructs user SID from file owner UID (S-1-5-21-0-0-0-{UID})
//   - "GROUP@": constructs group SID from file owner GID (S-1-5-21-0-0-0-{GID})
//   - "EVERYONE@": returns S-1-1-0
//   - Otherwise: constructs user SID from principal (hash or numeric UID)
func PrincipalToSID(who string, fileOwnerUID, fileOwnerGID uint32) *SID {
	switch who {
	case acl.SpecialOwner:
		return makeDittoFSUserSID(fileOwnerUID)
	case acl.SpecialGroup:
		return makeDittoFSGroupSID(fileOwnerGID)
	case acl.SpecialEveryone:
		return sidEveryone
	default:
		// Try to extract a numeric UID from "1000@localdomain" format
		if idx := strings.Index(who, "@"); idx > 0 {
			if uid, err := strconv.ParseUint(who[:idx], 10, 32); err == nil {
				return makeDittoFSUserSID(uint32(uid))
			}
		}
		// Fallback: use a hash-based RID
		var rid uint32
		for _, c := range who {
			rid = rid*31 + uint32(c)
		}
		return makeDittoFSUserSID(rid)
	}
}

// isDittoFSUserSID reports whether the SID matches the DittoFS user SID pattern
// S-1-5-21-0-0-0-{RID}. If it matches, the RID is returned.
func isDittoFSUserSID(sid *SID) (rid uint32, ok bool) {
	if sid.Revision == 1 &&
		sid.IdentifierAuthority == [6]byte{0, 0, 0, 0, 0, 5} &&
		sid.SubAuthorityCount >= 5 &&
		sid.SubAuthorities[0] == 21 &&
		sid.SubAuthorities[1] == 0 &&
		sid.SubAuthorities[2] == 0 &&
		sid.SubAuthorities[3] == 0 {
		return sid.SubAuthorities[4], true
	}
	return 0, false
}

// SIDToPrincipal converts a Windows SID to an NFSv4 principal.
//
// Mapping rules:
//   - Well-known SIDs are mapped directly (S-1-1-0 -> "EVERYONE@")
//   - User SIDs (S-1-5-21-...) extract RID as UID and format as "{uid}@localdomain"
//   - Unknown SIDs return their string representation
func SIDToPrincipal(sid *SID) string {
	sidStr := FormatSID(sid)
	if principal, ok := wellKnownSIDToPrincipal[sidStr]; ok {
		return principal
	}

	if rid, ok := isDittoFSUserSID(sid); ok {
		return fmt.Sprintf("%d@localdomain", rid)
	}

	return sidStr
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
//
// If additionalSecInfo is 0, all sections (owner, group, DACL) are included.
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

	// Build SIDs
	ownerSID := makeDittoFSUserSID(file.UID)
	groupSID := makeDittoFSGroupSID(file.GID)

	// Build DACL
	var daclBuf bytes.Buffer
	if includeDACL {
		buildDACL(&daclBuf, file)
	}

	// Compute offsets
	// Header is always 20 bytes
	var ownerOffset, groupOffset, daclOffset uint32
	currentOffset := uint32(sdHeaderSize)

	if includeOwner {
		ownerOffset = currentOffset
		currentOffset += uint32(SIDSize(ownerSID))
		// Pad to 4-byte boundary
		currentOffset = alignTo4(currentOffset)
	}

	if includeGroup {
		groupOffset = currentOffset
		currentOffset += uint32(SIDSize(groupSID))
		currentOffset = alignTo4(currentOffset)
	}

	if includeDACL {
		daclOffset = currentOffset
		// daclBuf already includes the ACL header and all ACEs
	}

	// Build the complete Security Descriptor
	var buf bytes.Buffer

	// Control flags
	control := uint16(seSelfRelative)
	if includeDACL {
		control |= seDACLPresent
	}

	// Header (20 bytes)
	buf.WriteByte(1) // Revision
	buf.WriteByte(0) // Sbz1
	_ = binary.Write(&buf, binary.LittleEndian, control)
	_ = binary.Write(&buf, binary.LittleEndian, ownerOffset)
	_ = binary.Write(&buf, binary.LittleEndian, groupOffset)
	_ = binary.Write(&buf, binary.LittleEndian, uint32(0)) // OffsetSacl (no SACL)
	_ = binary.Write(&buf, binary.LittleEndian, daclOffset)

	// Owner SID
	if includeOwner {
		EncodeSID(&buf, ownerSID)
		padTo4(&buf)
	}

	// Group SID
	if includeGroup {
		EncodeSID(&buf, groupSID)
		padTo4(&buf)
	}

	// DACL
	if includeDACL {
		buf.Write(daclBuf.Bytes())
	}

	return buf.Bytes(), nil
}

// windowsACE is an internal representation of a Windows ACE for encoding.
type windowsACE struct {
	aceType    uint8
	aceFlags   uint8
	accessMask uint32
	sid        *SID
}

// buildDACL constructs a DACL (Discretionary Access Control List) from the file's ACL.
// If the file has no ACL, a minimal DACL granting Everyone full access is built.
func buildDACL(buf *bytes.Buffer, file *metadata.File) {
	var aces []windowsACE

	if file.ACL != nil && len(file.ACL.ACEs) > 0 {
		aces = make([]windowsACE, 0, len(file.ACL.ACEs))
		for _, ace := range file.ACL.ACEs {
			aceType, ok := nfsToWindowsACEType(ace.Type)
			if !ok {
				continue
			}

			aces = append(aces, windowsACE{
				aceType:    aceType,
				aceFlags:   uint8(ace.Flag & 0xFF),
				accessMask: ace.AccessMask,
				sid:        PrincipalToSID(ace.Who, file.UID, file.GID),
			})
		}
	} else {
		// No ACL: build minimal DACL granting Everyone full access
		aces = append(aces, windowsACE{
			aceType:    accessAllowedACEType,
			aceFlags:   0,
			accessMask: 0x001F01FF, // FILE_ALL_ACCESS
			sid:        sidEveryone,
		})
	}

	// Compute total ACL size
	totalACLSize := aclHeaderSize
	for i := range aces {
		totalACLSize += aceHeaderSize + SIDSize(aces[i].sid)
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
		aceSize := uint16(aceHeaderSize + SIDSize(ace.sid))

		buf.WriteByte(ace.aceType)  // AceType
		buf.WriteByte(ace.aceFlags) // AceFlags
		_ = binary.Write(buf, binary.LittleEndian, aceSize)
		_ = binary.Write(buf, binary.LittleEndian, ace.accessMask)
		EncodeSID(buf, ace.sid)
	}
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
		sid, _, err := DecodeSID(data[offsetOwner:])
		if err == nil {
			uid := sidToUID(sid)
			ownerUID = &uid
		}
	}

	// Parse Group SID
	if offsetGroup > 0 && int(offsetGroup) < len(data) {
		sid, _, err := DecodeSID(data[offsetGroup:])
		if err == nil {
			gid := sidToUID(sid) // Same extraction logic for GID
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
		sid, _, err := DecodeSID(data[offset+aceHeaderSize : offset+int(aceSize)])
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
		who := SIDToPrincipal(sid)

		aces = append(aces, acl.ACE{
			Type:       nfsACEType,
			Flag:       uint32(aceFlags),
			AccessMask: accessMask,
			Who:        who,
		})

		offset += int(aceSize)
	}

	return &acl.ACL{ACEs: aces}, nil
}

// sidToUID extracts a UID from a DittoFS user SID (S-1-5-21-0-0-0-{RID}).
// For non-DittoFS SIDs, returns 65534 (nobody).
func sidToUID(sid *SID) uint32 {
	if rid, ok := isDittoFSUserSID(sid); ok {
		return rid
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

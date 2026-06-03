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
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
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

// SDBufferCreateContextTag is the SMB2_CREATE_SD_BUFFER create context tag
// per MS-SMB2 §2.2.13.2.2 — client supplies an initial SD at CREATE time.
const SDBufferCreateContextTag = "SecD"

// Security Descriptor control flags per MS-DTYP Section 2.4.6.
const (
	seSelfRelative       = 0x8000
	seDACLPresent        = 0x0004
	seSACLPresent        = 0x0010
	seDACLAutoInheritReq = 0x0100 // SE_DACL_AUTO_INHERIT_REQ — client requests inheritance computation
	seDACLAutoInherited  = 0x0400
	seDACLProtected      = 0x1000
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
// If additionalSecInfo is 0, no sections are included and a minimal empty SD
// is returned (header only). Callers should pass explicit flags for the
// sections they need.
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
	includeOwner := (additionalSecInfo & OwnerSecurityInformation) != 0
	includeGroup := (additionalSecInfo & GroupSecurityInformation) != 0
	includeDACL := (additionalSecInfo & DACLSecurityInformation) != 0
	includeSACL := (additionalSecInfo & SACLSecurityInformation) != 0

	// Build SIDs using the shared mapper
	ownerSID := defaultSIDMapper.UserSID(file.UID)
	groupSID := defaultSIDMapper.GroupSID(file.GID)

	// Null DACL: SE_DACL_PRESENT set but no DACL body (daclOffset stays 0)
	isNullDACL := includeDACL && file.ACL != nil && file.ACL.NullDACL

	// Build DACL
	var daclBuf bytes.Buffer
	var fileACL *acl.ACL
	if includeDACL && !isNullDACL {
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

	// Round-trip SE_DACL_AUTO_INHERITED from the parsed ACL (MS-DTYP §2.4.6,
	// §2.5.3.4.2). The SD-level Control bit and per-ACE
	// SEC_ACE_FLAG_INHERITED_ACE are independent fields per MS-DTYP §2.4.4.2,
	// so AutoInherited is the sole driver here. Parse-side canonicalization
	// (mirroring Samba source3/smbd/smb2_nttrans.c::canonicalize_inheritance_bits)
	// ensures AutoInherited reflects only client SETs of (AUTO_INHERITED &&
	// AUTO_INHERIT_REQ).
	// Source for control flags: the built DACL, or file.ACL for null DACL case
	flagSource := fileACL
	if flagSource == nil && isNullDACL {
		flagSource = file.ACL
	}
	if flagSource != nil {
		if flagSource.AutoInherited {
			control |= seDACLAutoInherited
		}
		if flagSource.Protected {
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

	if includeDACL && !isNullDACL {
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
	}

	// Build the complete Security Descriptor
	var buf bytes.Buffer

	// Header (20 bytes)
	buf.WriteByte(1) // Revision
	buf.WriteByte(0) // Sbz1
	writeUint16ToBuf(&buf, control)
	writeUint32ToBuf(&buf, ownerOffset)
	writeUint32ToBuf(&buf, groupOffset)
	writeUint32ToBuf(&buf, saclOffset)
	writeUint32ToBuf(&buf, daclOffset)

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
	case acl.SpecialOwnerRights:
		return sid.WellKnownOwnerRights
	case acl.SpecialCreatorOwner:
		return sid.WellKnownCreatorOwner
	case acl.SpecialCreatorGroup:
		return sid.WellKnownCreatorGroup
	case acl.SpecialSystem:
		return sid.WellKnownSystem
	case acl.SpecialAdministrators:
		return sid.WellKnownAdministrators
	default:
		return defaultSIDMapper.PrincipalToSID(who, fileUID, fileGID)
	}
}

// buildDACL constructs a DACL (Discretionary Access Control List) from the file's ACL.
//
// Per pkg/metadata/file_types.go FileAttr.ACL contract:
//   - file.ACL == nil          → no ACL stored; synthesize Windows-default
//     (owner + SYSTEM FullControl, no inherit flags) via
//     acl.SynthesizeWindowsDefault. Matches Samba's sd_def1
//     (source4/torture/smb2/acls.c).
//   - file.ACL != nil, len==0  → explicit empty DACL (deny-all). Emit a
//     0-ACE DACL on the wire; do NOT synthesize a default.
//   - file.ACL != nil, len>0   → emit stored ACEs as-is.
//
// Note: server-side access checks and Service.ComputeMaximalAccess still
// fall back to POSIX mode bits for nil ACL — this is intentional. The SD shape
// here is what the client sees; actual access enforcement keeps the legacy
// POSIX semantics so Unix mode bits stay authoritative on the server.
//
// Returns the source ACL used for SD control flag computation.
func buildDACL(buf *bytes.Buffer, file *metadata.File) *acl.ACL {
	var aces []windowsACE
	var fileACL *acl.ACL

	if file.ACL != nil {
		fileACL = file.ACL
	} else {
		fileACL = acl.SynthesizeWindowsDefault()
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
	writeUint16ToBuf(buf, uint16(totalACLSize))
	writeUint16ToBuf(buf, uint16(len(aces)))
	writeUint16ToBuf(buf, 0) // Sbz2

	// Write each ACE per MS-DTYP Section 2.4.4.2
	for i := range aces {
		ace := &aces[i]
		aceSize := uint16(aceHeaderSize + sid.SIDSize(ace.sid))

		buf.WriteByte(ace.aceType)  // AceType
		buf.WriteByte(ace.aceFlags) // AceFlags
		writeUint16ToBuf(buf, aceSize)
		writeUint32ToBuf(buf, ace.accessMask)
		sid.EncodeSID(buf, ace.sid)
	}

	return fileACL
}

// buildEmptySACL writes a valid empty SACL to buf.
// The SACL has revision=2, count=0, and total size=8 bytes.
func buildEmptySACL(buf *bytes.Buffer) {
	buf.WriteByte(2)                     // AclRevision
	buf.WriteByte(0)                     // Sbz1
	writeUint16ToBuf(buf, aclHeaderSize) // AclSize = 8
	writeUint16ToBuf(buf, 0)             // AceCount = 0
	writeUint16ToBuf(buf, 0)             // Sbz2
}

// ============================================================================
// Security Descriptor Parsing
// ============================================================================

// ParseSDOptions controls server-side handling of Security Descriptor parsing.
type ParseSDOptions struct {
	// CanonicalizeAutoInherited applies MS-DTYP §2.5.3.4.2 canonicalization:
	// SE_DACL_AUTO_INHERITED is persisted only when SET_INFO Security also
	// carries SE_DACL_AUTO_INHERIT_REQ. Mirrors Samba's
	// `acl flag inherited canonicalization = yes` (default).
	//
	// When false, AUTO_INHERITED is preserved verbatim from the inbound
	// Control word — Samba extension `acl flag inherited canonicalization = no`.
	// smbtorture smb2.acls_non_canonical.flags exercises this opt-out.
	CanonicalizeAutoInherited bool
}

// ParseSecurityDescriptor parses a self-relative SD with default (canonicalizing)
// semantics. New callers should prefer ParseSecurityDescriptorWithOptions and
// pass an explicit ParseSDOptions reflecting the target share's
// AclFlagInheritedCanonicalization setting.
//
// Returns pointers to allow callers to detect which sections were present.
// A nil pointer means that section was not present in the SD.
func ParseSecurityDescriptor(data []byte) (*uint32, *uint32, *acl.ACL, error) {
	return ParseSecurityDescriptorWithOptions(data, ParseSDOptions{CanonicalizeAutoInherited: true})
}

// ParseSecurityDescriptorWithOptions parses a self-relative Security Descriptor
// and extracts the owner UID, group GID, and NFSv4 ACL, honoring the
// canonicalization toggle in opts. See ParseSDOptions for semantics.
//
// Returns pointers to allow callers to detect which sections were present.
// A nil pointer means that section was not present in the SD.
func ParseSecurityDescriptorWithOptions(data []byte, opts ParseSDOptions) (ownerUID *uint32, ownerGID *uint32, fileACL *acl.ACL, err error) {
	if len(data) < sdHeaderSize {
		return nil, nil, nil, fmt.Errorf("security descriptor too short: %d bytes", len(data))
	}

	// Parse header
	r := smbenc.NewReader(data)
	r.Skip(2) // Revision(1) + Sbz1(1)
	control := r.ReadUint16()
	offsetOwner := r.ReadUint32()
	offsetGroup := r.ReadUint32()
	r.Skip(4) // offsetSACL (SACL parsing not implemented; preserve-on-write is future work)
	offsetDACL := r.ReadUint32()
	if r.Err() != nil {
		return nil, nil, nil, fmt.Errorf("failed to parse SD header: %w", r.Err())
	}

	// Parse Owner SID — only set ownerUID if the SID is recognized
	if offsetOwner > 0 && int(offsetOwner) < len(data) {
		s, _, err := sid.DecodeSID(data[offsetOwner:])
		if err == nil {
			if uid, ok := defaultSIDMapper.UIDFromSID(s); ok {
				ownerUID = &uid
			}
		}
	}

	// Parse Group SID — only set ownerGID if the SID is recognized
	if offsetGroup > 0 && int(offsetGroup) < len(data) {
		s, _, err := sid.DecodeSID(data[offsetGroup:])
		if err == nil {
			if gid, ok := defaultSIDMapper.GIDFromSID(s); ok {
				ownerGID = &gid
			} else if uid, ok := defaultSIDMapper.UIDFromSID(s); ok {
				ownerGID = &uid
			}
		}
	}

	// Parse DACL
	if offsetDACL > 0 && int(offsetDACL)+aclHeaderSize <= len(data) {
		daclData := data[offsetDACL:]
		fileACL, err = parseDACL(daclData)
		if err != nil {
			return ownerUID, ownerGID, nil, fmt.Errorf("failed to parse DACL: %w", err)
		}
	} else if offsetDACL == 0 && control&seDACLPresent != 0 {
		// SE_DACL_PRESENT set but offset is zero → null DACL (everyone full access)
		fileACL = &acl.ACL{NullDACL: true}
	}

	// Surface SD Control flags onto the ACL so SE_DACL_PROTECTED and
	// SE_DACL_AUTO_INHERITED round-trip through SET_INFO Security.
	//
	// When opts.CanonicalizeAutoInherited is true (default), apply MS-DTYP
	// §2.5.3.4.2 canonicalization mirroring Samba
	// source3/smbd/smb2_nttrans.c::canonicalize_inheritance_bits: the
	// AUTO_INHERIT_REQ bit is a request flag — server processes it and never
	// echoes it back. AUTO_INHERITED is persisted only when the client SETs
	// BOTH AUTO_INHERITED and AUTO_INHERIT_REQ; otherwise it is cleared.
	//
	// When opts.CanonicalizeAutoInherited is false, the Samba extension
	// `acl flag inherited canonicalization = no` is in effect: AUTO_INHERITED
	// is preserved verbatim from the inbound Control word with no
	// AUTO_INHERIT_REQ gate. See ParseSDOptions and
	// ParseSecurityDescriptorWithOptions.
	if fileACL != nil {
		if control&seDACLProtected != 0 {
			fileACL.Protected = true
		}
		autoInherited := control&seDACLAutoInherited != 0
		if opts.CanonicalizeAutoInherited {
			autoInheritReq := control&seDACLAutoInheritReq != 0
			if autoInheritReq && autoInherited {
				fileACL.AutoInherited = true
			}
		} else {
			// Samba extension: preserve AUTO_INHERITED verbatim, no AUTO_INHERIT_REQ gate.
			if autoInherited {
				fileACL.AutoInherited = true
			}
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
	aclR := smbenc.NewReader(data)
	aclR.Skip(2) // AclRevision(1) + Sbz1(1)
	aclR.Skip(2) // AclSize (not used)
	aceCount := aclR.ReadUint16()
	// Sbz2 not needed

	if aceCount > acl.MaxACECount {
		return nil, fmt.Errorf("DACL has %d ACEs, exceeds maximum %d", aceCount, acl.MaxACECount)
	}

	aces := make([]acl.ACE, 0, aceCount)
	offset := aclHeaderSize

	for i := 0; i < int(aceCount); i++ {
		if offset+aceHeaderSize > len(data) {
			return nil, fmt.Errorf("ACE %d extends beyond DACL data", i)
		}

		aceR := smbenc.NewReader(data[offset:])
		aceType := aceR.ReadUint8()
		aceFlags := aceR.ReadUint8()
		aceSize := aceR.ReadUint16()
		accessMask := aceR.ReadUint32()
		if aceR.Err() != nil {
			return nil, fmt.Errorf("ACE %d header parse error: %w", i, aceR.Err())
		}

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

		// Expand GENERIC_* bits to file-object-specific rights per
		// MS-DTYP §2.4.3 / §2.5.3 before storing. Generic rights MUST
		// be mapped at the SD-update boundary so subsequent access checks
		// (MS-FSA §2.1.5.1.2.1) operate on resolved masks.
		aces = append(aces, acl.ACE{
			Type:       nfsACEType,
			Flag:       acl.WindowsFlagsToNFSv4Flags(aceFlags),
			AccessMask: acl.ExpandGenericMask(accessMask),
			Who:        who,
		})

		offset += int(aceSize)
	}

	return &acl.ACL{
		ACEs:   aces,
		Source: acl.ACLSourceSMBExplicit,
	}, nil
}

// ============================================================================
// Alignment Helpers
// ============================================================================

// writeUint16ToBuf writes a little-endian uint16 to a bytes.Buffer.
func writeUint16ToBuf(buf *bytes.Buffer, v uint16) {
	var tmp [2]byte
	tmp[0] = byte(v)
	tmp[1] = byte(v >> 8)
	buf.Write(tmp[:])
}

// writeUint32ToBuf writes a little-endian uint32 to a bytes.Buffer.
func writeUint32ToBuf(buf *bytes.Buffer, v uint32) {
	var tmp [4]byte
	tmp[0] = byte(v)
	tmp[1] = byte(v >> 8)
	tmp[2] = byte(v >> 16)
	tmp[3] = byte(v >> 24)
	buf.Write(tmp[:])
}

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

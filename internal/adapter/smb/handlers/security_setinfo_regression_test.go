// Regression tests for smbtorture acls.DENY1 / INHERITFLAGS / CREATOR /
// INHERITANCE SET_INFO Security paths. The Samba tests build SDs via
// security_descriptor_dacl_create() then SETINFO; DittoFS previously
// returned STATUS_INVALID_PARAMETER for DENY1 / INHERITFLAGS because
// ValidateACL rejected ACE arrays that were not in Windows canonical
// presentation order (explicit DENY before explicit ALLOW, etc.).
//
// Per MS-DTYP §2.4.5 the ACL wire layout is an unordered ACE array;
// canonical order is a UI convention, not a wire requirement. Windows
// and Samba accept non-canonical DACLs on SET_INFO Security, and the
// evaluator walks the array in stored order so semantics remain
// deterministic. These tests construct raw self-relative SD bytes that
// mirror what Samba emits on the wire (per MS-DTYP §2.4.6 layout),
// feed them to ParseSecurityDescriptor, and assert both parse and
// ValidateACL accept the result.
package handlers

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/pkg/auth/sid"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
)

// buildSID returns the binary encoding of a SID built from authority + subauths.
func buildSID(t *testing.T, sidStr string) []byte {
	t.Helper()
	s, err := sid.ParseSIDString(sidStr)
	if err != nil {
		t.Fatalf("ParseSIDString(%q): %v", sidStr, err)
	}
	var buf bytes.Buffer
	sid.EncodeSID(&buf, s)
	return buf.Bytes()
}

// buildACE builds a single ACE (type + flags + size + mask + sid).
func buildACE(aceType, aceFlags uint8, accessMask uint32, sidBytes []byte) []byte {
	var buf bytes.Buffer
	aceSize := uint16(8 + len(sidBytes))
	buf.WriteByte(aceType)
	buf.WriteByte(aceFlags)
	writeUint16ToBuf(&buf, aceSize)
	writeUint32ToBuf(&buf, accessMask)
	buf.Write(sidBytes)
	return buf.Bytes()
}

// buildRawDACL builds a DACL: AclRev(1) + Sbz1(1) + AclSize(2) + AceCount(2) + Sbz2(2) + ACEs.
func buildRawDACL(aceCount uint16, aces []byte) []byte {
	var buf bytes.Buffer
	aclSize := uint16(8 + len(aces))
	buf.WriteByte(2) // AclRevision = ACL_REVISION (2)
	buf.WriteByte(0) // Sbz1
	writeUint16ToBuf(&buf, aclSize)
	writeUint16ToBuf(&buf, aceCount)
	writeUint16ToBuf(&buf, 0) // Sbz2
	buf.Write(aces)
	return buf.Bytes()
}

// buildSelfRelativeSD assembles a self-relative SD per MS-DTYP §2.4.6.
// Layout: Header(20) + DACL + Owner + Group.
//
// Pass nil for ownerSID / groupSID to indicate "absent" (offset=0).
// Pass nil for dacl to indicate no DACL.
func buildSelfRelativeSD(t *testing.T, control uint16, ownerSID, groupSID, daclBytes []byte) []byte {
	t.Helper()
	var buf bytes.Buffer

	// Header
	buf.WriteByte(1) // Revision
	buf.WriteByte(0) // Sbz1
	writeUint16ToBuf(&buf, control|0x8000)

	// Offsets — fill in after we know layout
	// Order on wire: DACL after header, Owner after DACL, Group after Owner.
	offsetDACL := uint32(0)
	offsetOwner := uint32(0)
	offsetGroup := uint32(0)
	offsetSACL := uint32(0)

	cursor := uint32(20)
	if daclBytes != nil {
		offsetDACL = cursor
		cursor += uint32(len(daclBytes))
	}
	if ownerSID != nil {
		offsetOwner = cursor
		cursor += uint32(len(ownerSID))
	}
	if groupSID != nil {
		offsetGroup = cursor
	}

	writeUint32ToBuf(&buf, offsetOwner)
	writeUint32ToBuf(&buf, offsetGroup)
	writeUint32ToBuf(&buf, offsetSACL)
	writeUint32ToBuf(&buf, offsetDACL)

	if daclBytes != nil {
		buf.Write(daclBytes)
	}
	if ownerSID != nil {
		buf.Write(ownerSID)
	}
	if groupSID != nil {
		buf.Write(groupSID)
	}
	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// Test cases: one per failing smbtorture test.
// ---------------------------------------------------------------------------

// TestSetInfoRepro_DENY1 mirrors source4/torture/smb2/acls.c::test_deny1.
// SD has 2 ACEs (ALLOW + DENY) for owner_sid. owner present, group NULL,
// SACL absent. SD type=0x8004 (self-rel | DACL_PRESENT).
func TestSetInfoRepro_DENY1(t *testing.T) {
	owner := buildSID(t, "S-1-5-21-1000-1000-1000-500") // arbitrary user-style SID

	const (
		secRightsFileRead uint32 = 0x00120089
		secFileWriteData  uint32 = 0x00000002
	)

	ace1 := buildACE(0x00, 0x00, secRightsFileRead|secFileWriteData, owner) // ALLOW
	ace2 := buildACE(0x01, 0x00, secFileWriteData, owner)                   // DENY

	dacl := buildRawDACL(2, append(ace1, ace2...))
	sd := buildSelfRelativeSD(t, 0x0004, owner, nil, dacl)

	t.Logf("DENY1 SD: %d bytes, hex=% x", len(sd), sd)
	ownerUID, ownerGID, fileACL, err := ParseSecurityDescriptor(sd)
	if err != nil {
		t.Errorf("DENY1 ParseSecurityDescriptor REJECTED: %v", err)
		return
	}
	t.Logf("DENY1 OK: ownerUID=%v ownerGID=%v acl=%v", ownerUID, ownerGID, fileACL)
	// Downstream: SetFileAttributes calls acl.ValidateACL.
	if err := acl.ValidateACL(fileACL); err != nil {
		t.Errorf("DENY1 ValidateACL REJECTED (this is the real rejection path -> ErrInvalidArgument -> StatusInvalidParameter): %v", err)
	}
}

// TestSetInfoRepro_INHERITFLAGS mirrors test_inheritance_flags.
// SD has 2 ACEs (owner_sid + SID_WORLD) with OI|CI flags. owner=NULL,
// group=NULL. SD type varies: 0x8004 plus combos of
// DACL_AUTO_INHERITED(0x0400), DACL_AUTO_INHERIT_REQ(0x0100),
// DACL_PROTECTED(0x1000).
func TestSetInfoRepro_INHERITFLAGS(t *testing.T) {
	owner := buildSID(t, "S-1-5-21-1000-1000-1000-500")
	world := buildSID(t, "S-1-1-0")

	const (
		secFileWriteData uint32 = 0x00000002
		secStdWriteDAC   uint32 = 0x00040000
		secFileAll       uint32 = 0x001F01FF
		secStdAll        uint32 = 0x001F0000
		oi               uint8  = 0x01
		ci               uint8  = 0x02
	)

	// i=15: all set bits — AUTO_INHERITED | AUTO_INHERIT_REQ | PROTECTED | INHERITED ACE flag
	ace1 := buildACE(0x00, oi|ci|0x10 /* INHERITED_ACE */, secFileWriteData|secStdWriteDAC, owner)
	ace2 := buildACE(0x00, 0x00, secFileAll|secStdAll, world)
	dacl := buildRawDACL(2, append(ace1, ace2...))

	control := uint16(0x0004 | 0x0400 | 0x0100 | 0x1000) // DACL_PRESENT | AUTO_INH | AUTO_INH_REQ | PROTECTED
	sd := buildSelfRelativeSD(t, control, nil, nil, dacl)

	t.Logf("INHERITFLAGS SD: %d bytes, control=0x%04x, hex=% x", len(sd), control, sd)
	ownerUID, ownerGID, fileACL, err := ParseSecurityDescriptor(sd)
	if err != nil {
		t.Errorf("INHERITFLAGS ParseSecurityDescriptor REJECTED: %v", err)
		return
	}
	t.Logf("INHERITFLAGS OK: ownerUID=%v ownerGID=%v acl protected=%v autoinh=%v",
		ownerUID, ownerGID, fileACL.Protected, fileACL.AutoInherited)
	if err := acl.ValidateACL(fileACL); err != nil {
		t.Errorf("INHERITFLAGS ValidateACL REJECTED: %v", err)
	}
}

// TestSetInfoRepro_CREATOR mirrors test_creator_sid first SET_INFO.
// SD has 1 ACE for SID_CREATOR_OWNER (S-1-3-0). owner=NULL, group=NULL.
func TestSetInfoRepro_CREATOR(t *testing.T) {
	creator := buildSID(t, "S-1-3-0")
	const (
		secRightsFileRead uint32 = 0x00120089
		secStdAll         uint32 = 0x001F0000
	)
	ace := buildACE(0x00, 0x00, secRightsFileRead|secStdAll, creator)
	dacl := buildRawDACL(1, ace)
	sd := buildSelfRelativeSD(t, 0x0004, nil, nil, dacl)

	t.Logf("CREATOR SD: %d bytes, hex=% x", len(sd), sd)
	ownerUID, ownerGID, fileACL, err := ParseSecurityDescriptor(sd)
	if err != nil {
		t.Errorf("CREATOR ParseSecurityDescriptor REJECTED: %v", err)
		return
	}
	t.Logf("CREATOR OK: ownerUID=%v ownerGID=%v acl=%+v", ownerUID, ownerGID, fileACL)
	if err := acl.ValidateACL(fileACL); err != nil {
		t.Errorf("CREATOR ValidateACL REJECTED: %v", err)
	}
}

// TestSetInfoRepro_INHERITANCE mirrors test_inheritance.
// SD has 2 ACEs (SID_CREATOR_OWNER + SID_WORLD). owner=NULL, group=NULL.
func TestSetInfoRepro_INHERITANCE(t *testing.T) {
	creator := buildSID(t, "S-1-3-0")
	world := buildSID(t, "S-1-1-0")

	const (
		secFileWriteData uint32 = 0x00000002
		secFileAll       uint32 = 0x001F01FF
		secStdAll        uint32 = 0x001F0000
		oi               uint8  = 0x01
	)

	// Variant with OI flag (most interesting; test iterates flag combos).
	ace1 := buildACE(0x00, oi, secFileWriteData, creator)
	ace2 := buildACE(0x00, 0x00, secFileAll|secStdAll, world)
	dacl := buildRawDACL(2, append(ace1, ace2...))
	sd := buildSelfRelativeSD(t, 0x0004, nil, nil, dacl)

	t.Logf("INHERITANCE SD: %d bytes, hex=% x", len(sd), sd)
	ownerUID, ownerGID, fileACL, err := ParseSecurityDescriptor(sd)
	if err != nil {
		t.Errorf("INHERITANCE ParseSecurityDescriptor REJECTED: %v", err)
		return
	}
	t.Logf("INHERITANCE OK: ownerUID=%v ownerGID=%v acl=%+v", ownerUID, ownerGID, fileACL)
	if err := acl.ValidateACL(fileACL); err != nil {
		t.Errorf("INHERITANCE ValidateACL REJECTED: %v", err)
	}
}

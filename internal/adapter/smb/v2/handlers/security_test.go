package handlers

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"

	"github.com/marmos91/dittofs/pkg/auth/sid"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
)

// TestMain sets up a deterministic SIDMapper for all tests in this package.
func TestMain(m *testing.M) {
	// Use a fixed machine SID (0,0,0) for deterministic test results.
	// This matches the default fallback mapper.
	SetSIDMapper(sid.NewSIDMapper(0, 0, 0))
	os.Exit(m.Run())
}

func TestBuildSecurityDescriptorWithACL(t *testing.T) {
	file := &metadata.File{
		FileAttr: metadata.FileAttr{
			UID:  1000,
			GID:  1000,
			Mode: 0o755,
			ACL: &acl.ACL{
				ACEs: []acl.ACE{
					{
						Type:       acl.ACE4_ACCESS_DENIED_ACE_TYPE,
						Flag:       0,
						AccessMask: acl.ACE4_WRITE_DATA | acl.ACE4_APPEND_DATA,
						Who:        "EVERYONE@",
					},
					{
						Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
						Flag:       0,
						AccessMask: acl.ACE4_READ_DATA | acl.ACE4_EXECUTE,
						Who:        "EVERYONE@",
					},
					{
						Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
						Flag:       0,
						AccessMask: 0x001F01FF, // Full access
						Who:        "OWNER@",
					},
				},
			},
		},
	}

	data, err := BuildSecurityDescriptor(file, 0)
	if err != nil {
		t.Fatalf("BuildSecurityDescriptor: %v", err)
	}

	// Verify basic structure
	if len(data) < sdHeaderSize {
		t.Fatalf("Security descriptor too short: %d bytes", len(data))
	}
	if data[0] != 1 {
		t.Errorf("Revision = %d, want 1", data[0])
	}

	// Verify 4-byte alignment of the entire SD
	if len(data)%4 != 0 {
		t.Errorf("Security descriptor size %d is not 4-byte aligned", len(data))
	}

	// Round-trip: parse it back
	ownerUID, ownerGID, parsedACL, err := ParseSecurityDescriptor(data)
	if err != nil {
		t.Fatalf("ParseSecurityDescriptor: %v", err)
	}
	if ownerUID == nil || *ownerUID != 1000 {
		t.Errorf("Owner UID = %v, want 1000", ownerUID)
	}
	if ownerGID == nil || *ownerGID != 1000 {
		t.Errorf("Owner GID = %v, want 1000", ownerGID)
	}
	if parsedACL == nil {
		t.Fatal("Parsed ACL is nil")
	}
	if len(parsedACL.ACEs) != 3 {
		t.Fatalf("Parsed ACL has %d ACEs, want 3", len(parsedACL.ACEs))
	}

	// Verify ACE types and masks
	if parsedACL.ACEs[0].Type != acl.ACE4_ACCESS_DENIED_ACE_TYPE {
		t.Errorf("ACE[0].Type = %d, want DENY(%d)", parsedACL.ACEs[0].Type, acl.ACE4_ACCESS_DENIED_ACE_TYPE)
	}
	if parsedACL.ACEs[1].Type != acl.ACE4_ACCESS_ALLOWED_ACE_TYPE {
		t.Errorf("ACE[1].Type = %d, want ALLOW(%d)", parsedACL.ACEs[1].Type, acl.ACE4_ACCESS_ALLOWED_ACE_TYPE)
	}
	if parsedACL.ACEs[0].AccessMask != acl.ACE4_WRITE_DATA|acl.ACE4_APPEND_DATA {
		t.Errorf("ACE[0].AccessMask = 0x%x, want 0x%x", parsedACL.ACEs[0].AccessMask, acl.ACE4_WRITE_DATA|acl.ACE4_APPEND_DATA)
	}

	// Verify parsed ACL is marked as SMB explicit (round-trip through parseDACL)
	if parsedACL.Source != acl.ACLSourceSMBExplicit {
		t.Errorf("Parsed ACL.Source = %q, want %q", parsedACL.Source, acl.ACLSourceSMBExplicit)
	}
}

// TestBuildSD_NilACL_SynthesizesWindowsDefault verifies that
// BuildSecurityDescriptor emits the Windows default DACL when the file
// has no explicit ACL — owner + SYSTEM FULL_CONTROL, flags=0. Matches
// Samba's sd_def1 (source4/torture/smb2/acls.c) and unblocks the
// non-inheritable rows of smb2.acls.INHERITANCE.
func TestBuildSD_NilACL_SynthesizesWindowsDefault(t *testing.T) {
	file := &metadata.File{
		FileAttr: metadata.FileAttr{
			UID:  1000,
			GID:  1000,
			Mode: 0o755,
		},
	}

	data, err := BuildSecurityDescriptor(file, 0)
	if err != nil {
		t.Fatalf("BuildSecurityDescriptor: %v", err)
	}

	_, _, parsedACL, err := ParseSecurityDescriptor(data)
	if err != nil {
		t.Fatalf("ParseSecurityDescriptor: %v", err)
	}
	if parsedACL == nil {
		t.Fatal("Expected DACL, got nil")
	}

	// Windows default = 2 ACEs (owner FULL + SYSTEM FULL), no inherit flags.
	if len(parsedACL.ACEs) != 2 {
		t.Fatalf("Expected 2 ACEs (Windows default), got %d", len(parsedACL.ACEs))
	}

	for i, ace := range parsedACL.ACEs {
		if ace.Type != acl.ACE4_ACCESS_ALLOWED_ACE_TYPE {
			t.Errorf("ACE[%d].Type = %d, want ALLOW(%d)", i, ace.Type, acl.ACE4_ACCESS_ALLOWED_ACE_TYPE)
		}
		if ace.Flag != 0 {
			t.Errorf("ACE[%d].Flag = 0x%x, want 0 (Windows default is non-inheritable)", i, ace.Flag)
		}
		if ace.AccessMask != acl.FullAccessMask {
			t.Errorf("ACE[%d].AccessMask = 0x%x, want FullAccessMask 0x%x", i, ace.AccessMask, acl.FullAccessMask)
		}
	}

	// ACE order is fixed by SynthesizeWindowsDefault: owner first, SYSTEM second.
	// After SD round-trip, owner@ resolves through the SIDMapper to
	// "<uid>@localdomain"; SYSTEM (S-1-5-18) is not a domain SID so it
	// surfaces as "sid:S-1-5-18".
	if got, want := parsedACL.ACEs[0].Who, "1000@localdomain"; got != want {
		t.Errorf("ACE[0].Who = %q, want %q (owner round-trip)", got, want)
	}
	if got, want := parsedACL.ACEs[1].Who, "sid:S-1-5-18"; got != want {
		t.Errorf("ACE[1].Who = %q, want %q (SYSTEM SID round-trip)", got, want)
	}
}

// TestBuildSD_EmptyACL_EmitsZeroACEDACL verifies the FileAttr.ACL contract
// (pkg/metadata/file_types.go): file.ACL non-nil with len(ACEs)==0 is an
// explicit empty DACL (deny-all) and MUST NOT fall through to Windows-default
// synthesis. Round-trip emits a 0-ACE DACL.
func TestBuildSD_EmptyACL_EmitsZeroACEDACL(t *testing.T) {
	file := &metadata.File{
		FileAttr: metadata.FileAttr{
			UID:  1000,
			GID:  1000,
			Mode: 0o755,
			ACL:  &acl.ACL{ACEs: nil},
		},
	}

	data, err := BuildSecurityDescriptor(file, 0)
	if err != nil {
		t.Fatalf("BuildSecurityDescriptor: %v", err)
	}

	_, _, parsedACL, err := ParseSecurityDescriptor(data)
	if err != nil {
		t.Fatalf("ParseSecurityDescriptor: %v", err)
	}
	if parsedACL == nil {
		t.Fatal("Expected DACL, got nil")
	}
	if len(parsedACL.ACEs) != 0 {
		t.Errorf("Expected 0-ACE DACL (deny-all), got %d ACEs — synthesis must not run for non-nil empty ACL", len(parsedACL.ACEs))
	}
}

// TestBuildSD_ExplicitACL_UsesExisting verifies that an explicit ACL is encoded directly.
func TestBuildSD_ExplicitACL_UsesExisting(t *testing.T) {
	explicitACL := &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Flag:       0,
				AccessMask: acl.ACE4_READ_DATA,
				Who:        "OWNER@",
			},
		},
	}

	file := &metadata.File{
		FileAttr: metadata.FileAttr{
			UID:  500,
			GID:  20,
			Mode: 0o755,
			ACL:  explicitACL,
		},
	}

	data, err := BuildSecurityDescriptor(file, 0)
	if err != nil {
		t.Fatalf("BuildSecurityDescriptor: %v", err)
	}

	_, _, parsedACL, err := ParseSecurityDescriptor(data)
	if err != nil {
		t.Fatalf("ParseSecurityDescriptor: %v", err)
	}
	if parsedACL == nil {
		t.Fatal("Expected DACL, got nil")
	}

	// Should have exactly 1 ACE from the explicit ACL
	if len(parsedACL.ACEs) != 1 {
		t.Fatalf("Expected 1 ACE (explicit), got %d", len(parsedACL.ACEs))
	}
	if parsedACL.ACEs[0].AccessMask != acl.ACE4_READ_DATA {
		t.Errorf("ACE[0].AccessMask = 0x%x, want 0x%x", parsedACL.ACEs[0].AccessMask, acl.ACE4_READ_DATA)
	}
}

// TestBuildSD_SACL_EmptyStub verifies that requesting SACL produces a valid
// 8-byte empty SACL with SE_SACL_PRESENT set.
func TestBuildSD_SACL_EmptyStub(t *testing.T) {
	file := &metadata.File{
		FileAttr: metadata.FileAttr{
			UID:  1000,
			GID:  1000,
			Mode: 0o755,
		},
	}

	secInfo := uint32(OwnerSecurityInformation | GroupSecurityInformation | DACLSecurityInformation | SACLSecurityInformation)
	data, err := BuildSecurityDescriptor(file, secInfo)
	if err != nil {
		t.Fatalf("BuildSecurityDescriptor: %v", err)
	}

	if len(data) < sdHeaderSize {
		t.Fatalf("SD too short: %d", len(data))
	}

	// Check control flags
	control := binary.LittleEndian.Uint16(data[2:4])
	if control&seSACLPresent == 0 {
		t.Error("SE_SACL_PRESENT not set in control flags")
	}

	// Check SACL offset is non-zero
	saclOffset := binary.LittleEndian.Uint32(data[12:16])
	if saclOffset == 0 {
		t.Fatal("SACL offset is 0 when SACL was requested")
	}

	// Verify SACL data: 8 bytes (revision=2, sbz1=0, size=8, count=0, sbz2=0)
	if int(saclOffset)+8 > len(data) {
		t.Fatalf("SACL extends beyond SD data (offset=%d, len=%d)", saclOffset, len(data))
	}

	saclData := data[saclOffset:]
	if saclData[0] != 2 {
		t.Errorf("SACL revision = %d, want 2", saclData[0])
	}
	saclSize := binary.LittleEndian.Uint16(saclData[2:4])
	if saclSize != 8 {
		t.Errorf("SACL size = %d, want 8", saclSize)
	}
	aceCount := binary.LittleEndian.Uint16(saclData[4:6])
	if aceCount != 0 {
		t.Errorf("SACL ace count = %d, want 0", aceCount)
	}
}

// TestBuildSD_SACL_NotRequested verifies that SACL offset is 0 and
// SE_SACL_PRESENT is not set when SACL is not requested.
func TestBuildSD_SACL_NotRequested(t *testing.T) {
	file := &metadata.File{
		FileAttr: metadata.FileAttr{
			UID:  1000,
			GID:  1000,
			Mode: 0o755,
		},
	}

	// Request only owner, group, DACL (no SACL)
	secInfo := uint32(OwnerSecurityInformation | GroupSecurityInformation | DACLSecurityInformation)
	data, err := BuildSecurityDescriptor(file, secInfo)
	if err != nil {
		t.Fatalf("BuildSecurityDescriptor: %v", err)
	}

	control := binary.LittleEndian.Uint16(data[2:4])
	if control&seSACLPresent != 0 {
		t.Error("SE_SACL_PRESENT should NOT be set when SACL not requested")
	}

	saclOffset := binary.LittleEndian.Uint32(data[12:16])
	if saclOffset != 0 {
		t.Errorf("SACL offset = %d, want 0 when SACL not requested", saclOffset)
	}
}

// TestBuildSD_AutoInherited_PerACEFlagNotEnough verifies the P6-2 invariant:
// per MS-DTYP §2.4.6 / §2.4.4.2 the SD-level SE_DACL_AUTO_INHERITED Control
// bit and the per-ACE SEC_ACE_FLAG_INHERITED_ACE are independent fields, so
// per-ACE INHERITED_ACE flags alone do NOT imply the Control bit. (Inverse of
// the legacy fallback removed in P6-2; the bit must come from
// acl.ACL.AutoInherited, which itself reflects parse-side canonicalization.)
func TestBuildSD_AutoInherited_PerACEFlagNotEnough(t *testing.T) {
	file := &metadata.File{
		FileAttr: metadata.FileAttr{
			UID:  1000,
			GID:  1000,
			Mode: 0o755,
			ACL: &acl.ACL{
				AutoInherited: false,
				ACEs: []acl.ACE{
					{
						Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
						Flag:       acl.ACE4_INHERITED_ACE, // NFSv4 0x80
						AccessMask: 0x001F01FF,
						Who:        "OWNER@",
					},
				},
			},
		},
	}

	data, err := BuildSecurityDescriptor(file, 0)
	if err != nil {
		t.Fatalf("BuildSecurityDescriptor: %v", err)
	}

	control := binary.LittleEndian.Uint16(data[2:4])
	if control&seDACLAutoInherited != 0 {
		t.Error("SE_DACL_AUTO_INHERITED set from per-ACE INHERITED_ACE alone (legacy fallback should be removed)")
	}
}

// TestBuildSD_Protected verifies SE_DACL_PROTECTED is set when ACL.Protected is true.
func TestBuildSD_Protected(t *testing.T) {
	file := &metadata.File{
		FileAttr: metadata.FileAttr{
			UID:  1000,
			GID:  1000,
			Mode: 0o755,
			ACL: &acl.ACL{
				Protected: true,
				ACEs: []acl.ACE{
					{
						Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
						Flag:       0,
						AccessMask: 0x001F01FF,
						Who:        "OWNER@",
					},
				},
			},
		},
	}

	data, err := BuildSecurityDescriptor(file, 0)
	if err != nil {
		t.Fatalf("BuildSecurityDescriptor: %v", err)
	}

	control := binary.LittleEndian.Uint16(data[2:4])
	if control&seDACLProtected == 0 {
		t.Error("SE_DACL_PROTECTED not set when ACL.Protected is true")
	}
}

// TestBuildSD_FlagTranslation verifies ACE flags use explicit translation
// (INHERITED_ACE NFSv4 0x80 encodes as Windows 0x10 in wire format).
func TestBuildSD_FlagTranslation(t *testing.T) {
	file := &metadata.File{
		FileAttr: metadata.FileAttr{
			UID:  1000,
			GID:  1000,
			Mode: 0o755,
			ACL: &acl.ACL{
				ACEs: []acl.ACE{
					{
						Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
						Flag:       acl.ACE4_INHERITED_ACE, // NFSv4 0x80
						AccessMask: 0x001F01FF,
						Who:        "EVERYONE@",
					},
				},
			},
		},
	}

	data, err := BuildSecurityDescriptor(file, DACLSecurityInformation)
	if err != nil {
		t.Fatalf("BuildSecurityDescriptor: %v", err)
	}

	// Find the DACL offset
	daclOffset := binary.LittleEndian.Uint32(data[16:20])
	if daclOffset == 0 {
		t.Fatal("DACL offset is 0")
	}

	// Skip ACL header (8 bytes) to get to first ACE
	aceOffset := int(daclOffset) + aclHeaderSize
	if aceOffset+2 > len(data) {
		t.Fatal("ACE extends beyond data")
	}

	// ACE flags byte is at offset+1
	wireFlags := data[aceOffset+1]
	if wireFlags != 0x10 {
		t.Errorf("Wire ACE flags = 0x%02x, want 0x10 (INHERITED_ACE)", wireFlags)
	}

	// Round-trip: parse back and verify NFSv4 flag is 0x80
	_, _, parsedACL, err := ParseSecurityDescriptor(data)
	if err != nil {
		t.Fatalf("ParseSecurityDescriptor: %v", err)
	}
	if parsedACL == nil || len(parsedACL.ACEs) != 1 {
		t.Fatalf("Expected 1 ACE, got %v", parsedACL)
	}
	if parsedACL.ACEs[0].Flag != acl.ACE4_INHERITED_ACE {
		t.Errorf("Parsed ACE flag = 0x%x, want 0x%x (INHERITED_ACE)", parsedACL.ACEs[0].Flag, acl.ACE4_INHERITED_ACE)
	}
}

// TestParseSD_RoundTrip verifies build+parse produces matching owner/group/ACL.
func TestParseSD_RoundTrip(t *testing.T) {
	file := &metadata.File{
		FileAttr: metadata.FileAttr{
			UID:  500,
			GID:  20,
			Mode: 0o750,
			ACL: &acl.ACL{
				ACEs: []acl.ACE{
					{
						Type:       acl.ACE4_ACCESS_DENIED_ACE_TYPE,
						Flag:       0,
						AccessMask: acl.ACE4_WRITE_DATA,
						Who:        "EVERYONE@",
					},
					{
						Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
						Flag:       acl.ACE4_FILE_INHERIT_ACE | acl.ACE4_DIRECTORY_INHERIT_ACE,
						AccessMask: 0x001F01FF,
						Who:        "OWNER@",
					},
				},
			},
		},
	}

	data, err := BuildSecurityDescriptor(file, 0)
	if err != nil {
		t.Fatalf("BuildSecurityDescriptor: %v", err)
	}

	ownerUID, ownerGID, parsedACL, err := ParseSecurityDescriptor(data)
	if err != nil {
		t.Fatalf("ParseSecurityDescriptor: %v", err)
	}

	if ownerUID == nil || *ownerUID != 500 {
		t.Errorf("Owner UID = %v, want 500", ownerUID)
	}
	if ownerGID == nil || *ownerGID != 20 {
		t.Errorf("Owner GID = %v, want 20", ownerGID)
	}

	if parsedACL == nil || len(parsedACL.ACEs) != 2 {
		t.Fatalf("Expected 2 ACEs, got %v", parsedACL)
	}

	ace0 := parsedACL.ACEs[0]
	if ace0.Type != acl.ACE4_ACCESS_DENIED_ACE_TYPE {
		t.Errorf("ACE[0].Type = %d, want DENY", ace0.Type)
	}
	if ace0.Who != "EVERYONE@" {
		t.Errorf("ACE[0].Who = %q, want EVERYONE@", ace0.Who)
	}

	ace1 := parsedACL.ACEs[1]
	if ace1.Type != acl.ACE4_ACCESS_ALLOWED_ACE_TYPE {
		t.Errorf("ACE[1].Type = %d, want ALLOW", ace1.Type)
	}
	if ace1.Flag&acl.ACE4_FILE_INHERIT_ACE == 0 {
		t.Error("ACE[1] missing FILE_INHERIT flag")
	}
	if ace1.Flag&acl.ACE4_DIRECTORY_INHERIT_ACE == 0 {
		t.Error("ACE[1] missing DIRECTORY_INHERIT flag")
	}
}

// TestBuildSD_FieldOrder verifies SACL appears before DACL in binary output.
func TestBuildSD_FieldOrder(t *testing.T) {
	file := &metadata.File{
		FileAttr: metadata.FileAttr{
			UID:  1000,
			GID:  1000,
			Mode: 0o755,
			ACL: &acl.ACL{
				ACEs: []acl.ACE{
					{
						Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
						Flag:       0,
						AccessMask: 0x001F01FF,
						Who:        "OWNER@",
					},
				},
			},
		},
	}

	secInfo := uint32(OwnerSecurityInformation | GroupSecurityInformation | DACLSecurityInformation | SACLSecurityInformation)
	data, err := BuildSecurityDescriptor(file, secInfo)
	if err != nil {
		t.Fatalf("BuildSecurityDescriptor: %v", err)
	}

	saclOffset := binary.LittleEndian.Uint32(data[12:16])
	daclOffset := binary.LittleEndian.Uint32(data[16:20])
	ownerOffset := binary.LittleEndian.Uint32(data[4:8])
	groupOffset := binary.LittleEndian.Uint32(data[8:12])

	// Verify order: SACL < DACL < Owner < Group
	if saclOffset == 0 || daclOffset == 0 || ownerOffset == 0 || groupOffset == 0 {
		t.Fatalf("All offsets should be non-zero: sacl=%d, dacl=%d, owner=%d, group=%d",
			saclOffset, daclOffset, ownerOffset, groupOffset)
	}

	if saclOffset >= daclOffset {
		t.Errorf("SACL offset (%d) should be before DACL offset (%d)", saclOffset, daclOffset)
	}
	if daclOffset >= ownerOffset {
		t.Errorf("DACL offset (%d) should be before Owner offset (%d)", daclOffset, ownerOffset)
	}
	if ownerOffset >= groupOffset {
		t.Errorf("Owner offset (%d) should be before Group offset (%d)", ownerOffset, groupOffset)
	}
}

func TestBuildSecurityDescriptorDACLOnly(t *testing.T) {
	file := &metadata.File{
		FileAttr: metadata.FileAttr{
			UID:  1000,
			GID:  1000,
			Mode: 0o644,
		},
	}

	// Request only DACL
	data, err := BuildSecurityDescriptor(file, DACLSecurityInformation)
	if err != nil {
		t.Fatalf("BuildSecurityDescriptor: %v", err)
	}

	// Parse it back
	ownerUID, ownerGID, parsedACL, err := ParseSecurityDescriptor(data)
	if err != nil {
		t.Fatalf("ParseSecurityDescriptor: %v", err)
	}

	// Owner and Group should NOT be present
	if ownerUID != nil {
		t.Errorf("Owner UID should be nil when only DACL requested, got %d", *ownerUID)
	}
	if ownerGID != nil {
		t.Errorf("Owner GID should be nil when only DACL requested, got %d", *ownerGID)
	}

	// DACL should still be present
	if parsedACL == nil {
		t.Fatal("Expected DACL to be present")
	}
}

func TestFourByteAlignment(t *testing.T) {
	// Build SD and verify all offsets are 4-byte aligned
	file := &metadata.File{
		FileAttr: metadata.FileAttr{
			UID:  1000,
			GID:  1000,
			Mode: 0o644,
			ACL: &acl.ACL{
				ACEs: []acl.ACE{
					{Type: acl.ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: 0x001F01FF, Who: "OWNER@"},
				},
			},
		},
	}

	data, err := BuildSecurityDescriptor(file, 0)
	if err != nil {
		t.Fatalf("BuildSecurityDescriptor: %v", err)
	}

	if len(data) < sdHeaderSize {
		t.Fatalf("SD too short: %d", len(data))
	}

	// Check offsets from header
	tests := []struct {
		name   string
		offset int
	}{
		{"OffsetOwner", 4},
		{"OffsetGroup", 8},
		{"OffsetSacl", 12},
		{"OffsetDacl", 16},
	}

	for _, tt := range tests {
		offsetVal := binary.LittleEndian.Uint32(data[tt.offset : tt.offset+4])
		if offsetVal > 0 && offsetVal%4 != 0 {
			t.Errorf("%s = %d, not 4-byte aligned", tt.name, offsetVal)
		}
	}
}

func TestSIDMapperIntegration(t *testing.T) {
	// Verify that SetSIDMapper/GetSIDMapper work
	original := GetSIDMapper()
	defer SetSIDMapper(original) // Restore after test

	m := sid.NewSIDMapper(111, 222, 333)
	SetSIDMapper(m)

	if GetSIDMapper() != m {
		t.Error("GetSIDMapper should return the mapper set by SetSIDMapper")
	}

	// Setting nil should keep the current mapper
	SetSIDMapper(nil)
	if GetSIDMapper() != m {
		t.Error("SetSIDMapper(nil) should not change the mapper")
	}
}

func TestSDRoundTripWithMapper(t *testing.T) {
	// Test that build+parse produces consistent UIDs/GIDs with the mapper
	file := &metadata.File{
		FileAttr: metadata.FileAttr{
			UID:  42,
			GID:  100,
			Mode: 0o755,
			ACL: &acl.ACL{
				ACEs: []acl.ACE{
					{
						Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
						Flag:       0,
						AccessMask: 0x001F01FF,
						Who:        "OWNER@",
					},
				},
			},
		},
	}

	data, err := BuildSecurityDescriptor(file, 0)
	if err != nil {
		t.Fatalf("BuildSecurityDescriptor: %v", err)
	}

	ownerUID, ownerGID, _, err := ParseSecurityDescriptor(data)
	if err != nil {
		t.Fatalf("ParseSecurityDescriptor: %v", err)
	}

	if ownerUID == nil || *ownerUID != 42 {
		t.Errorf("Round-trip owner UID = %v, want 42", ownerUID)
	}
	if ownerGID == nil || *ownerGID != 100 {
		t.Errorf("Round-trip owner GID = %v, want 100", ownerGID)
	}
}

// TestBuildSD_SpecialSIDs verifies that SYSTEM@ and ADMINISTRATORS@ principals
// are mapped to their correct well-known SIDs.
func TestBuildSD_SpecialSIDs(t *testing.T) {
	file := &metadata.File{
		FileAttr: metadata.FileAttr{
			UID:  1000,
			GID:  1000,
			Mode: 0o755,
			ACL: &acl.ACL{
				ACEs: []acl.ACE{
					{
						Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
						Flag:       0,
						AccessMask: acl.FullAccessMask,
						Who:        acl.SpecialSystem,
					},
					{
						Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
						Flag:       0,
						AccessMask: acl.FullAccessMask,
						Who:        acl.SpecialAdministrators,
					},
				},
			},
		},
	}

	data, err := BuildSecurityDescriptor(file, DACLSecurityInformation)
	if err != nil {
		t.Fatalf("BuildSecurityDescriptor: %v", err)
	}

	// Find the DACL
	daclOffset := binary.LittleEndian.Uint32(data[16:20])
	if daclOffset == 0 {
		t.Fatal("DACL offset is 0")
	}

	// Parse the ACL header to get ACE count
	aceCount := binary.LittleEndian.Uint16(data[daclOffset+4 : daclOffset+6])
	if aceCount != 2 {
		t.Fatalf("Expected 2 ACEs, got %d", aceCount)
	}

	// Parse first ACE's SID (should be S-1-5-18 for SYSTEM)
	aceStart := int(daclOffset) + aclHeaderSize
	aceSID1, _, err := sid.DecodeSID(data[aceStart+aceHeaderSize:])
	if err != nil {
		t.Fatalf("Failed to decode first ACE SID: %v", err)
	}
	if !aceSID1.Equal(sid.WellKnownSystem) {
		t.Errorf("First ACE SID = %s, want S-1-5-18 (SYSTEM)", sid.FormatSID(aceSID1))
	}

	// Parse second ACE's SID (should be S-1-5-32-544 for Administrators)
	ace1Size := binary.LittleEndian.Uint16(data[aceStart+2 : aceStart+4])
	ace2Start := aceStart + int(ace1Size)
	aceSID2, _, err := sid.DecodeSID(data[ace2Start+aceHeaderSize:])
	if err != nil {
		t.Fatalf("Failed to decode second ACE SID: %v", err)
	}
	if !aceSID2.Equal(sid.WellKnownAdministrators) {
		t.Errorf("Second ACE SID = %s, want S-1-5-32-544 (Administrators)", sid.FormatSID(aceSID2))
	}
}

// TestParseSecurityDescriptor_PreservesSIDForm asserts that an SD ACE keyed
// to a well-known SID (S-1-5-32-544 / "ADMINISTRATORS@") for which the parse
// path has no POSIX mapping is preserved as ACE.Who="sid:S-1-5-32-544". This
// is the round-trip side of P2-3 — non-mappable SIDs must reach the ACL
// evaluator with the "sid:" prefix so SID-aware matching works.
func TestParseSecurityDescriptor_PreservesSIDForm(t *testing.T) {
	file := &metadata.File{
		FileAttr: metadata.FileAttr{
			UID:  1000,
			GID:  1000,
			Mode: 0o755,
			ACL: &acl.ACL{
				ACEs: []acl.ACE{
					{
						Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
						Flag:       0,
						AccessMask: 0x001F01FF,
						Who:        acl.SpecialAdministrators,
					},
				},
			},
		},
	}

	data, err := BuildSecurityDescriptor(file, 0)
	if err != nil {
		t.Fatalf("BuildSecurityDescriptor: %v", err)
	}
	_, _, parsedACL, err := ParseSecurityDescriptor(data)
	if err != nil {
		t.Fatalf("ParseSecurityDescriptor: %v", err)
	}
	if parsedACL == nil || len(parsedACL.ACEs) != 1 {
		t.Fatalf("expected 1 ACE, got %#v", parsedACL)
	}
	got := parsedACL.ACEs[0].Who
	want := "sid:S-1-5-32-544"
	if got != want {
		t.Errorf("ACE.Who = %q, want %q", got, want)
	}
}

// TestSecurityDescriptor_SIDFormRoundTrip asserts that a non-POSIX-mappable SID
// preserved as ACE.Who="sid:..." after parse can be re-encoded by
// BuildSecurityDescriptor without drift. Regression for the round-trip break
// flagged on PR #506: PrincipalToSID must honor the "sid:" prefix.
func TestSecurityDescriptor_SIDFormRoundTrip(t *testing.T) {
	file := &metadata.File{
		FileAttr: metadata.FileAttr{
			UID:  1000,
			GID:  1000,
			Mode: 0o755,
			ACL: &acl.ACL{
				ACEs: []acl.ACE{
					{
						Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
						Flag:       0,
						AccessMask: 0x001F01FF,
						Who:        acl.SpecialAdministrators,
					},
				},
			},
		},
	}

	data1, err := BuildSecurityDescriptor(file, 0)
	if err != nil {
		t.Fatalf("BuildSecurityDescriptor pass 1: %v", err)
	}
	_, _, parsed1, err := ParseSecurityDescriptor(data1)
	if err != nil {
		t.Fatalf("ParseSecurityDescriptor pass 1: %v", err)
	}
	if parsed1 == nil || len(parsed1.ACEs) != 1 {
		t.Fatalf("expected 1 ACE after parse 1, got %#v", parsed1)
	}
	if got := parsed1.ACEs[0].Who; got != "sid:S-1-5-32-544" {
		t.Fatalf("parse 1 Who = %q, want %q", got, "sid:S-1-5-32-544")
	}

	// Re-encode using the parsed ACL (Who = "sid:S-1-5-32-544"). This is the
	// path Copilot flagged: PrincipalToSID must strip the prefix and produce
	// the same SID, not a hash-based domain SID.
	file2 := &metadata.File{
		FileAttr: metadata.FileAttr{
			UID:  1000,
			GID:  1000,
			Mode: 0o755,
			ACL:  parsed1,
		},
	}
	data2, err := BuildSecurityDescriptor(file2, 0)
	if err != nil {
		t.Fatalf("BuildSecurityDescriptor pass 2: %v", err)
	}
	_, _, parsed2, err := ParseSecurityDescriptor(data2)
	if err != nil {
		t.Fatalf("ParseSecurityDescriptor pass 2: %v", err)
	}
	if got := parsed2.ACEs[0].Who; got != "sid:S-1-5-32-544" {
		t.Errorf("round-trip Who = %q, want %q (PrincipalToSID likely missing sid: handling)", got, "sid:S-1-5-32-544")
	}
}

// TestParseSecurityDescriptor_CapturesControlFlags asserts that an SD whose
// Control field carries SE_DACL_PROTECTED + (SE_DACL_AUTO_INHERITED &
// SE_DACL_AUTO_INHERIT_REQ) yields an ACL with both flags set after parse.
// Per MS-DTYP §2.5.3.4.2 / Samba canonicalize_inheritance_bits, AutoInherited
// is only captured when BOTH AUTO_INHERITED and AUTO_INHERIT_REQ are set by
// the client.
func TestParseSecurityDescriptor_CapturesControlFlags(t *testing.T) {
	// Hand-craft a self-relative SD with Control =
	// seSelfRelative|seDACLPresent|seDACLProtected|seDACLAutoInherited|seDACLAutoInheritReq.
	// Build path doesn't emit AUTO_INHERIT_REQ, so we cannot use it here.
	const (
		hdrSize = 20
		aclSize = 8
	)
	buf := make([]byte, hdrSize+aclSize)
	buf[0] = 1 // Revision
	buf[1] = 0 // Sbz1
	binary.LittleEndian.PutUint16(buf[2:4], uint16(seSelfRelative|seDACLPresent|seDACLProtected|seDACLAutoInherited|seDACLAutoInheritReq))
	binary.LittleEndian.PutUint32(buf[4:8], 0)         // offsetOwner
	binary.LittleEndian.PutUint32(buf[8:12], 0)        // offsetGroup
	binary.LittleEndian.PutUint32(buf[12:16], 0)       // offsetSACL
	binary.LittleEndian.PutUint32(buf[16:20], hdrSize) // offsetDACL
	// Empty DACL
	buf[20] = 2 // AclRevision
	buf[21] = 0
	binary.LittleEndian.PutUint16(buf[22:24], aclSize)
	binary.LittleEndian.PutUint16(buf[24:26], 0)
	binary.LittleEndian.PutUint16(buf[26:28], 0)

	_, _, parsed, err := ParseSecurityDescriptor(buf)
	if err != nil {
		t.Fatalf("ParseSecurityDescriptor: %v", err)
	}
	if parsed == nil {
		t.Fatal("parsed ACL is nil")
	}
	if !parsed.Protected {
		t.Error("Protected = false, want true")
	}
	if !parsed.AutoInherited {
		t.Error("AutoInherited = false, want true (both AUTO_INHERITED and AUTO_INHERIT_REQ set)")
	}
}

// TestParseSecurityDescriptor_NoControlFlagsLeavesACLDefault asserts that an SD
// whose Control field carries only seSelfRelative|seDACLPresent (no Protected
// or AutoInherited bits) yields an ACL with both flags at their zero default.
func TestParseSecurityDescriptor_NoControlFlagsLeavesACLDefault(t *testing.T) {
	// Hand-craft a self-relative SD: header (20B) + empty DACL (8B, ACE count=0).
	// Control = seSelfRelative | seDACLPresent only.
	const (
		hdrSize = 20
		aclSize = 8
	)
	buf := make([]byte, hdrSize+aclSize)
	// Revision=1, Sbz1=0
	buf[0] = 1
	buf[1] = 0
	// Control (LE)
	binary.LittleEndian.PutUint16(buf[2:4], uint16(seSelfRelative|seDACLPresent))
	// offsetOwner, offsetGroup, offsetSACL = 0
	binary.LittleEndian.PutUint32(buf[4:8], 0)
	binary.LittleEndian.PutUint32(buf[8:12], 0)
	binary.LittleEndian.PutUint32(buf[12:16], 0)
	// offsetDACL = 20 (right after header)
	binary.LittleEndian.PutUint32(buf[16:20], hdrSize)
	// DACL: AclRevision=2, Sbz1=0, AclSize=8, AceCount=0, Sbz2=0
	buf[20] = 2
	buf[21] = 0
	binary.LittleEndian.PutUint16(buf[22:24], aclSize)
	binary.LittleEndian.PutUint16(buf[24:26], 0)
	binary.LittleEndian.PutUint16(buf[26:28], 0)

	_, _, parsed, err := ParseSecurityDescriptor(buf)
	if err != nil {
		t.Fatalf("ParseSecurityDescriptor: %v", err)
	}
	if parsed == nil {
		t.Fatal("parsed ACL is nil")
	}
	if parsed.Protected {
		t.Error("Protected = true, want false (no SE_DACL_PROTECTED bit in Control)")
	}
	if parsed.AutoInherited {
		t.Error("AutoInherited = true, want false (no SE_DACL_AUTO_INHERITED bit in Control)")
	}
}

// TestSecurityDescriptor_OwnerRightsRoundTrip asserts that an ACL keyed
// to OWNER_RIGHTS (S-1-3-4) survives a BuildSecurityDescriptor →
// ParseSecurityDescriptor cycle as ACE.Who = OwnerRights@.
func TestSecurityDescriptor_OwnerRightsRoundTrip(t *testing.T) {
	file := &metadata.File{
		FileAttr: metadata.FileAttr{
			UID:  1000,
			GID:  1000,
			Mode: 0o755,
			ACL: &acl.ACL{
				ACEs: []acl.ACE{
					{
						Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
						AccessMask: acl.ACE4_READ_DATA | acl.ACE4_READ_ATTRIBUTES,
						Who:        acl.SpecialOwnerRights,
					},
				},
			},
		},
	}
	data, err := BuildSecurityDescriptor(file, 0)
	if err != nil {
		t.Fatalf("BuildSecurityDescriptor: %v", err)
	}
	_, _, parsed, err := ParseSecurityDescriptor(data)
	if err != nil {
		t.Fatalf("ParseSecurityDescriptor: %v", err)
	}
	if parsed == nil || len(parsed.ACEs) != 1 {
		t.Fatalf("expected 1 ACE, got %#v", parsed)
	}
	if got := parsed.ACEs[0].Who; got != acl.SpecialOwnerRights {
		t.Errorf("ACE.Who = %q, want %q", got, acl.SpecialOwnerRights)
	}
}

// TestBuildSD_AutoInherited_FromACLField verifies that BuildSecurityDescriptor
// emits SE_DACL_AUTO_INHERITED when acl.ACL.AutoInherited is true, even if no
// ACE carries the per-ACE INHERITED_ACE flag. This locks down the symmetric
// round-trip: parse captures the Control bit into the ACL field; build emits
// the Control bit directly from that field (MS-DTYP §2.4.6).
// Regression for the asymmetric build path P6 fixes.
func TestBuildSD_AutoInherited_FromACLField(t *testing.T) {
	file := &metadata.File{
		FileAttr: metadata.FileAttr{
			UID:  1000,
			GID:  1000,
			Mode: 0o755,
			ACL: &acl.ACL{
				AutoInherited: true,
				ACEs: []acl.ACE{
					{
						Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
						Flag:       0, // explicitly NOT INHERITED_ACE
						AccessMask: 0x001F01FF,
						Who:        acl.SpecialOwner,
					},
				},
			},
		},
	}

	data, err := BuildSecurityDescriptor(file, 0)
	if err != nil {
		t.Fatalf("BuildSecurityDescriptor: %v", err)
	}

	control := binary.LittleEndian.Uint16(data[2:4])
	if control&seDACLAutoInherited == 0 {
		t.Error("SE_DACL_AUTO_INHERITED not set when ACL.AutoInherited=true")
	}
}

// TestBuildSD_AutoInherited_RoundTrip verifies the full client→parse→build
// round trip for SE_DACL_AUTO_INHERITED. A client SET_INFO carrying both
// AUTO_INHERITED and AUTO_INHERIT_REQ is parsed (canonicalization captures the
// bit), the ACL is then re-built via BuildSecurityDescriptor, and the re-built
// SD must still carry SE_DACL_AUTO_INHERITED. AUTO_INHERIT_REQ is a one-way
// client request — server never echoes it back, mirroring Samba
// canonicalize_inheritance_bits.
func TestBuildSD_AutoInherited_RoundTrip(t *testing.T) {
	// Hand-craft the inbound client SD with both AUTO_INHERITED and
	// AUTO_INHERIT_REQ in the Control word. Build path won't emit
	// AUTO_INHERIT_REQ on its own. Include a non-inherited ACE so the
	// build-side `len(ACEs) > 0` guard preserves the parsed ACL (zero-ACE
	// DACLs are intentionally replaced by mode synthesis).
	clientSD := makeAutoInheritSD(t, seDACLAutoInherited|seDACLAutoInheritReq, false)

	_, _, parsed, err := ParseSecurityDescriptor(clientSD)
	if err != nil {
		t.Fatalf("ParseSecurityDescriptor: %v", err)
	}
	if parsed == nil {
		t.Fatal("parsed ACL is nil")
	}
	if !parsed.AutoInherited {
		t.Fatal("parsed.AutoInherited = false; canonicalization failed to capture flag")
	}

	// Re-build from the parsed ACL and assert the Control bit survives.
	file2 := &metadata.File{
		FileAttr: metadata.FileAttr{
			UID:  1000,
			GID:  1000,
			Mode: 0o755,
			ACL:  parsed,
		},
	}
	data2, err := BuildSecurityDescriptor(file2, 0)
	if err != nil {
		t.Fatalf("BuildSecurityDescriptor (rebuild): %v", err)
	}

	control := binary.LittleEndian.Uint16(data2[2:4])
	if control&seDACLAutoInherited == 0 {
		t.Error("SE_DACL_AUTO_INHERITED dropped on re-build from parsed ACL")
	}
	// Per Samba canonicalize_inheritance_bits, AUTO_INHERIT_REQ is never echoed back.
	if control&seDACLAutoInheritReq != 0 {
		t.Error("SE_DACL_AUTO_INHERIT_REQ was echoed back in build output (must not be)")
	}
}

// TestBuildSD_AutoInherited_NotSet verifies that BuildSecurityDescriptor does
// NOT emit SE_DACL_AUTO_INHERITED when neither acl.ACL.AutoInherited is true
// nor any ACE carries the INHERITED_ACE flag (negative case for P6 fix).
func TestBuildSD_AutoInherited_NotSet(t *testing.T) {
	file := &metadata.File{
		FileAttr: metadata.FileAttr{
			UID:  1000,
			GID:  1000,
			Mode: 0o755,
			ACL: &acl.ACL{
				AutoInherited: false,
				ACEs: []acl.ACE{
					{
						Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
						Flag:       0,
						AccessMask: 0x001F01FF,
						Who:        acl.SpecialOwner,
					},
				},
			},
		},
	}

	data, err := BuildSecurityDescriptor(file, 0)
	if err != nil {
		t.Fatalf("BuildSecurityDescriptor: %v", err)
	}

	control := binary.LittleEndian.Uint16(data[2:4])
	if control&seDACLAutoInherited != 0 {
		t.Error("SE_DACL_AUTO_INHERITED set when ACL.AutoInherited=false and no ACE carries INHERITED_ACE")
	}
}

// makeAutoInheritSD hand-crafts a self-relative SD whose Control word has the
// given high bits OR'd in (in addition to seSelfRelative|seDACLPresent), and
// embeds a single ALLOW-EVERYONE ACE optionally marked with the wire
// INHERITED_ACE flag so the inheritance matrix can assert independence from
// per-ACE flags.
func makeAutoInheritSD(t *testing.T, extraControl uint16, perACEInherited bool) []byte {
	t.Helper()
	// Layout: header(20) + DACL header(8) + ACE(8 + everyone SID).
	everyoneSID := sid.WellKnownEveryone
	sidLen := sid.SIDSize(everyoneSID)
	aceLen := aceHeaderSize + sidLen
	daclLen := aclHeaderSize + aceLen
	totalLen := sdHeaderSize + daclLen
	buf := make([]byte, totalLen)
	// SD header
	buf[0] = 1 // Revision
	binary.LittleEndian.PutUint16(buf[2:4], uint16(seSelfRelative|seDACLPresent)|extraControl)
	binary.LittleEndian.PutUint32(buf[16:20], sdHeaderSize)
	// DACL header
	buf[20] = 2
	binary.LittleEndian.PutUint16(buf[22:24], uint16(daclLen))
	binary.LittleEndian.PutUint16(buf[24:26], 1) // AceCount=1
	// ACE
	aceOff := sdHeaderSize + aclHeaderSize
	buf[aceOff] = accessAllowedACEType
	if perACEInherited {
		buf[aceOff+1] = 0x10 // Windows INHERITED_ACE wire bit
	}
	binary.LittleEndian.PutUint16(buf[aceOff+2:aceOff+4], uint16(aceLen))
	binary.LittleEndian.PutUint32(buf[aceOff+4:aceOff+8], 0x001F01FF)
	var sidBuf bytes.Buffer
	sid.EncodeSID(&sidBuf, everyoneSID)
	copy(buf[aceOff+aceHeaderSize:], sidBuf.Bytes())
	return buf
}

// TestParseSD_AutoInheritedCanonicalization is the 4-bit matrix mirroring
// smbtorture's acls.INHERITFLAGS / acls_non_canonical.flags expectations.
// Per Samba canonicalize_inheritance_bits (MS-DTYP §2.5.3.4.2), the
// server captures SE_DACL_AUTO_INHERITED on the persisted ACL ONLY when the
// client sets both AUTO_INHERITED and AUTO_INHERIT_REQ. The per-ACE
// SEC_ACE_FLAG_INHERITED_ACE is independent and never escalates the SD bit.
func TestParseSD_AutoInheritedCanonicalization(t *testing.T) {
	cases := []struct {
		name                 string
		autoInherited        bool
		autoInheritReq       bool
		protected            bool
		perACEInherited      bool
		wantACLAutoInherit   bool
		wantACLProtected     bool
		wantBuildAutoInherit bool
	}{
		{"none", false, false, false, false, false, false, false},
		{"auto_only", true, false, false, false, false, false, false},
		{"req_only", false, true, false, false, false, false, false},
		{"auto_and_req", true, true, false, false, true, false, true},
		{"protected_only", false, false, true, false, false, true, false},
		{"all_no_perace", true, true, true, false, true, true, true},
		{"perACE_inherited_only", false, false, false, true, false, false, false},
		{"perACE_with_protected", false, false, true, true, false, true, false},
		{"auto_and_req_with_perACE", true, true, false, true, true, false, true},
		{"auto_and_protected_no_req", true, false, true, false, false, true, false},
		{"all_four_bits", true, true, true, true, true, true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ctrl uint16
			if tc.autoInherited {
				ctrl |= seDACLAutoInherited
			}
			if tc.autoInheritReq {
				ctrl |= seDACLAutoInheritReq
			}
			if tc.protected {
				ctrl |= seDACLProtected
			}
			sd := makeAutoInheritSD(t, ctrl, tc.perACEInherited)

			_, _, parsed, err := ParseSecurityDescriptor(sd)
			if err != nil {
				t.Fatalf("ParseSecurityDescriptor: %v", err)
			}
			if parsed == nil {
				t.Fatal("parsed ACL is nil")
			}
			if parsed.AutoInherited != tc.wantACLAutoInherit {
				t.Errorf("parsed.AutoInherited = %v, want %v", parsed.AutoInherited, tc.wantACLAutoInherit)
			}
			if parsed.Protected != tc.wantACLProtected {
				t.Errorf("parsed.Protected = %v, want %v", parsed.Protected, tc.wantACLProtected)
			}

			// Re-build and verify Control bits round-trip correctly.
			file := &metadata.File{
				FileAttr: metadata.FileAttr{
					UID:  1000,
					GID:  1000,
					Mode: 0o755,
					ACL:  parsed,
				},
			}
			rebuilt, err := BuildSecurityDescriptor(file, 0)
			if err != nil {
				t.Fatalf("BuildSecurityDescriptor: %v", err)
			}
			rebuiltCtrl := binary.LittleEndian.Uint16(rebuilt[2:4])
			gotAuto := rebuiltCtrl&seDACLAutoInherited != 0
			if gotAuto != tc.wantBuildAutoInherit {
				t.Errorf("re-build SE_DACL_AUTO_INHERITED = %v, want %v", gotAuto, tc.wantBuildAutoInherit)
			}
			gotProtected := rebuiltCtrl&seDACLProtected != 0
			if gotProtected != tc.wantACLProtected {
				t.Errorf("re-build SE_DACL_PROTECTED = %v, want %v", gotProtected, tc.wantACLProtected)
			}
			// AUTO_INHERIT_REQ must never be echoed back on build.
			if rebuiltCtrl&seDACLAutoInheritReq != 0 {
				t.Error("re-build echoed SE_DACL_AUTO_INHERIT_REQ (must not)")
			}
		})
	}
}

// TestParseSD_NoCanonicalization_PreservesAutoInherited covers the Samba
// extension path (`acl flag inherited canonicalization = no`) where
// SE_DACL_AUTO_INHERITED is preserved verbatim from the inbound Control word
// with no AUTO_INHERIT_REQ gate. The distinguishing case is
// `autoinherited_alone`: AUTO_INHERITED set, AUTO_INHERIT_REQ unset — the
// canonicalizing default would drop it, this opt-out keeps it.
func TestParseSD_NoCanonicalization_PreservesAutoInherited(t *testing.T) {
	cases := []struct {
		name               string
		autoInherited      bool
		autoInheritReq     bool
		wantACLAutoInherit bool
	}{
		{"autoinherited_alone", true, false, true},
		{"auto_and_req", true, true, true},
		{"req_alone", false, true, false},
		{"neither", false, false, false},
	}

	opts := ParseSDOptions{CanonicalizeAutoInherited: false}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ctrl uint16
			if tc.autoInherited {
				ctrl |= seDACLAutoInherited
			}
			if tc.autoInheritReq {
				ctrl |= seDACLAutoInheritReq
			}
			sd := makeAutoInheritSD(t, ctrl, false)

			_, _, parsed, err := ParseSecurityDescriptorWithOptions(sd, opts)
			if err != nil {
				t.Fatalf("ParseSecurityDescriptorWithOptions: %v", err)
			}
			if parsed == nil {
				t.Fatal("parsed ACL is nil")
			}
			if parsed.AutoInherited != tc.wantACLAutoInherit {
				t.Errorf("parsed.AutoInherited = %v, want %v (opts.CanonicalizeAutoInherited=false)",
					parsed.AutoInherited, tc.wantACLAutoInherit)
			}
		})
	}
}

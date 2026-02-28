package handlers

import (
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

// TestBuildSD_NilACL_SynthesizesDACL verifies that BuildSecurityDescriptor
// synthesizes a proper POSIX-derived DACL when the file has no explicit ACL.
func TestBuildSD_NilACL_SynthesizesDACL(t *testing.T) {
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

	// Mode 0755 should produce Allow-only ACEs (Samba-style, no Deny ACEs).
	// SynthesizeFromMode produces:
	// 1. ALLOW OWNER@ (rwx + admin)
	// 2. ALLOW GROUP@ (r-x)
	// 3. ALLOW EVERYONE@ (r-x)
	// 4. ALLOW SYSTEM@ (full)
	// 5. ALLOW ADMINISTRATORS@ (full)
	// Total: 5 Allow ACEs, 0 Deny ACEs.
	if len(parsedACL.ACEs) != 5 {
		t.Fatalf("Expected 5 ACEs from synthesis, got %d", len(parsedACL.ACEs))
	}

	// Samba-style: NO Deny ACEs should be present
	for _, ace := range parsedACL.ACEs {
		if ace.Type == acl.ACE4_ACCESS_DENIED_ACE_TYPE {
			t.Errorf("Unexpected DENY ACE in synthesized DACL (Samba-style should be Allow-only): %s", ace.Who)
		}
	}

	// Should NOT be a simple "Everyone: Full Access" single ACE
	if len(parsedACL.ACEs) == 1 && parsedACL.ACEs[0].AccessMask == 0x001F01FF {
		t.Error("Got Everyone:Full single ACE -- expected POSIX-derived DACL with per-principal Allow ACEs")
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

// TestBuildSD_AutoInherited verifies SE_DACL_AUTO_INHERITED is set when
// ACEs have INHERITED_ACE flag.
func TestBuildSD_AutoInherited(t *testing.T) {
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
	if control&seDACLAutoInherited == 0 {
		t.Error("SE_DACL_AUTO_INHERITED not set when ACEs have INHERITED_ACE flag")
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

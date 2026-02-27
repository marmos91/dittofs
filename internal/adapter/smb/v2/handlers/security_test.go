package handlers

import (
	"encoding/binary"
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
	m.Run()
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
}

func TestBuildSecurityDescriptorNilACL(t *testing.T) {
	file := &metadata.File{
		FileAttr: metadata.FileAttr{
			UID:  0,
			GID:  0,
			Mode: 0o777,
		},
	}

	data, err := BuildSecurityDescriptor(file, 0)
	if err != nil {
		t.Fatalf("BuildSecurityDescriptor: %v", err)
	}

	// Should contain a minimal DACL granting Everyone full access
	_, _, parsedACL, err := ParseSecurityDescriptor(data)
	if err != nil {
		t.Fatalf("ParseSecurityDescriptor: %v", err)
	}
	if parsedACL == nil {
		t.Fatal("Expected DACL, got nil")
	}
	if len(parsedACL.ACEs) != 1 {
		t.Fatalf("Expected 1 ACE (Everyone full access), got %d", len(parsedACL.ACEs))
	}
	if parsedACL.ACEs[0].Who != "EVERYONE@" {
		t.Errorf("ACE[0].Who = %q, want EVERYONE@", parsedACL.ACEs[0].Who)
	}
	if parsedACL.ACEs[0].AccessMask != 0x001F01FF {
		t.Errorf("ACE[0].AccessMask = 0x%x, want 0x001F01FF", parsedACL.ACEs[0].AccessMask)
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

func TestParseSecurityDescriptorRoundTrip(t *testing.T) {
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

	// Build
	data, err := BuildSecurityDescriptor(file, 0)
	if err != nil {
		t.Fatalf("BuildSecurityDescriptor: %v", err)
	}

	// Parse
	ownerUID, ownerGID, parsedACL, err := ParseSecurityDescriptor(data)
	if err != nil {
		t.Fatalf("ParseSecurityDescriptor: %v", err)
	}

	// Verify owner/group
	if ownerUID == nil || *ownerUID != 500 {
		t.Errorf("Owner UID = %v, want 500", ownerUID)
	}
	if ownerGID == nil || *ownerGID != 20 {
		t.Errorf("Owner GID = %v, want 20", ownerGID)
	}

	// Verify ACEs
	if parsedACL == nil || len(parsedACL.ACEs) != 2 {
		t.Fatalf("Expected 2 ACEs, got %v", parsedACL)
	}

	// First ACE: DENY EVERYONE@ WRITE_DATA
	ace0 := parsedACL.ACEs[0]
	if ace0.Type != acl.ACE4_ACCESS_DENIED_ACE_TYPE {
		t.Errorf("ACE[0].Type = %d, want DENY", ace0.Type)
	}
	if ace0.Who != "EVERYONE@" {
		t.Errorf("ACE[0].Who = %q, want EVERYONE@", ace0.Who)
	}

	// Second ACE: ALLOW OWNER@ with inheritance flags
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

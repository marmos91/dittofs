package handlers

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
)

func TestSIDEncodeDecodeRoundTrip(t *testing.T) {
	tests := []struct {
		name   string
		sidStr string
	}{
		{"Everyone", "S-1-1-0"},
		{"CreatorOwner", "S-1-3-0"},
		{"CreatorGroup", "S-1-3-1"},
		{"NTAuthority", "S-1-5-18"},
		{"DittoFSUser1000", "S-1-5-21-0-0-0-1000"},
		{"DittoFSUser0", "S-1-5-21-0-0-0-0"},
		{"DittoFSUserMax", "S-1-5-21-0-0-0-4294967295"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Parse string to SID
			sid, err := ParseSIDString(tt.sidStr)
			if err != nil {
				t.Fatalf("ParseSIDString(%q): %v", tt.sidStr, err)
			}

			// Encode to binary
			var buf bytes.Buffer
			EncodeSID(&buf, sid)
			encoded := buf.Bytes()

			// Decode from binary
			decoded, consumed, err := DecodeSID(encoded)
			if err != nil {
				t.Fatalf("DecodeSID: %v", err)
			}
			if consumed != len(encoded) {
				t.Errorf("DecodeSID consumed %d bytes, expected %d", consumed, len(encoded))
			}

			// Format back to string
			result := FormatSID(decoded)
			if result != tt.sidStr {
				t.Errorf("Round-trip failed: started %q, got %q", tt.sidStr, result)
			}
		})
	}
}

func TestSIDSize(t *testing.T) {
	// Everyone: S-1-1-0 (1 sub-authority) -> 8 + 4*1 = 12
	sid := parseSIDMust("S-1-1-0")
	if got := SIDSize(sid); got != 12 {
		t.Errorf("SIDSize(S-1-1-0) = %d, want 12", got)
	}

	// DittoFS user: S-1-5-21-0-0-0-1000 (5 sub-authorities) -> 8 + 4*5 = 28
	sid = parseSIDMust("S-1-5-21-0-0-0-1000")
	if got := SIDSize(sid); got != 28 {
		t.Errorf("SIDSize(S-1-5-21-0-0-0-1000) = %d, want 28", got)
	}
}

func TestPrincipalToSID(t *testing.T) {
	tests := []struct {
		name       string
		who        string
		ownerUID   uint32
		ownerGID   uint32
		wantSID    string
		prefixOnly bool // when true, wantSID is treated as a prefix match
	}{
		{"OwnerAt", "OWNER@", 1000, 1000, "S-1-5-21-0-0-0-1000", false},
		{"GroupAt", "GROUP@", 1000, 1001, "S-1-5-21-0-0-0-1001", false},
		{"EveryoneAt", "EVERYONE@", 0, 0, "S-1-1-0", false},
		{"NumericUID", "501@localdomain", 0, 0, "S-1-5-21-0-0-0-501", false},
		{"NamedPrincipal", "alice@EXAMPLE.COM", 0, 0, "S-1-5-21-0-0-0-", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sid := PrincipalToSID(tt.who, tt.ownerUID, tt.ownerGID)
			result := FormatSID(sid)

			if tt.prefixOnly {
				if !strings.HasPrefix(result, tt.wantSID) {
					t.Errorf("PrincipalToSID(%q) = %q, expected prefix %q", tt.who, result, tt.wantSID)
				}
			} else if result != tt.wantSID {
				t.Errorf("PrincipalToSID(%q) = %q, want %q", tt.who, result, tt.wantSID)
			}
		})
	}
}

func TestSIDToPrincipal(t *testing.T) {
	tests := []struct {
		name      string
		sidStr    string
		wantPrinc string
	}{
		{"Everyone", "S-1-1-0", "EVERYONE@"},
		{"CreatorOwner", "S-1-3-0", "OWNER@"},
		{"CreatorGroup", "S-1-3-1", "GROUP@"},
		{"DittoFSUser1000", "S-1-5-21-0-0-0-1000", "1000@localdomain"},
		{"DittoFSUser0", "S-1-5-21-0-0-0-0", "0@localdomain"},
		{"UnknownSID", "S-1-5-32-544", "S-1-5-32-544"}, // Fallback to string
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sid := parseSIDMust(tt.sidStr)
			result := SIDToPrincipal(sid)
			if result != tt.wantPrinc {
				t.Errorf("SIDToPrincipal(%q) = %q, want %q", tt.sidStr, result, tt.wantPrinc)
			}
		})
	}
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

func TestParseSIDStringErrors(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"NoPrefix", "1-1-0"},
		{"TooShort", "S-1"},
		{"BadRevision", "S-abc-5"},
		{"BadAuthority", "S-1-abc"},
		{"BadSubAuthority", "S-1-5-abc"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseSIDString(tt.input)
			if err == nil {
				t.Errorf("ParseSIDString(%q) should fail", tt.input)
			}
		})
	}
}

func TestDecodeSIDErrors(t *testing.T) {
	// Too short
	_, _, err := DecodeSID([]byte{1, 2, 3})
	if err == nil {
		t.Error("DecodeSID with 3 bytes should fail")
	}

	// SubAuthorityCount says 2 but not enough data
	data := []byte{1, 2, 0, 0, 0, 0, 0, 5} // 2 sub-auths, but only 8 bytes
	_, _, err = DecodeSID(data)
	if err == nil {
		t.Error("DecodeSID with insufficient sub-authority data should fail")
	}
}

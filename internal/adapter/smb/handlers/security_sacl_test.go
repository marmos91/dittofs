package handlers

import (
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
)

// TestBuildSD_SACL_RoundTrip verifies that a stored SACL (audit ACEs) is
// serialized on QUERY_INFO and parses back identically on SET_INFO — closing
// the parsed-then-dropped gap (#1228 Fix C). The DACL is unaffected.
func TestBuildSD_SACL_RoundTrip(t *testing.T) {
	file := &metadata.File{
		FileAttr: metadata.FileAttr{
			UID:  1000,
			GID:  1000,
			Mode: 0o755,
			ACL: &acl.ACL{
				ACEs: []acl.ACE{
					{
						Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
						AccessMask: acl.ACE4_READ_DATA | acl.ACE4_EXECUTE,
						Who:        acl.SpecialEveryone,
					},
				},
				SACL: []acl.ACE{
					{
						Type:       acl.ACE4_SYSTEM_AUDIT_ACE_TYPE,
						Flag:       acl.ACE4_SUCCESSFUL_ACCESS_ACE_FLAG | acl.ACE4_FAILED_ACCESS_ACE_FLAG,
						AccessMask: acl.ACE4_WRITE_DATA,
						Who:        acl.SpecialEveryone,
					},
				},
			},
		},
	}

	secInfo := uint32(DACLSecurityInformation | SACLSecurityInformation)
	data, err := BuildSecurityDescriptor(file, secInfo)
	if err != nil {
		t.Fatalf("BuildSecurityDescriptor: %v", err)
	}

	// SE_SACL_PRESENT must be set and the SACL must carry exactly one ACE.
	control := binary.LittleEndian.Uint16(data[2:4])
	if control&seSACLPresent == 0 {
		t.Error("SE_SACL_PRESENT not set")
	}
	saclOffset := binary.LittleEndian.Uint32(data[12:16])
	if saclOffset == 0 {
		t.Fatal("SACL offset is 0 when a SACL is stored")
	}
	saclCount := binary.LittleEndian.Uint16(data[saclOffset+4 : saclOffset+6])
	if saclCount != 1 {
		t.Fatalf("SACL ACE count = %d, want 1", saclCount)
	}

	// Round-trip: parse the SD back and confirm the SACL survives.
	_, _, parsed, err := ParseSecurityDescriptor(data)
	if err != nil {
		t.Fatalf("ParseSecurityDescriptor: %v", err)
	}
	if parsed == nil {
		t.Fatal("parsed ACL is nil")
	}
	if len(parsed.SACL) != 1 {
		t.Fatalf("parsed SACL has %d ACEs, want 1", len(parsed.SACL))
	}
	got := parsed.SACL[0]
	if got.Type != acl.ACE4_SYSTEM_AUDIT_ACE_TYPE {
		t.Errorf("SACL ACE type = %d, want AUDIT(%d)", got.Type, acl.ACE4_SYSTEM_AUDIT_ACE_TYPE)
	}
	wantFlag := uint32(acl.ACE4_SUCCESSFUL_ACCESS_ACE_FLAG | acl.ACE4_FAILED_ACCESS_ACE_FLAG)
	if got.Flag != wantFlag {
		t.Errorf("SACL ACE flag = 0x%x, want 0x%x (success+failed audit flags must round-trip)", got.Flag, wantFlag)
	}
	if got.AccessMask != acl.ACE4_WRITE_DATA {
		t.Errorf("SACL ACE mask = 0x%x, want 0x%x", got.AccessMask, acl.ACE4_WRITE_DATA)
	}
	if got.Who != acl.SpecialEveryone {
		t.Errorf("SACL ACE who = %q, want %q", got.Who, acl.SpecialEveryone)
	}

	// DACL must still be present and unchanged.
	if len(parsed.ACEs) != 1 {
		t.Fatalf("parsed DACL has %d ACEs, want 1", len(parsed.ACEs))
	}
}

// TestBuildSD_SACL_EmptyStub_WhenNoStoredSACL verifies the nil-SACL case still
// emits the legacy empty stub (count=0, size=8) — behavior unchanged when no
// SACL is stored.
func TestBuildSD_SACL_EmptyStub_WhenNoStoredSACL(t *testing.T) {
	file := &metadata.File{
		FileAttr: metadata.FileAttr{
			UID: 1000, GID: 1000, Mode: 0o755,
			ACL: &acl.ACL{ACEs: []acl.ACE{{
				Type: acl.ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: acl.ACE4_READ_DATA, Who: acl.SpecialOwner,
			}}},
		},
	}

	data, err := BuildSecurityDescriptor(file, uint32(DACLSecurityInformation|SACLSecurityInformation))
	if err != nil {
		t.Fatalf("BuildSecurityDescriptor: %v", err)
	}
	saclOffset := binary.LittleEndian.Uint32(data[12:16])
	saclCount := binary.LittleEndian.Uint16(data[saclOffset+4 : saclOffset+6])
	if saclCount != 0 {
		t.Errorf("SACL ACE count = %d, want 0 (empty stub)", saclCount)
	}
	saclSize := binary.LittleEndian.Uint16(data[saclOffset+2 : saclOffset+4])
	if saclSize != 8 {
		t.Errorf("SACL size = %d, want 8 (empty stub)", saclSize)
	}
}

// TestParseSD_SACLOnly_NoDACL verifies that an SD carrying only a SACL section
// (SE_SACL_PRESENT, no SE_DACL_PRESENT) parses into a carrier ACL with the SACL
// populated and no DACL ACEs — so a SACL-only SET_INFO does not fabricate a DACL.
func TestParseSD_SACLOnly_NoDACL(t *testing.T) {
	// Build an SD that has a SACL but no DACL by round-tripping a file whose
	// ACL has only a SACL, requesting only the SACL section.
	file := &metadata.File{
		FileAttr: metadata.FileAttr{
			UID: 1000, GID: 1000, Mode: 0o755,
			ACL: &acl.ACL{
				SACL: []acl.ACE{{
					Type:       acl.ACE4_SYSTEM_AUDIT_ACE_TYPE,
					Flag:       acl.ACE4_FAILED_ACCESS_ACE_FLAG,
					AccessMask: acl.ACE4_WRITE_DATA,
					Who:        acl.SpecialOwner,
				}},
			},
		},
	}
	data, err := BuildSecurityDescriptor(file, uint32(SACLSecurityInformation))
	if err != nil {
		t.Fatalf("BuildSecurityDescriptor: %v", err)
	}

	_, _, parsed, err := ParseSecurityDescriptor(data)
	if err != nil {
		t.Fatalf("ParseSecurityDescriptor: %v", err)
	}
	if parsed == nil {
		t.Fatal("expected a carrier ACL for the SACL, got nil")
	}
	if len(parsed.SACL) != 1 {
		t.Fatalf("parsed SACL has %d ACEs, want 1", len(parsed.SACL))
	}
	if len(parsed.ACEs) != 0 {
		t.Errorf("SACL-only SD produced %d DACL ACEs, want 0", len(parsed.ACEs))
	}
	if parsed.NullDACL {
		t.Error("SACL-only SD must not be marked NullDACL")
	}
}

// TestMergeSecurityACL_PreservesUnrequestedSection verifies the SET_INFO merge:
// a DACL-only request preserves the current SACL, and a SACL-only request
// preserves the current DACL.
func TestMergeSecurityACL_PreservesUnrequestedSection(t *testing.T) {
	current := &acl.ACL{
		ACEs:   []acl.ACE{{Type: acl.ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: acl.ACE4_READ_DATA, Who: acl.SpecialOwner}},
		Source: acl.ACLSourceSMBExplicit,
		SACL:   []acl.ACE{{Type: acl.ACE4_SYSTEM_AUDIT_ACE_TYPE, AccessMask: acl.ACE4_WRITE_DATA, Who: acl.SpecialEveryone}},
	}

	// DACL-only SET: new DACL replaces ACEs, SACL preserved from current.
	parsedDACL := &acl.ACL{
		ACEs:   []acl.ACE{{Type: acl.ACE4_ACCESS_DENIED_ACE_TYPE, AccessMask: acl.ACE4_WRITE_DATA, Who: acl.SpecialEveryone}},
		Source: acl.ACLSourceSMBExplicit,
	}
	out := mergeSecurityACL(parsedDACL, current, true, false)
	if len(out.ACEs) != 1 || out.ACEs[0].Type != acl.ACE4_ACCESS_DENIED_ACE_TYPE {
		t.Errorf("DACL-only merge: DACL not taken from parsed: %+v", out.ACEs)
	}
	if len(out.SACL) != 1 || out.SACL[0].Type != acl.ACE4_SYSTEM_AUDIT_ACE_TYPE {
		t.Errorf("DACL-only merge: SACL not preserved from current: %+v", out.SACL)
	}

	// SACL-only SET: new SACL replaces SACL, DACL preserved from current.
	parsedSACL := &acl.ACL{
		SACL: []acl.ACE{{Type: acl.ACE4_SYSTEM_AUDIT_ACE_TYPE, AccessMask: acl.ACE4_READ_DATA, Who: acl.SpecialOwner}},
	}
	out = mergeSecurityACL(parsedSACL, current, false, true)
	if len(out.ACEs) != 1 || out.ACEs[0].Who != acl.SpecialOwner {
		t.Errorf("SACL-only merge: DACL not preserved from current: %+v", out.ACEs)
	}
	if out.Source != acl.ACLSourceSMBExplicit {
		t.Errorf("SACL-only merge: DACL Source not preserved, got %q", out.Source)
	}
	if len(out.SACL) != 1 || out.SACL[0].AccessMask != acl.ACE4_READ_DATA {
		t.Errorf("SACL-only merge: SACL not taken from parsed: %+v", out.SACL)
	}
}

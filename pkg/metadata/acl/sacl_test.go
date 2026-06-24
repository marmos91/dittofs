package acl

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestACL_SACL_JSONRoundTrip verifies the SACL field serializes and
// deserializes through JSON — the storage path used by the postgres (JSONB),
// badger, and sqlite backends. No migration is needed precisely because the
// whole ACL struct is JSON-marshaled.
func TestACL_SACL_JSONRoundTrip(t *testing.T) {
	original := &ACL{
		ACEs: []ACE{
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialOwner},
		},
		SACL: []ACE{
			{
				Type:       ACE4_SYSTEM_AUDIT_ACE_TYPE,
				Flag:       ACE4_SUCCESSFUL_ACCESS_ACE_FLAG | ACE4_FAILED_ACCESS_ACE_FLAG,
				AccessMask: ACE4_WRITE_DATA,
				Who:        SpecialEveryone,
			},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded ACL
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(decoded.SACL) != 1 {
		t.Fatalf("decoded SACL len = %d, want 1", len(decoded.SACL))
	}
	got := decoded.SACL[0]
	if got.Type != ACE4_SYSTEM_AUDIT_ACE_TYPE {
		t.Errorf("SACL Type = %d, want AUDIT", got.Type)
	}
	if got.Flag != ACE4_SUCCESSFUL_ACCESS_ACE_FLAG|ACE4_FAILED_ACCESS_ACE_FLAG {
		t.Errorf("SACL Flag = 0x%x", got.Flag)
	}
	if got.AccessMask != ACE4_WRITE_DATA || got.Who != SpecialEveryone {
		t.Errorf("SACL ACE roundtrip mismatch: %+v", got)
	}
}

// TestACL_SACL_JSONOmitEmpty verifies a nil SACL is omitted from the wire form,
// keeping existing stored ACLs byte-identical (no spurious "sacl" key).
func TestACL_SACL_JSONOmitEmpty(t *testing.T) {
	a := &ACL{ACEs: []ACE{{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialOwner}}}
	data, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "sacl") {
		t.Errorf("nil SACL should be omitted, got %s", data)
	}
}

// TestAdjustACLForMode_PreservesSACL verifies chmod does not drop the SACL.
func TestAdjustACLForMode_PreservesSACL(t *testing.T) {
	in := &ACL{
		ACEs: []ACE{{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: rwxMaskBits, Who: SpecialOwner}},
		SACL: []ACE{{Type: ACE4_SYSTEM_AUDIT_ACE_TYPE, AccessMask: ACE4_WRITE_DATA, Who: SpecialEveryone}},
	}
	out := AdjustACLForMode(in, 0o755)
	if len(out.SACL) != 1 {
		t.Fatalf("AdjustACLForMode dropped SACL: got %d entries, want 1", len(out.SACL))
	}
	if out.SACL[0].Type != ACE4_SYSTEM_AUDIT_ACE_TYPE {
		t.Errorf("SACL ACE type changed: %d", out.SACL[0].Type)
	}
}

// TestAdjustACLForMode_SACLDeepCopy verifies the "returns a new ACL; the
// original is not modified" contract extends to the SACL: mutating the returned
// ACL's SACL must not corrupt the caller's original backing array.
func TestAdjustACLForMode_SACLDeepCopy(t *testing.T) {
	in := &ACL{
		ACEs: []ACE{{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: rwxMaskBits, Who: SpecialOwner}},
		SACL: []ACE{{Type: ACE4_SYSTEM_AUDIT_ACE_TYPE, AccessMask: ACE4_WRITE_DATA, Who: SpecialEveryone}},
	}
	out := AdjustACLForMode(in, 0o755)

	// Mutate the returned SACL — the original must be untouched.
	out.SACL[0].AccessMask = ACE4_READ_DATA
	if in.SACL[0].AccessMask != ACE4_WRITE_DATA {
		t.Errorf("AdjustACLForMode aliased SACL backing array: original mutated to 0x%x", in.SACL[0].AccessMask)
	}
}

// TestValidateACL_SACLMaxSize verifies the 64KB serialized-size bound applies to
// the SACL, not just the DACL — an oversized SACL (via an unbounded Who) must be
// rejected.
func TestValidateACL_SACLMaxSize(t *testing.T) {
	huge := make([]byte, MaxDACLSize)
	for i := range huge {
		huge[i] = 'a'
	}
	a := &ACL{
		SACL: []ACE{{Type: ACE4_SYSTEM_AUDIT_ACE_TYPE, AccessMask: ACE4_WRITE_DATA, Who: string(huge)}},
	}
	if err := ValidateACL(a); err == nil {
		t.Error("oversized SACL must be rejected by the MaxDACLSize bound")
	}
}

// TestValidateACL_SACLEntries verifies SACL entries are validated and a
// malformed SACL ACE is rejected.
func TestValidateACL_SACLEntries(t *testing.T) {
	good := &ACL{
		SACL: []ACE{{Type: ACE4_SYSTEM_AUDIT_ACE_TYPE, AccessMask: ACE4_WRITE_DATA, Who: SpecialEveryone}},
	}
	if err := ValidateACL(good); err != nil {
		t.Errorf("valid SACL rejected: %v", err)
	}

	badWho := &ACL{
		SACL: []ACE{{Type: ACE4_SYSTEM_AUDIT_ACE_TYPE, AccessMask: ACE4_WRITE_DATA, Who: ""}},
	}
	if err := ValidateACL(badWho); err == nil {
		t.Error("SACL ACE with empty Who should be rejected")
	}

	badType := &ACL{
		SACL: []ACE{{Type: 0xFF, AccessMask: ACE4_WRITE_DATA, Who: SpecialEveryone}},
	}
	if err := ValidateACL(badType); err == nil {
		t.Error("SACL ACE with invalid type should be rejected")
	}
}

// TestAuditFlags_RoundTrip verifies the SUCCESSFUL/FAILED audit ACE flags map
// to/from their Windows bit positions (0x40 / 0x80) — required for SACL audit
// ACEs to round-trip.
func TestAuditFlags_RoundTrip(t *testing.T) {
	nfs := uint32(ACE4_SUCCESSFUL_ACCESS_ACE_FLAG | ACE4_FAILED_ACCESS_ACE_FLAG)
	win := NFSv4FlagsToWindowsFlags(nfs)
	if win != 0x40|0x80 {
		t.Fatalf("NFSv4->Windows audit flags = 0x%x, want 0xc0", win)
	}
	if back := WindowsFlagsToNFSv4Flags(win); back != nfs {
		t.Errorf("Windows->NFSv4 audit flags = 0x%x, want 0x%x", back, nfs)
	}
}

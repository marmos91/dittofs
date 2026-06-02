package handlers

import (
	"testing"
)

// TestDecodeFullEaEntries_SingleEntry exercises the canonical short form
// smbtorture emits for `smb2.ea.acl_xattr`: a single FILE_FULL_EA_INFORMATION
// entry with NextEntryOffset=0 and a non-empty EaName.
func TestDecodeFullEaEntries_SingleEntry(t *testing.T) {
	// Layout: NextEntryOffset=0 (4B), Flags=0, EaNameLength=4, EaValueLength=6,
	// Name="void"\0, Value="testme".
	buf := []byte{
		0x00, 0x00, 0x00, 0x00, // NextEntryOffset
		0x00,       // Flags
		0x04,       // EaNameLength
		0x06, 0x00, // EaValueLength
		'v', 'o', 'i', 'd', 0x00, // Name + NUL
		't', 'e', 's', 't', 'm', 'e', // Value
	}
	entries, err := decodeFullEaEntries(buf)
	if err != nil {
		t.Fatalf("decodeFullEaEntries: %v", err)
	}
	if len(entries) != 1 || entries[0].name != "void" || string(entries[0].value) != "testme" {
		t.Errorf("entries = %+v, want [{void testme}]", entries)
	}
}

// TestDecodeFullEaEntries_RejectsTruncatedName pins the defensive bounds-
// checking so a malformed EA chain returns an error rather than misparsing the
// value as a name.
func TestDecodeFullEaEntries_RejectsTruncatedName(t *testing.T) {
	// EaNameLength=4 but only 2 name bytes present.
	buf := []byte{
		0x00, 0x00, 0x00, 0x00, 0x00, 0x04, 0x00, 0x00,
		'a', 'b',
	}
	if _, err := decodeFullEaEntries(buf); err == nil {
		t.Fatal("expected error for truncated name, got nil")
	}
}

// TestIsReservedACLXattrName_CaseInsensitive pins the case-insensitive match
// contract so smbtorture's literal "security.NTACL" and any caller using
// "Security.ntacl" both trip the ACCESS_DENIED guard.
func TestIsReservedACLXattrName_CaseInsensitive(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"security.NTACL", true},
		{"Security.NtAcl", true},
		{"SECURITY.NTACL", true},
		{"security.ntacl_other", false},
		{"user.something", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isReservedACLXattrName(tc.name); got != tc.want {
			t.Errorf("isReservedACLXattrName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

package acl

import (
	"testing"
)

func TestNFSv4FlagsToWindowsFlags_InheritedACE(t *testing.T) {
	// Critical case: INHERITED_ACE must map 0x80 -> 0x10, NOT 0x80.
	got := NFSv4FlagsToWindowsFlags(ACE4_INHERITED_ACE)
	if got != 0x10 {
		t.Errorf("NFSv4FlagsToWindowsFlags(ACE4_INHERITED_ACE=0x80) = 0x%02x, want 0x10", got)
	}
}

func TestWindowsFlagsToNFSv4Flags_InheritedACE(t *testing.T) {
	// Inverse: Windows 0x10 must map to NFSv4 0x80.
	got := WindowsFlagsToNFSv4Flags(0x10)
	if got != ACE4_INHERITED_ACE {
		t.Errorf("WindowsFlagsToNFSv4Flags(0x10) = 0x%08x, want 0x%08x", got, ACE4_INHERITED_ACE)
	}
}

func TestNFSv4FlagsToWindowsFlags_CombinedFlags(t *testing.T) {
	// FILE_INHERIT(0x01) | DIRECTORY_INHERIT(0x02) | INHERITED(0x80) -> 0x01 | 0x02 | 0x10 = 0x13
	nfsFlags := uint32(ACE4_FILE_INHERIT_ACE | ACE4_DIRECTORY_INHERIT_ACE | ACE4_INHERITED_ACE)
	got := NFSv4FlagsToWindowsFlags(nfsFlags)
	want := uint8(0x13) // CI | OI | INHERITED
	if got != want {
		t.Errorf("NFSv4FlagsToWindowsFlags(0x%08x) = 0x%02x, want 0x%02x", nfsFlags, got, want)
	}
}

func TestFlagRoundTrip(t *testing.T) {
	// Round-trip: NFSv4 -> Windows -> NFSv4 for all valid flag combinations.
	tests := []struct {
		name     string
		nfsFlags uint32
	}{
		{"no flags", 0},
		{"file inherit", ACE4_FILE_INHERIT_ACE},
		{"directory inherit", ACE4_DIRECTORY_INHERIT_ACE},
		{"no propagate", ACE4_NO_PROPAGATE_INHERIT_ACE},
		{"inherit only", ACE4_INHERIT_ONLY_ACE},
		{"inherited", ACE4_INHERITED_ACE},
		{"CI+OI", ACE4_FILE_INHERIT_ACE | ACE4_DIRECTORY_INHERIT_ACE},
		{"CI+OI+NP", ACE4_FILE_INHERIT_ACE | ACE4_DIRECTORY_INHERIT_ACE | ACE4_NO_PROPAGATE_INHERIT_ACE},
		{"CI+OI+IO", ACE4_FILE_INHERIT_ACE | ACE4_DIRECTORY_INHERIT_ACE | ACE4_INHERIT_ONLY_ACE},
		{"CI+OI+INHERITED", ACE4_FILE_INHERIT_ACE | ACE4_DIRECTORY_INHERIT_ACE | ACE4_INHERITED_ACE},
		{"all flags", ACE4_FILE_INHERIT_ACE | ACE4_DIRECTORY_INHERIT_ACE | ACE4_NO_PROPAGATE_INHERIT_ACE | ACE4_INHERIT_ONLY_ACE | ACE4_INHERITED_ACE},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			winFlags := NFSv4FlagsToWindowsFlags(tt.nfsFlags)
			roundTripped := WindowsFlagsToNFSv4Flags(winFlags)
			if roundTripped != tt.nfsFlags {
				t.Errorf("round-trip failed: NFSv4(0x%08x) -> Win(0x%02x) -> NFSv4(0x%08x)",
					tt.nfsFlags, winFlags, roundTripped)
			}
		})
	}
}

func TestWindowsFlagsRoundTrip(t *testing.T) {
	// Round-trip: Windows -> NFSv4 -> Windows for all valid flag combinations.
	tests := []struct {
		name     string
		winFlags uint8
	}{
		{"no flags", 0},
		{"CI", 0x01},
		{"OI", 0x02},
		{"NP", 0x04},
		{"IO", 0x08},
		{"INHERITED", 0x10},
		{"CI+OI", 0x03},
		{"CI+OI+INHERITED", 0x13},
		{"all flags", 0x1F},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nfsFlags := WindowsFlagsToNFSv4Flags(tt.winFlags)
			roundTripped := NFSv4FlagsToWindowsFlags(nfsFlags)
			if roundTripped != tt.winFlags {
				t.Errorf("round-trip failed: Win(0x%02x) -> NFSv4(0x%08x) -> Win(0x%02x)",
					tt.winFlags, nfsFlags, roundTripped)
			}
		})
	}
}

func TestNFSv4FlagsToWindowsFlags_IndividualFlags(t *testing.T) {
	tests := []struct {
		name    string
		nfsFlag uint32
		winFlag uint8
	}{
		{"FILE_INHERIT", ACE4_FILE_INHERIT_ACE, 0x01},
		{"DIRECTORY_INHERIT", ACE4_DIRECTORY_INHERIT_ACE, 0x02},
		{"NO_PROPAGATE", ACE4_NO_PROPAGATE_INHERIT_ACE, 0x04},
		{"INHERIT_ONLY", ACE4_INHERIT_ONLY_ACE, 0x08},
		{"INHERITED", ACE4_INHERITED_ACE, 0x10},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NFSv4FlagsToWindowsFlags(tt.nfsFlag)
			if got != tt.winFlag {
				t.Errorf("NFSv4FlagsToWindowsFlags(0x%08x) = 0x%02x, want 0x%02x",
					tt.nfsFlag, got, tt.winFlag)
			}
		})
	}
}

package acl

import (
	"testing"
)

func TestDeriveMode_OwnerGroupEveryone(t *testing.T) {
	tests := []struct {
		name     string
		acl      *ACL
		wantMode uint32
	}{
		{
			name: "mode 755",
			acl: &ACL{ACEs: []ACE{
				{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA | ACE4_WRITE_DATA | ACE4_APPEND_DATA | ACE4_EXECUTE, Who: SpecialOwner},
				{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA | ACE4_EXECUTE, Who: SpecialGroup},
				{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA | ACE4_EXECUTE, Who: SpecialEveryone},
			}},
			wantMode: 0755,
		},
		{
			name: "mode 644",
			acl: &ACL{ACEs: []ACE{
				{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA | ACE4_WRITE_DATA | ACE4_APPEND_DATA, Who: SpecialOwner},
				{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialGroup},
				{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialEveryone},
			}},
			wantMode: 0644,
		},
		{
			name: "mode 000",
			acl: &ACL{ACEs: []ACE{
				{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: 0, Who: SpecialOwner},
				{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: 0, Who: SpecialGroup},
				{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: 0, Who: SpecialEveryone},
			}},
			wantMode: 0000,
		},
		{
			name: "mode 777",
			acl: &ACL{ACEs: []ACE{
				{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA | ACE4_WRITE_DATA | ACE4_APPEND_DATA | ACE4_EXECUTE, Who: SpecialOwner},
				{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA | ACE4_WRITE_DATA | ACE4_APPEND_DATA | ACE4_EXECUTE, Who: SpecialGroup},
				{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA | ACE4_WRITE_DATA | ACE4_APPEND_DATA | ACE4_EXECUTE, Who: SpecialEveryone},
			}},
			wantMode: 0777,
		},
		{
			name:     "nil ACL",
			acl:      nil,
			wantMode: 0,
		},
		{
			name:     "empty ACL",
			acl:      &ACL{ACEs: []ACE{}},
			wantMode: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DeriveMode(tt.acl)
			if got != tt.wantMode {
				t.Errorf("DeriveMode() = 0%o, want 0%o", got, tt.wantMode)
			}
		})
	}
}

func TestDeriveMode_SkipsDenyACEs(t *testing.T) {
	a := &ACL{ACEs: []ACE{
		{Type: ACE4_ACCESS_DENIED_ACE_TYPE, AccessMask: ACE4_WRITE_DATA, Who: SpecialOwner},
		{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA | ACE4_WRITE_DATA | ACE4_APPEND_DATA | ACE4_EXECUTE, Who: SpecialOwner},
		{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA | ACE4_EXECUTE, Who: SpecialGroup},
		{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA | ACE4_EXECUTE, Who: SpecialEveryone},
	}}

	// DENY ACEs are not used for mode derivation.
	got := DeriveMode(a)
	if got != 0755 {
		t.Errorf("DeriveMode() = 0%o, want 0755 (DENY ACEs should not affect mode)", got)
	}
}

func TestDeriveMode_SkipsInheritOnlyACEs(t *testing.T) {
	a := &ACL{ACEs: []ACE{
		{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, Flag: ACE4_INHERIT_ONLY_ACE, AccessMask: ACE4_READ_DATA | ACE4_WRITE_DATA | ACE4_APPEND_DATA | ACE4_EXECUTE, Who: SpecialOwner},
		{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialOwner},
		{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialGroup},
		{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialEveryone},
	}}

	got := DeriveMode(a)
	if got != 0444 {
		t.Errorf("DeriveMode() = 0%o, want 0444 (INHERIT_ONLY ACEs should not affect mode)", got)
	}
}

func TestDeriveMode_SkipsNamedPrincipals(t *testing.T) {
	a := &ACL{ACEs: []ACE{
		{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA | ACE4_WRITE_DATA | ACE4_APPEND_DATA | ACE4_EXECUTE, Who: "alice@example.com"},
		{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialOwner},
		{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialGroup},
		{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialEveryone},
	}}

	// Named principals are not OWNER@/GROUP@/EVERYONE@.
	got := DeriveMode(a)
	if got != 0444 {
		t.Errorf("DeriveMode() = 0%o, want 0444 (named principals should not affect mode)", got)
	}
}

func TestDeriveMode_MultipleOwnerACEs(t *testing.T) {
	// Multiple OWNER@ ALLOW ACEs: bits are OR'd together.
	a := &ACL{ACEs: []ACE{
		{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialOwner},
		{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_WRITE_DATA | ACE4_APPEND_DATA, Who: SpecialOwner},
		{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialGroup},
		{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialEveryone},
	}}

	got := DeriveMode(a)
	if got != 0644 {
		t.Errorf("DeriveMode() = 0%o, want 0644 (multiple OWNER@ ACEs should OR)", got)
	}
}

func TestAdjustACLForMode_PreservesExplicitACEs(t *testing.T) {
	original := &ACL{ACEs: []ACE{
		{Type: ACE4_ACCESS_DENIED_ACE_TYPE, AccessMask: ACE4_WRITE_DATA, Who: "alice@example.com"},
		{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA | ACE4_WRITE_DATA | ACE4_APPEND_DATA | ACE4_EXECUTE | ACE4_READ_ACL, Who: SpecialOwner},
		{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA | ACE4_EXECUTE | ACE4_WRITE_DATA | ACE4_APPEND_DATA, Who: "bob@example.com"},
		{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA | ACE4_EXECUTE, Who: SpecialGroup},
		{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA | ACE4_EXECUTE, Who: SpecialEveryone},
	}}

	// Change mode to 644.
	result := AdjustACLForMode(original, 0644)

	// Alice's DENY should be unchanged.
	if result.ACEs[0].AccessMask != ACE4_WRITE_DATA {
		t.Errorf("alice DENY mask: got 0x%x, want 0x%x", result.ACEs[0].AccessMask, ACE4_WRITE_DATA)
	}

	// Bob's ALLOW should be unchanged (not OWNER@/GROUP@/EVERYONE@).
	if result.ACEs[2].AccessMask != ACE4_READ_DATA|ACE4_EXECUTE|ACE4_WRITE_DATA|ACE4_APPEND_DATA {
		t.Errorf("bob ALLOW mask should be unchanged: got 0x%x", result.ACEs[2].AccessMask)
	}

	// OWNER@ ALLOW should have rw- (no execute) PLUS preserved READ_ACL.
	ownerExpected := uint32(ACE4_READ_DATA | ACE4_WRITE_DATA | ACE4_APPEND_DATA | ACE4_READ_ACL)
	if result.ACEs[1].AccessMask != ownerExpected {
		t.Errorf("OWNER@ ALLOW mask: got 0x%x, want 0x%x", result.ACEs[1].AccessMask, ownerExpected)
	}

	// GROUP@ ALLOW should have r-- (0644 group = r--).
	groupExpected := uint32(ACE4_READ_DATA)
	if result.ACEs[3].AccessMask != groupExpected {
		t.Errorf("GROUP@ ALLOW mask: got 0x%x, want 0x%x", result.ACEs[3].AccessMask, groupExpected)
	}

	// EVERYONE@ ALLOW should have r-- (0644 other = r--).
	otherExpected := uint32(ACE4_READ_DATA)
	if result.ACEs[4].AccessMask != otherExpected {
		t.Errorf("EVERYONE@ ALLOW mask: got 0x%x, want 0x%x", result.ACEs[4].AccessMask, otherExpected)
	}
}

func TestAdjustACLForMode_Mode755(t *testing.T) {
	original := &ACL{ACEs: []ACE{
		{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialOwner},
		{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialGroup},
		{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialEveryone},
	}}

	result := AdjustACLForMode(original, 0755)

	// OWNER@ should have rwx.
	ownerExpected := uint32(ACE4_READ_DATA | ACE4_WRITE_DATA | ACE4_APPEND_DATA | ACE4_EXECUTE)
	if result.ACEs[0].AccessMask != ownerExpected {
		t.Errorf("OWNER@ mask: got 0x%x, want 0x%x", result.ACEs[0].AccessMask, ownerExpected)
	}

	// GROUP@ should have r-x.
	groupExpected := uint32(ACE4_READ_DATA | ACE4_EXECUTE)
	if result.ACEs[1].AccessMask != uint32(groupExpected) {
		t.Errorf("GROUP@ mask: got 0x%x, want 0x%x", result.ACEs[1].AccessMask, groupExpected)
	}

	// EVERYONE@ should have r-x.
	if result.ACEs[2].AccessMask != uint32(groupExpected) {
		t.Errorf("EVERYONE@ mask: got 0x%x, want 0x%x", result.ACEs[2].AccessMask, groupExpected)
	}
}

func TestAdjustACLForMode_Mode000(t *testing.T) {
	original := &ACL{ACEs: []ACE{
		{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA | ACE4_WRITE_DATA | ACE4_APPEND_DATA | ACE4_EXECUTE, Who: SpecialOwner},
		{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA | ACE4_EXECUTE, Who: SpecialGroup},
		{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialEveryone},
	}}

	result := AdjustACLForMode(original, 0000)

	// All rwx bits should be cleared.
	for i, ace := range result.ACEs {
		rwx := ace.AccessMask & rwxMaskBits
		if rwx != 0 {
			t.Errorf("ACE %d mask should have no rwx bits: got 0x%x", i, rwx)
		}
	}
}

func TestAdjustACLForMode_PreservesNonRWXBits(t *testing.T) {
	original := &ACL{ACEs: []ACE{
		{
			Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
			AccessMask: ACE4_READ_DATA | ACE4_READ_ACL | ACE4_WRITE_ACL | ACE4_DELETE | ACE4_SYNCHRONIZE,
			Who:        SpecialOwner,
		},
	}}

	result := AdjustACLForMode(original, 0700)

	// Non-rwx bits (READ_ACL, WRITE_ACL, DELETE, SYNCHRONIZE) should be preserved.
	expected := ACE4_READ_DATA | ACE4_WRITE_DATA | ACE4_APPEND_DATA | ACE4_EXECUTE |
		ACE4_READ_ACL | ACE4_WRITE_ACL | ACE4_DELETE | ACE4_SYNCHRONIZE
	if result.ACEs[0].AccessMask != uint32(expected) {
		t.Errorf("OWNER@ mask: got 0x%x, want 0x%x", result.ACEs[0].AccessMask, expected)
	}
}

func TestAdjustACLForMode_DenyACE(t *testing.T) {
	original := &ACL{ACEs: []ACE{
		{Type: ACE4_ACCESS_DENIED_ACE_TYPE, AccessMask: ACE4_WRITE_DATA | ACE4_APPEND_DATA, Who: SpecialOwner},
		{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA | ACE4_WRITE_DATA | ACE4_APPEND_DATA | ACE4_EXECUTE, Who: SpecialOwner},
	}}

	// Change to mode 500 (r-x for owner).
	result := AdjustACLForMode(original, 0500)

	// DENY ACE for OWNER@ should deny w (the bits NOT in mode 5=r-x).
	denyExpected := ACE4_WRITE_DATA | ACE4_APPEND_DATA
	if result.ACEs[0].AccessMask != uint32(denyExpected) {
		t.Errorf("OWNER@ DENY mask: got 0x%x, want 0x%x", result.ACEs[0].AccessMask, denyExpected)
	}

	// ALLOW ACE for OWNER@ should have r-x.
	allowExpected := ACE4_READ_DATA | ACE4_EXECUTE
	if result.ACEs[1].AccessMask != uint32(allowExpected) {
		t.Errorf("OWNER@ ALLOW mask: got 0x%x, want 0x%x", result.ACEs[1].AccessMask, allowExpected)
	}
}

func TestAdjustACLForMode_NilACL(t *testing.T) {
	result := AdjustACLForMode(nil, 0755)
	if result != nil {
		t.Error("expected nil result for nil ACL")
	}
}

func TestAdjustACLForMode_DoesNotModifyOriginal(t *testing.T) {
	original := &ACL{ACEs: []ACE{
		{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: ACE4_READ_DATA, Who: SpecialOwner},
	}}

	originalMask := original.ACEs[0].AccessMask

	_ = AdjustACLForMode(original, 0777)

	// Original should not be modified.
	if original.ACEs[0].AccessMask != originalMask {
		t.Error("AdjustACLForMode modified the original ACL")
	}
}

func TestModeRoundTrip(t *testing.T) {
	// Set mode -> derive mode should match.
	modes := []uint32{0755, 0644, 0777, 0000, 0700, 0500, 0400}

	for _, mode := range modes {
		base := &ACL{ACEs: []ACE{
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: 0, Who: SpecialOwner},
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: 0, Who: SpecialGroup},
			{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, AccessMask: 0, Who: SpecialEveryone},
		}}

		adjusted := AdjustACLForMode(base, mode)
		derived := DeriveMode(adjusted)

		if derived != mode {
			t.Errorf("round-trip mode 0%o: AdjustACLForMode -> DeriveMode = 0%o", mode, derived)
		}
	}
}

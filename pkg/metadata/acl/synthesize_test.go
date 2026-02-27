package acl

import (
	"testing"
)

// Helper to find ACEs by type and who.
func findACE(aces []ACE, typ uint32, who string) *ACE {
	for i := range aces {
		if aces[i].Type == typ && aces[i].Who == who {
			return &aces[i]
		}
	}
	return nil
}

func TestSynthesizeFromMode_0755_Directory(t *testing.T) {
	acl := SynthesizeFromMode(0755, 1000, 1000, true)
	if acl == nil {
		t.Fatal("SynthesizeFromMode returned nil")
	}

	// Expect: DENY GROUP@ write, DENY EVERYONE@ write, ALLOW OWNER@ rwx,
	// ALLOW GROUP@ rx, ALLOW EVERYONE@ rx, ALLOW SYSTEM@ full, ALLOW ADMIN@ full.
	// Owner has rwx(7), group has rx(5), other has rx(5).
	// Both group and other are missing write vs owner (7 &^ 5 = 2),
	// so both GROUP@ and EVERYONE@ get deny ACEs for write.

	// Check deny: EVERYONE@ should be denied write bits (owner has w, other doesn't).
	denyEveryone := findACE(acl.ACEs, ACE4_ACCESS_DENIED_ACE_TYPE, SpecialEveryone)
	if denyEveryone == nil {
		t.Fatal("expected DENY EVERYONE@ ACE for write restriction")
	}
	// ownerRWX=7, otherRWX=5, diff=2 (write). Should deny write-related bits.
	writeMask := rwxToFullMask(2, true) // write bit for directory
	if denyEveryone.AccessMask != writeMask {
		t.Errorf("DENY EVERYONE@ mask = 0x%08x, want 0x%08x", denyEveryone.AccessMask, writeMask)
	}

	// GROUP@ deny exists (group=5, ownerRWX &^ groupRWX = 7 &^ 5 = 2, deny write).
	denyGroup := findACE(acl.ACEs, ACE4_ACCESS_DENIED_ACE_TYPE, SpecialGroup)
	if denyGroup == nil {
		t.Fatal("expected DENY GROUP@ ACE for write restriction")
	}

	// Check allow OWNER@.
	allowOwner := findACE(acl.ACEs, ACE4_ACCESS_ALLOWED_ACE_TYPE, SpecialOwner)
	if allowOwner == nil {
		t.Fatal("expected ALLOW OWNER@ ACE")
	}
	expectedOwnerMask := rwxToFullMask(7, true) | alwaysGrantedMask
	if allowOwner.AccessMask != expectedOwnerMask {
		t.Errorf("ALLOW OWNER@ mask = 0x%08x, want 0x%08x", allowOwner.AccessMask, expectedOwnerMask)
	}

	// Check allow GROUP@.
	allowGroup := findACE(acl.ACEs, ACE4_ACCESS_ALLOWED_ACE_TYPE, SpecialGroup)
	if allowGroup == nil {
		t.Fatal("expected ALLOW GROUP@ ACE")
	}
	expectedGroupMask := rwxToFullMask(5, true) // rx
	if allowGroup.AccessMask != expectedGroupMask {
		t.Errorf("ALLOW GROUP@ mask = 0x%08x, want 0x%08x", allowGroup.AccessMask, expectedGroupMask)
	}

	// Check allow EVERYONE@.
	allowEveryone := findACE(acl.ACEs, ACE4_ACCESS_ALLOWED_ACE_TYPE, SpecialEveryone)
	if allowEveryone == nil {
		t.Fatal("expected ALLOW EVERYONE@ ACE")
	}
	expectedOtherMask := rwxToFullMask(5, true) // rx
	if allowEveryone.AccessMask != expectedOtherMask {
		t.Errorf("ALLOW EVERYONE@ mask = 0x%08x, want 0x%08x", allowEveryone.AccessMask, expectedOtherMask)
	}

	// Check SYSTEM@ and ADMINISTRATORS@.
	allowSystem := findACE(acl.ACEs, ACE4_ACCESS_ALLOWED_ACE_TYPE, SpecialSystem)
	if allowSystem == nil {
		t.Fatal("expected ALLOW SYSTEM@ ACE")
	}
	if allowSystem.AccessMask != FullAccessMask {
		t.Errorf("ALLOW SYSTEM@ mask = 0x%08x, want 0x%08x", allowSystem.AccessMask, FullAccessMask)
	}

	allowAdmin := findACE(acl.ACEs, ACE4_ACCESS_ALLOWED_ACE_TYPE, SpecialAdministrators)
	if allowAdmin == nil {
		t.Fatal("expected ALLOW ADMINISTRATORS@ ACE")
	}
	if allowAdmin.AccessMask != FullAccessMask {
		t.Errorf("ALLOW ADMINISTRATORS@ mask = 0x%08x, want 0x%08x", allowAdmin.AccessMask, FullAccessMask)
	}

	// All directory ACEs should have CI+OI flags.
	for i, ace := range acl.ACEs {
		ciOi := uint32(ACE4_FILE_INHERIT_ACE | ACE4_DIRECTORY_INHERIT_ACE)
		if ace.Flag&ciOi != ciOi {
			t.Errorf("ACE %d (%s %s): missing CI+OI flags, flag=0x%08x", i, ace.TypeString(), ace.Who, ace.Flag)
		}
	}
}

func TestSynthesizeFromMode_0750_Directory(t *testing.T) {
	acl := SynthesizeFromMode(0750, 1000, 1000, true)
	if acl == nil {
		t.Fatal("SynthesizeFromMode returned nil")
	}

	// ownerRWX=7, groupRWX=5, otherRWX=0
	// DENY GROUP@ write (7 &^ 5 = 2)
	// DENY EVERYONE@ rwx (7 &^ 0 = 7)
	// ALLOW OWNER@ rwx
	// ALLOW GROUP@ rx
	// No ALLOW EVERYONE@ (otherRWX=0)
	// ALLOW SYSTEM@, ALLOW ADMINISTRATORS@

	denyGroup := findACE(acl.ACEs, ACE4_ACCESS_DENIED_ACE_TYPE, SpecialGroup)
	if denyGroup == nil {
		t.Fatal("expected DENY GROUP@ ACE")
	}
	writeMask := rwxToFullMask(2, true)
	if denyGroup.AccessMask != writeMask {
		t.Errorf("DENY GROUP@ mask = 0x%08x, want 0x%08x", denyGroup.AccessMask, writeMask)
	}

	denyEveryone := findACE(acl.ACEs, ACE4_ACCESS_DENIED_ACE_TYPE, SpecialEveryone)
	if denyEveryone == nil {
		t.Fatal("expected DENY EVERYONE@ ACE")
	}
	rwxMask := rwxToFullMask(7, true)
	if denyEveryone.AccessMask != rwxMask {
		t.Errorf("DENY EVERYONE@ mask = 0x%08x, want 0x%08x", denyEveryone.AccessMask, rwxMask)
	}

	// No ALLOW EVERYONE@ since otherRWX=0.
	allowEveryone := findACE(acl.ACEs, ACE4_ACCESS_ALLOWED_ACE_TYPE, SpecialEveryone)
	if allowEveryone != nil {
		t.Error("unexpected ALLOW EVERYONE@ ACE when other has no permissions")
	}

	// ALLOW GROUP@ should have rx.
	allowGroup := findACE(acl.ACEs, ACE4_ACCESS_ALLOWED_ACE_TYPE, SpecialGroup)
	if allowGroup == nil {
		t.Fatal("expected ALLOW GROUP@ ACE")
	}
}

func TestSynthesizeFromMode_0644_File(t *testing.T) {
	acl := SynthesizeFromMode(0644, 1000, 1000, false)
	if acl == nil {
		t.Fatal("SynthesizeFromMode returned nil")
	}

	// ownerRWX=6, groupRWX=4, otherRWX=4
	// DENY GROUP@ write (6 &^ 4 = 2)
	// DENY EVERYONE@ write (6 &^ 4 = 2)
	// ALLOW OWNER@ rw
	// ALLOW GROUP@ r
	// ALLOW EVERYONE@ r
	// ALLOW SYSTEM@, ALLOW ADMINISTRATORS@

	denyGroup := findACE(acl.ACEs, ACE4_ACCESS_DENIED_ACE_TYPE, SpecialGroup)
	if denyGroup == nil {
		t.Fatal("expected DENY GROUP@ ACE")
	}
	writeMask := rwxToFullMask(2, false)
	if denyGroup.AccessMask != writeMask {
		t.Errorf("DENY GROUP@ mask = 0x%08x, want 0x%08x", denyGroup.AccessMask, writeMask)
	}

	denyEveryone := findACE(acl.ACEs, ACE4_ACCESS_DENIED_ACE_TYPE, SpecialEveryone)
	if denyEveryone == nil {
		t.Fatal("expected DENY EVERYONE@ ACE")
	}
	if denyEveryone.AccessMask != writeMask {
		t.Errorf("DENY EVERYONE@ mask = 0x%08x, want 0x%08x", denyEveryone.AccessMask, writeMask)
	}

	// File ACEs should NOT have CI+OI flags.
	for i, ace := range acl.ACEs {
		ciOi := uint32(ACE4_FILE_INHERIT_ACE | ACE4_DIRECTORY_INHERIT_ACE)
		if ace.Flag&ciOi != 0 {
			t.Errorf("ACE %d (%s %s): file ACE should not have CI+OI flags, flag=0x%08x",
				i, ace.TypeString(), ace.Who, ace.Flag)
		}
	}

	allowOwner := findACE(acl.ACEs, ACE4_ACCESS_ALLOWED_ACE_TYPE, SpecialOwner)
	if allowOwner == nil {
		t.Fatal("expected ALLOW OWNER@ ACE")
	}
	expectedOwnerMask := rwxToFullMask(6, false) | alwaysGrantedMask
	if allowOwner.AccessMask != expectedOwnerMask {
		t.Errorf("ALLOW OWNER@ mask = 0x%08x, want 0x%08x", allowOwner.AccessMask, expectedOwnerMask)
	}
}

func TestSynthesizeFromMode_0000(t *testing.T) {
	acl := SynthesizeFromMode(0000, 1000, 1000, false)
	if acl == nil {
		t.Fatal("SynthesizeFromMode returned nil")
	}

	// ownerRWX=0, groupRWX=0, otherRWX=0
	// No DENY ACEs (no bits to deny: 0 &^ 0 = 0).
	// ALLOW OWNER@ with only alwaysGrantedMask (no rwx).
	// No ALLOW GROUP@ or EVERYONE@ (zero perms).
	// ALLOW SYSTEM@, ALLOW ADMINISTRATORS@.

	for _, ace := range acl.ACEs {
		if ace.Type == ACE4_ACCESS_DENIED_ACE_TYPE {
			t.Errorf("unexpected DENY ACE for mode 0000: %s", ace.Who)
		}
	}

	allowOwner := findACE(acl.ACEs, ACE4_ACCESS_ALLOWED_ACE_TYPE, SpecialOwner)
	if allowOwner == nil {
		t.Fatal("expected ALLOW OWNER@ ACE")
	}
	// Only admin rights, no rwx.
	if allowOwner.AccessMask != alwaysGrantedMask {
		t.Errorf("ALLOW OWNER@ mask = 0x%08x, want 0x%08x (only admin rights)", allowOwner.AccessMask, alwaysGrantedMask)
	}

	// No GROUP@ or EVERYONE@ allow.
	if findACE(acl.ACEs, ACE4_ACCESS_ALLOWED_ACE_TYPE, SpecialGroup) != nil {
		t.Error("unexpected ALLOW GROUP@ for mode 0000")
	}
	if findACE(acl.ACEs, ACE4_ACCESS_ALLOWED_ACE_TYPE, SpecialEveryone) != nil {
		t.Error("unexpected ALLOW EVERYONE@ for mode 0000")
	}

	// SYSTEM and ADMINISTRATORS should still be present.
	if findACE(acl.ACEs, ACE4_ACCESS_ALLOWED_ACE_TYPE, SpecialSystem) == nil {
		t.Error("expected ALLOW SYSTEM@ ACE")
	}
	if findACE(acl.ACEs, ACE4_ACCESS_ALLOWED_ACE_TYPE, SpecialAdministrators) == nil {
		t.Error("expected ALLOW ADMINISTRATORS@ ACE")
	}
}

func TestSynthesizeFromMode_0777(t *testing.T) {
	acl := SynthesizeFromMode(0777, 1000, 1000, true)
	if acl == nil {
		t.Fatal("SynthesizeFromMode returned nil")
	}

	// All equal to owner: no DENY ACEs.
	for _, ace := range acl.ACEs {
		if ace.Type == ACE4_ACCESS_DENIED_ACE_TYPE {
			t.Errorf("unexpected DENY ACE for mode 0777: %s", ace.Who)
		}
	}

	// ALLOW OWNER@ rwx + admin, ALLOW GROUP@ rwx, ALLOW EVERYONE@ rwx,
	// ALLOW SYSTEM@ full, ALLOW ADMINISTRATORS@ full.
	allowOwner := findACE(acl.ACEs, ACE4_ACCESS_ALLOWED_ACE_TYPE, SpecialOwner)
	if allowOwner == nil {
		t.Fatal("expected ALLOW OWNER@ ACE")
	}

	allowGroup := findACE(acl.ACEs, ACE4_ACCESS_ALLOWED_ACE_TYPE, SpecialGroup)
	if allowGroup == nil {
		t.Fatal("expected ALLOW GROUP@ ACE")
	}

	allowEveryone := findACE(acl.ACEs, ACE4_ACCESS_ALLOWED_ACE_TYPE, SpecialEveryone)
	if allowEveryone == nil {
		t.Fatal("expected ALLOW EVERYONE@ ACE")
	}

	// Verify rwx masks for directory.
	expectedRWX := rwxToFullMask(7, true)
	if allowGroup.AccessMask != expectedRWX {
		t.Errorf("ALLOW GROUP@ mask = 0x%08x, want 0x%08x", allowGroup.AccessMask, expectedRWX)
	}
	if allowEveryone.AccessMask != expectedRWX {
		t.Errorf("ALLOW EVERYONE@ mask = 0x%08x, want 0x%08x", allowEveryone.AccessMask, expectedRWX)
	}
}

func TestSynthesizeFromMode_0700(t *testing.T) {
	acl := SynthesizeFromMode(0700, 0, 0, false)
	if acl == nil {
		t.Fatal("SynthesizeFromMode returned nil")
	}

	// ownerRWX=7, groupRWX=0, otherRWX=0
	// DENY GROUP@ rwx (7 &^ 0 = 7)
	// DENY EVERYONE@ rwx (7 &^ 0 = 7)
	// ALLOW OWNER@ rwx + admin
	// No ALLOW GROUP@ or EVERYONE@
	// ALLOW SYSTEM@, ALLOW ADMINISTRATORS@

	denyGroup := findACE(acl.ACEs, ACE4_ACCESS_DENIED_ACE_TYPE, SpecialGroup)
	if denyGroup == nil {
		t.Fatal("expected DENY GROUP@ ACE")
	}
	rwxMask := rwxToFullMask(7, false)
	if denyGroup.AccessMask != rwxMask {
		t.Errorf("DENY GROUP@ mask = 0x%08x, want 0x%08x", denyGroup.AccessMask, rwxMask)
	}

	denyEveryone := findACE(acl.ACEs, ACE4_ACCESS_DENIED_ACE_TYPE, SpecialEveryone)
	if denyEveryone == nil {
		t.Fatal("expected DENY EVERYONE@ ACE")
	}
	if denyEveryone.AccessMask != rwxMask {
		t.Errorf("DENY EVERYONE@ mask = 0x%08x, want 0x%08x", denyEveryone.AccessMask, rwxMask)
	}

	if findACE(acl.ACEs, ACE4_ACCESS_ALLOWED_ACE_TYPE, SpecialGroup) != nil {
		t.Error("unexpected ALLOW GROUP@ for mode 0700")
	}
	if findACE(acl.ACEs, ACE4_ACCESS_ALLOWED_ACE_TYPE, SpecialEveryone) != nil {
		t.Error("unexpected ALLOW EVERYONE@ for mode 0700")
	}
}

func TestSynthesizeCanonicalOrdering(t *testing.T) {
	modes := []uint32{0755, 0750, 0644, 0000, 0777, 0700, 0400, 0111, 0666}
	for _, mode := range modes {
		for _, isDir := range []bool{true, false} {
			acl := SynthesizeFromMode(mode, 1000, 1000, isDir)
			if err := ValidateACL(acl); err != nil {
				t.Errorf("mode 0%o isDir=%v: ValidateACL failed: %v", mode, isDir, err)
			}
		}
	}
}

func TestSynthesizeInheritanceFlags(t *testing.T) {
	ciOi := uint32(ACE4_FILE_INHERIT_ACE | ACE4_DIRECTORY_INHERIT_ACE)

	// Directory: all ACEs should have CI+OI.
	dirACL := SynthesizeFromMode(0755, 1000, 1000, true)
	for i, ace := range dirACL.ACEs {
		if ace.Flag&ciOi != ciOi {
			t.Errorf("dir ACE %d (%s %s): missing CI+OI, flag=0x%08x", i, ace.TypeString(), ace.Who, ace.Flag)
		}
	}

	// File: no ACEs should have CI+OI.
	fileACL := SynthesizeFromMode(0755, 1000, 1000, false)
	for i, ace := range fileACL.ACEs {
		if ace.Flag&ciOi != 0 {
			t.Errorf("file ACE %d (%s %s): unexpected CI+OI, flag=0x%08x", i, ace.TypeString(), ace.Who, ace.Flag)
		}
	}
}

func TestSynthesizeSourceTracking(t *testing.T) {
	acl := SynthesizeFromMode(0755, 1000, 1000, true)
	if acl.Source != ACLSourcePOSIXDerived {
		t.Errorf("Source = %q, want %q", acl.Source, ACLSourcePOSIXDerived)
	}
}

func TestRwxToFullMask_File(t *testing.T) {
	tests := []struct {
		name string
		rwx  uint32
	}{
		{"no perms", 0},
		{"read only", 4},
		{"write only", 2},
		{"execute only", 1},
		{"read-write", 6},
		{"read-execute", 5},
		{"write-execute", 3},
		{"full rwx", 7},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mask := rwxToFullMask(tt.rwx, false)

			// Verify read bits.
			if tt.rwx&modeRead != 0 {
				if mask&ACE4_READ_DATA == 0 {
					t.Error("missing READ_DATA for read permission")
				}
				if mask&ACE4_READ_ATTRIBUTES == 0 {
					t.Error("missing READ_ATTRIBUTES for read permission")
				}
				if mask&ACE4_READ_NAMED_ATTRS == 0 {
					t.Error("missing READ_NAMED_ATTRS for read permission")
				}
				if mask&ACE4_READ_ACL == 0 {
					t.Error("missing READ_ACL for read permission")
				}
			}

			// Verify write bits.
			if tt.rwx&modeWrite != 0 {
				if mask&ACE4_WRITE_DATA == 0 {
					t.Error("missing WRITE_DATA for write permission")
				}
				if mask&ACE4_APPEND_DATA == 0 {
					t.Error("missing APPEND_DATA for write permission")
				}
				if mask&ACE4_WRITE_ATTRIBUTES == 0 {
					t.Error("missing WRITE_ATTRIBUTES for write permission")
				}
				// File: no DELETE_CHILD.
				if mask&ACE4_DELETE_CHILD != 0 {
					t.Error("unexpected DELETE_CHILD for file write permission")
				}
			}

			// Verify execute bits.
			if tt.rwx&modeExecute != 0 {
				if mask&ACE4_EXECUTE == 0 {
					t.Error("missing EXECUTE for execute permission")
				}
			}
		})
	}
}

func TestRwxToFullMask_Directory(t *testing.T) {
	// Write on directory should include DELETE_CHILD.
	mask := rwxToFullMask(2, true) // write bit
	if mask&ACE4_DELETE_CHILD == 0 {
		t.Error("missing DELETE_CHILD for directory write permission")
	}

	// Read on directory should not include DELETE_CHILD.
	mask = rwxToFullMask(4, true) // read bit
	if mask&ACE4_DELETE_CHILD != 0 {
		t.Error("unexpected DELETE_CHILD for directory read permission")
	}
}

func TestFullAccessMask_Coverage(t *testing.T) {
	// FullAccessMask should include all access bits.
	expectedBits := []uint32{
		ACE4_READ_DATA, ACE4_WRITE_DATA, ACE4_APPEND_DATA,
		ACE4_READ_NAMED_ATTRS, ACE4_WRITE_NAMED_ATTRS,
		ACE4_EXECUTE, ACE4_DELETE_CHILD,
		ACE4_READ_ATTRIBUTES, ACE4_WRITE_ATTRIBUTES,
		ACE4_DELETE, ACE4_READ_ACL, ACE4_WRITE_ACL,
		ACE4_WRITE_OWNER, ACE4_SYNCHRONIZE,
	}

	for _, bit := range expectedBits {
		if FullAccessMask&bit == 0 {
			t.Errorf("FullAccessMask missing bit 0x%08x", bit)
		}
	}
}

func TestSynthesizeWellKnownSIDs(t *testing.T) {
	// SYSTEM and ADMINISTRATORS should always be present, even for mode 0000.
	modes := []uint32{0000, 0700, 0755, 0777}
	for _, mode := range modes {
		acl := SynthesizeFromMode(mode, 0, 0, false)

		sys := findACE(acl.ACEs, ACE4_ACCESS_ALLOWED_ACE_TYPE, SpecialSystem)
		if sys == nil {
			t.Errorf("mode 0%o: missing SYSTEM@ ACE", mode)
			continue
		}
		if sys.AccessMask != FullAccessMask {
			t.Errorf("mode 0%o: SYSTEM@ mask = 0x%08x, want 0x%08x", mode, sys.AccessMask, FullAccessMask)
		}

		admin := findACE(acl.ACEs, ACE4_ACCESS_ALLOWED_ACE_TYPE, SpecialAdministrators)
		if admin == nil {
			t.Errorf("mode 0%o: missing ADMINISTRATORS@ ACE", mode)
			continue
		}
		if admin.AccessMask != FullAccessMask {
			t.Errorf("mode 0%o: ADMINISTRATORS@ mask = 0x%08x, want 0x%08x", mode, admin.AccessMask, FullAccessMask)
		}
	}
}

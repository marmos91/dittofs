package acl

import (
	"fmt"
	"testing"
)

func TestComputeInheritedACL_FileInheritOnChildFile(t *testing.T) {
	// AutoInherited=true on the parent so the child carries INHERITED_ACE
	// per MS-DTYP §2.5.3.4.2 (Samba calculate_inherited_from_parent).
	parentACL := &ACL{
		AutoInherited: true,
		ACEs: []ACE{
			{
				Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Flag:       ACE4_FILE_INHERIT_ACE,
				AccessMask: ACE4_READ_DATA,
				Who:        SpecialEveryone,
			},
		},
	}

	result := ComputeInheritedACL(parentACL, false, Creator{})

	if result == nil {
		t.Fatal("expected non-nil result for file with FILE_INHERIT parent ACE")
	}
	if len(result.ACEs) != 1 {
		t.Fatalf("expected 1 ACE, got %d", len(result.ACEs))
	}

	ace := result.ACEs[0]
	// Should have INHERITED_ACE flag set.
	if !ace.IsInherited() {
		t.Error("expected INHERITED_ACE flag on child ACE")
	}
	// All inheritance flags should be cleared for files.
	if ace.Flag&inheritanceMask != 0 {
		t.Errorf("expected all inheritance flags cleared for file ACE, got flag 0x%x", ace.Flag)
	}
	// Permission mask should be preserved.
	if ace.AccessMask != ACE4_READ_DATA {
		t.Errorf("expected AccessMask to be preserved, got 0x%x", ace.AccessMask)
	}
}

func TestComputeInheritedACL_DirectoryInheritOnChildDir(t *testing.T) {
	parentACL := &ACL{
		AutoInherited: true,
		ACEs: []ACE{
			{
				Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Flag:       ACE4_DIRECTORY_INHERIT_ACE,
				AccessMask: ACE4_READ_DATA | ACE4_EXECUTE,
				Who:        SpecialGroup,
			},
		},
	}

	result := ComputeInheritedACL(parentACL, true, Creator{})

	if result == nil {
		t.Fatal("expected non-nil result for directory with DIRECTORY_INHERIT parent ACE")
	}
	if len(result.ACEs) != 1 {
		t.Fatalf("expected 1 ACE, got %d", len(result.ACEs))
	}

	ace := result.ACEs[0]
	if !ace.IsInherited() {
		t.Error("expected INHERITED_ACE flag on child ACE")
	}
	// DIRECTORY_INHERIT should still be set (propagates to grandchildren).
	if ace.Flag&ACE4_DIRECTORY_INHERIT_ACE == 0 {
		t.Error("expected DIRECTORY_INHERIT to be preserved for directory inheritance")
	}
}

func TestComputeInheritedACL_NoPropagateStopsInheritance(t *testing.T) {
	parentACL := &ACL{
		AutoInherited: true,
		ACEs: []ACE{
			{
				Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Flag:       ACE4_DIRECTORY_INHERIT_ACE | ACE4_NO_PROPAGATE_INHERIT_ACE,
				AccessMask: ACE4_READ_DATA,
				Who:        SpecialOwner,
			},
		},
	}

	result := ComputeInheritedACL(parentACL, true, Creator{})

	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if len(result.ACEs) != 1 {
		t.Fatalf("expected 1 ACE, got %d", len(result.ACEs))
	}

	ace := result.ACEs[0]
	if !ace.IsInherited() {
		t.Error("expected INHERITED_ACE flag set")
	}
	// All inheritance flags should be cleared due to NO_PROPAGATE.
	if ace.Flag&inheritanceMask != 0 {
		t.Errorf("expected all inheritance flags cleared due to NO_PROPAGATE, got flag 0x%x", ace.Flag)
	}
}

func TestComputeInheritedACL_InheritOnlyClearedOnChild(t *testing.T) {
	// INHERIT_ONLY + DIRECTORY_INHERIT: ACE does NOT apply to parent,
	// but DOES apply to child directory (INHERIT_ONLY should be cleared).
	parentACL := &ACL{
		AutoInherited: true,
		ACEs: []ACE{
			{
				Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Flag:       ACE4_DIRECTORY_INHERIT_ACE | ACE4_INHERIT_ONLY_ACE,
				AccessMask: ACE4_WRITE_DATA,
				Who:        SpecialEveryone,
			},
		},
	}

	result := ComputeInheritedACL(parentACL, true, Creator{})

	if result == nil {
		t.Fatal("expected non-nil result")
	}

	ace := result.ACEs[0]
	// INHERIT_ONLY should be cleared: the ACE now applies to this directory.
	if ace.IsInheritOnly() {
		t.Error("expected INHERIT_ONLY to be cleared on inherited ACE")
	}
	// DIRECTORY_INHERIT should still be set (continues propagating).
	if ace.Flag&ACE4_DIRECTORY_INHERIT_ACE == 0 {
		t.Error("expected DIRECTORY_INHERIT to be preserved")
	}
	if !ace.IsInherited() {
		t.Error("expected INHERITED_ACE flag set")
	}
}

func TestComputeInheritedACL_NilParent(t *testing.T) {
	result := ComputeInheritedACL(nil, false, Creator{})
	if result != nil {
		t.Error("expected nil result for nil parent ACL")
	}

	result = ComputeInheritedACL(nil, true, Creator{})
	if result != nil {
		t.Error("expected nil result for nil parent ACL (directory)")
	}
}

func TestComputeInheritedACL_NoInheritableACEs(t *testing.T) {
	parentACL := &ACL{ACEs: []ACE{
		{
			Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
			Flag:       0, // No inheritance flags.
			AccessMask: ACE4_READ_DATA,
			Who:        SpecialEveryone,
		},
	}}

	result := ComputeInheritedACL(parentACL, false, Creator{})
	if result != nil {
		t.Error("expected nil for file when no FILE_INHERIT flag")
	}

	result = ComputeInheritedACL(parentACL, true, Creator{})
	if result != nil {
		t.Error("expected nil for directory when no DIRECTORY_INHERIT flag")
	}
}

func TestComputeInheritedACL_MixedInheritFlags(t *testing.T) {
	parentACL := &ACL{ACEs: []ACE{
		{
			Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
			Flag:       ACE4_FILE_INHERIT_ACE, // file only
			AccessMask: ACE4_READ_DATA,
			Who:        SpecialEveryone,
		},
		{
			Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
			Flag:       ACE4_DIRECTORY_INHERIT_ACE, // directory only
			AccessMask: ACE4_WRITE_DATA,
			Who:        SpecialOwner,
		},
		{
			Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
			Flag:       ACE4_FILE_INHERIT_ACE | ACE4_DIRECTORY_INHERIT_ACE, // both
			AccessMask: ACE4_EXECUTE,
			Who:        SpecialGroup,
		},
	}}

	// File child: should get ACE 0 (FILE_INHERIT) and ACE 2 (both).
	fileResult := ComputeInheritedACL(parentACL, false, Creator{})
	if fileResult == nil {
		t.Fatal("expected non-nil result for file")
	}
	if len(fileResult.ACEs) != 2 {
		t.Fatalf("expected 2 ACEs for file, got %d", len(fileResult.ACEs))
	}
	if fileResult.ACEs[0].Who != SpecialEveryone {
		t.Errorf("file ACE 0: expected EVERYONE@, got %s", fileResult.ACEs[0].Who)
	}
	if fileResult.ACEs[1].Who != SpecialGroup {
		t.Errorf("file ACE 1: expected GROUP@, got %s", fileResult.ACEs[1].Who)
	}

	// Directory child: should get all three ACEs.
	//   ACE 0 (OI only on parent) → propagates to dir as OI|INHERIT_ONLY so
	//     it does not apply here but reaches file grandchildren
	//     (MS-DTYP §2.5.3.4.1 / Samba calculate_inherited_from_parent).
	//   ACE 1 (CI only) → applies at this dir; CI preserved.
	//   ACE 2 (OI|CI)   → applies and propagates; both bits preserved.
	dirResult := ComputeInheritedACL(parentACL, true, Creator{})
	if dirResult == nil {
		t.Fatal("expected non-nil result for directory")
	}
	if len(dirResult.ACEs) != 3 {
		t.Fatalf("expected 3 ACEs for directory, got %d", len(dirResult.ACEs))
	}
	if dirResult.ACEs[0].Who != SpecialEveryone {
		t.Errorf("dir ACE 0: expected EVERYONE@, got %s", dirResult.ACEs[0].Who)
	}
	wantACE0Flag := uint32(ACE4_FILE_INHERIT_ACE | ACE4_INHERIT_ONLY_ACE)
	if dirResult.ACEs[0].Flag != wantACE0Flag {
		t.Errorf("dir ACE 0 (OI-only on parent): flag=0x%x, want 0x%x (OI|INHERIT_ONLY)",
			dirResult.ACEs[0].Flag, wantACE0Flag)
	}
	if dirResult.ACEs[1].Who != SpecialOwner {
		t.Errorf("dir ACE 1: expected OWNER@, got %s", dirResult.ACEs[1].Who)
	}
	if dirResult.ACEs[2].Who != SpecialGroup {
		t.Errorf("dir ACE 2: expected GROUP@, got %s", dirResult.ACEs[2].Who)
	}
}

func TestComputeInheritedACL_FileInheritClearsAllFlags(t *testing.T) {
	// FILE_INHERIT + DIRECTORY_INHERIT: when inherited by a file,
	// ALL inheritance flags are cleared.
	parentACL := &ACL{
		AutoInherited: true,
		ACEs: []ACE{
			{
				Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Flag:       ACE4_FILE_INHERIT_ACE | ACE4_DIRECTORY_INHERIT_ACE,
				AccessMask: ACE4_READ_DATA,
				Who:        SpecialEveryone,
			},
		},
	}

	result := ComputeInheritedACL(parentACL, false, Creator{})
	if result == nil {
		t.Fatal("expected non-nil result")
	}

	ace := result.ACEs[0]
	// Only INHERITED_ACE should remain.
	expectedFlag := uint32(ACE4_INHERITED_ACE)
	if ace.Flag != expectedFlag {
		t.Errorf("file ACE flag: got 0x%x, want 0x%x (INHERITED_ACE only)", ace.Flag, expectedFlag)
	}
}

func TestComputeInheritedACL_PreservesDenyType(t *testing.T) {
	parentACL := &ACL{ACEs: []ACE{
		{
			Type:       ACE4_ACCESS_DENIED_ACE_TYPE,
			Flag:       ACE4_FILE_INHERIT_ACE,
			AccessMask: ACE4_WRITE_DATA,
			Who:        "alice@example.com",
		},
	}}

	result := ComputeInheritedACL(parentACL, false, Creator{})
	if result == nil {
		t.Fatal("expected non-nil result")
	}

	ace := result.ACEs[0]
	if ace.Type != ACE4_ACCESS_DENIED_ACE_TYPE {
		t.Error("expected DENY type to be preserved in inherited ACE")
	}
	if ace.Who != "alice@example.com" {
		t.Errorf("expected who to be preserved, got %s", ace.Who)
	}
}

func TestPropagateACL_ReplacesInheritedKeepsExplicit(t *testing.T) {
	existingACL := &ACL{ACEs: []ACE{
		// Explicit ACE (no INHERITED_ACE flag).
		{Type: ACE4_ACCESS_DENIED_ACE_TYPE, AccessMask: ACE4_WRITE_DATA, Who: "alice@example.com"},
		// Inherited ACE (will be replaced).
		{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, Flag: ACE4_INHERITED_ACE, AccessMask: ACE4_READ_DATA, Who: SpecialEveryone},
	}}

	newParentACL := &ACL{
		AutoInherited: true,
		ACEs: []ACE{
			{
				Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Flag:       ACE4_FILE_INHERIT_ACE,
				AccessMask: ACE4_READ_DATA | ACE4_EXECUTE,
				Who:        SpecialOwner,
			},
		},
	}

	result := PropagateACL(newParentACL, existingACL, false, Creator{})

	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if len(result.ACEs) != 2 {
		t.Fatalf("expected 2 ACEs (1 explicit + 1 inherited), got %d", len(result.ACEs))
	}

	// First: explicit DENY for alice (preserved).
	if result.ACEs[0].Who != "alice@example.com" || result.ACEs[0].Type != ACE4_ACCESS_DENIED_ACE_TYPE {
		t.Error("expected explicit DENY for alice to be preserved")
	}
	if result.ACEs[0].IsInherited() {
		t.Error("explicit ACE should not have INHERITED_ACE flag")
	}

	// Second: new inherited ALLOW for OWNER@.
	if result.ACEs[1].Who != SpecialOwner || result.ACEs[1].Type != ACE4_ACCESS_ALLOWED_ACE_TYPE {
		t.Error("expected inherited ALLOW for OWNER@")
	}
	if !result.ACEs[1].IsInherited() {
		t.Error("new inherited ACE should have INHERITED_ACE flag")
	}
}

func TestPropagateACL_NilExisting(t *testing.T) {
	parentACL := &ACL{ACEs: []ACE{
		{
			Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
			Flag:       ACE4_FILE_INHERIT_ACE,
			AccessMask: ACE4_READ_DATA,
			Who:        SpecialEveryone,
		},
	}}

	result := PropagateACL(parentACL, nil, false, Creator{})
	if result == nil {
		t.Fatal("expected non-nil result when existing is nil")
	}
	if len(result.ACEs) != 1 {
		t.Fatalf("expected 1 ACE, got %d", len(result.ACEs))
	}
}

func TestPropagateACL_NilParent(t *testing.T) {
	existingACL := &ACL{ACEs: []ACE{
		{Type: ACE4_ACCESS_DENIED_ACE_TYPE, AccessMask: ACE4_WRITE_DATA, Who: "alice@example.com"},
		{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, Flag: ACE4_INHERITED_ACE, AccessMask: ACE4_READ_DATA, Who: SpecialEveryone},
	}}

	result := PropagateACL(nil, existingACL, false, Creator{})

	// Should keep only explicit ACEs (inherited removed since parent is nil).
	if result == nil {
		t.Fatal("expected non-nil result (explicit ACE remains)")
	}
	if len(result.ACEs) != 1 {
		t.Fatalf("expected 1 ACE (explicit only), got %d", len(result.ACEs))
	}
	if result.ACEs[0].Who != "alice@example.com" {
		t.Error("expected explicit ACE to be preserved")
	}
}

func TestPropagateACL_BothNil(t *testing.T) {
	result := PropagateACL(nil, nil, false, Creator{})
	if result != nil {
		t.Error("expected nil result when both parent and existing are nil")
	}
}

func TestPropagateACL_AllInheritedReplaced(t *testing.T) {
	existingACL := &ACL{ACEs: []ACE{
		// Only inherited ACEs.
		{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, Flag: ACE4_INHERITED_ACE, AccessMask: ACE4_READ_DATA, Who: SpecialEveryone},
	}}

	// Parent has no inheritable ACEs for files.
	newParentACL := &ACL{ACEs: []ACE{
		{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, Flag: ACE4_DIRECTORY_INHERIT_ACE, AccessMask: ACE4_READ_DATA, Who: SpecialOwner},
	}}

	// For a file, the DIRECTORY_INHERIT ACE is not inherited.
	result := PropagateACL(newParentACL, existingACL, false, Creator{})
	if result != nil {
		t.Error("expected nil result when all inherited ACEs removed and no new ones")
	}
}

func TestComputeInheritedACL_CreatorOwnerSubstitution_POSIX(t *testing.T) {
	// Parent has a CREATOR_OWNER inheritable placeholder ACE. When inherited
	// onto a file with no Windows SID known, it should resolve to the POSIX
	// form "<uid>@localdomain" frozen at create time.
	parentACL := &ACL{
		AutoInherited: true,
		ACEs: []ACE{
			{
				Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Flag:       ACE4_FILE_INHERIT_ACE | ACE4_INHERIT_ONLY_ACE,
				AccessMask: ACE4_READ_DATA | ACE4_WRITE_DATA,
				Who:        SpecialCreatorOwner,
			},
		},
	}

	result := ComputeInheritedACL(parentACL, false, Creator{UID: 1001})

	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if len(result.ACEs) != 1 {
		t.Fatalf("expected 1 ACE, got %d", len(result.ACEs))
	}

	ace := result.ACEs[0]
	if ace.Who != "1001@localdomain" {
		t.Errorf("expected Who=1001@localdomain, got %q", ace.Who)
	}
	if !ace.IsInherited() {
		t.Error("expected INHERITED_ACE flag set")
	}
	if ace.IsInheritOnly() {
		t.Error("expected INHERIT_ONLY cleared on file child")
	}
}

func TestComputeInheritedACL_CreatorOwnerSubstitution_SID(t *testing.T) {
	// Same scenario but creator has a Windows SID — substitution uses sid:<SID>.
	parentACL := &ACL{
		AutoInherited: true,
		ACEs: []ACE{
			{
				Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Flag:       ACE4_FILE_INHERIT_ACE | ACE4_INHERIT_ONLY_ACE,
				AccessMask: ACE4_READ_DATA,
				Who:        SpecialCreatorOwner,
			},
		},
	}

	result := ComputeInheritedACL(parentACL, false, Creator{UID: 1001, SID: "S-1-5-21-1-2-3-2001"})

	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if len(result.ACEs) != 1 {
		t.Fatalf("expected 1 ACE, got %d", len(result.ACEs))
	}

	ace := result.ACEs[0]
	if ace.Who != "sid:S-1-5-21-1-2-3-2001" {
		t.Errorf("expected Who=sid:S-1-5-21-1-2-3-2001, got %q", ace.Who)
	}
	if !ace.IsInherited() {
		t.Error("expected INHERITED_ACE flag set")
	}
}

func TestComputeInheritedACL_CreatorGroupSubstitution_POSIX(t *testing.T) {
	parentACL := &ACL{ACEs: []ACE{
		{
			Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
			Flag:       ACE4_FILE_INHERIT_ACE,
			AccessMask: ACE4_READ_DATA,
			Who:        SpecialCreatorGroup,
		},
	}}

	result := ComputeInheritedACL(parentACL, false, Creator{UID: 1001, GID: 2002})

	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if len(result.ACEs) != 1 {
		t.Fatalf("expected 1 ACE, got %d", len(result.ACEs))
	}

	ace := result.ACEs[0]
	if ace.Who != "2002@localdomain" {
		t.Errorf("expected Who=2002@localdomain (creator GID), got %q", ace.Who)
	}
}

func TestComputeInheritedACL_CreatorGroupSubstitution_SID(t *testing.T) {
	parentACL := &ACL{ACEs: []ACE{
		{
			Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
			Flag:       ACE4_FILE_INHERIT_ACE,
			AccessMask: ACE4_READ_DATA,
			Who:        SpecialCreatorGroup,
		},
	}}

	result := ComputeInheritedACL(parentACL, false, Creator{UID: 1001, GID: 2002, SID: "S-1-5-21-1-2-3-2001"})

	if result == nil {
		t.Fatal("expected non-nil result")
	}

	ace := result.ACEs[0]
	// Group substitution uses creator.SID for Windows form when available.
	// "SpecialCreatorGroup → creator.GID/SID" — the SID branch
	// embeds the creator's SID. The same SID is used because the SD only
	// carries one identifier per creator (no separate group SID is plumbed).
	if ace.Who != "sid:S-1-5-21-1-2-3-2001" {
		t.Errorf("expected Who=sid:S-1-5-21-1-2-3-2001, got %q", ace.Who)
	}
}

func TestComputeInheritedACL_AutoInheritedPropagation(t *testing.T) {
	// Parent SD has SE_DACL_AUTO_INHERITED set + an inheritable ACE.
	// Per MS-DTYP §2.5.3.4.2, the computed child SD must also have
	// SE_DACL_AUTO_INHERITED set (Samba parity: set_inherited_sd).
	parentACL := &ACL{
		AutoInherited: true,
		ACEs: []ACE{
			{
				Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Flag:       ACE4_FILE_INHERIT_ACE,
				AccessMask: 0xFF,
				Who:        SpecialEveryone,
			},
		},
	}

	result := ComputeInheritedACL(parentACL, false, Creator{})

	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if !result.AutoInherited {
		t.Error("expected AutoInherited=true propagated from parent")
	}
}

func TestComputeInheritedACL_ParentNotAutoInherited_ChildNotAutoInherited(t *testing.T) {
	parentACL := &ACL{
		AutoInherited: false,
		ACEs: []ACE{
			{
				Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Flag:       ACE4_FILE_INHERIT_ACE,
				AccessMask: 0xFF,
				Who:        SpecialEveryone,
			},
		},
	}

	result := ComputeInheritedACL(parentACL, false, Creator{})

	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.AutoInherited {
		t.Error("expected AutoInherited=false when parent has it cleared")
	}
}

func TestComputeInheritedACL_ProtectedNotPropagated(t *testing.T) {
	// Parent has Protected + AutoInherited set. Only AutoInherited
	// propagates; Protected is per-SD and blocks inheritance, never
	// itself inherited onto the child.
	parentACL := &ACL{
		Protected:     true,
		AutoInherited: true,
		ACEs: []ACE{
			{
				Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Flag:       ACE4_FILE_INHERIT_ACE,
				AccessMask: 0xFF,
				Who:        SpecialEveryone,
			},
		},
	}

	result := ComputeInheritedACL(parentACL, false, Creator{})

	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Protected {
		t.Error("expected Protected=false on child (Protected is per-SD, never inherited)")
	}
	if !result.AutoInherited {
		t.Error("expected AutoInherited=true on child")
	}
}

func TestPropagateACL_AutoInheritedPropagation(t *testing.T) {
	// PropagateACL must also propagate AutoInherited from parent onto
	// the recomputed combined child SD.
	parentACL := &ACL{
		AutoInherited: true,
		ACEs: []ACE{
			{
				Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Flag:       ACE4_FILE_INHERIT_ACE,
				AccessMask: 0xFF,
				Who:        SpecialEveryone,
			},
		},
	}
	existingACL := &ACL{
		ACEs: []ACE{
			{Type: ACE4_ACCESS_DENIED_ACE_TYPE, AccessMask: ACE4_WRITE_DATA, Who: "alice@example.com"},
		},
	}

	result := PropagateACL(parentACL, existingACL, false, Creator{})

	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if !result.AutoInherited {
		t.Error("expected AutoInherited=true propagated through PropagateACL")
	}
}

func TestComputeInheritedACL_ParentAutoInheritedButNoInheritableACEs_ReturnsNil(t *testing.T) {
	// Parent has AutoInherited set but no inheritable ACEs. Existing
	// semantics: return nil ("no ACL to set on child" — child gets a
	// synthesized SD per existing creation path). The AutoInherited bit
	// does not synthesize an empty ACL.
	parentACL := &ACL{
		AutoInherited: true,
		ACEs: []ACE{
			{
				Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Flag:       0, // no inherit flags
				AccessMask: ACE4_READ_DATA,
				Who:        SpecialEveryone,
			},
		},
	}

	result := ComputeInheritedACL(parentACL, false, Creator{})
	if result != nil {
		t.Errorf("expected nil when no inheritable ACEs even if parent has AutoInherited, got %+v", result)
	}
}

// buildParentACLWithFlags constructs a parent ACL with a single inheritable
// ACE for the inheritance conformance matrices below. It mirrors the
// smbtorture parent SD shape used by `test_inheritance_flags` and
// `test_inheritance`: one ALLOW ACE granting WRITE_DATA to EVERYONE@ with
// the supplied ACE inheritance flags. controlFlags carries the bool pair
// (autoInherited, protected) for the parent SD; parentACEHasInheritedBit
// optionally pre-sets ACE4_INHERITED_ACE on the parent ACE itself (matching
// smbtorture's "i&8" iteration bit).
func buildParentACLWithFlags(autoInherited, protected bool, aceFlags uint32, parentACEHasInheritedBit bool) *ACL {
	flag := aceFlags
	if parentACEHasInheritedBit {
		flag |= ACE4_INHERITED_ACE
	}
	return &ACL{
		AutoInherited: autoInherited,
		Protected:     protected,
		ACEs: []ACE{
			{
				Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Flag:       flag,
				AccessMask: ACE4_WRITE_DATA,
				Who:        SpecialEveryone,
			},
		},
	}
}

// TestComputeInheritedACL_InheritanceFlagsMatrix mirrors smbtorture's
// `test_inheritance_flags` from source4/torture/smb2/acls.c. It walks a
// 16-case matrix over the parent SD-level control flag combinations
// affecting inheritance: AutoInherited, AutoInheritReq (request-only, not
// stored on the in-memory model — skipped), Protected, and whether the
// parent ACE itself carries ACE4_INHERITED_ACE.
//
// Bit layout (matches smbtorture i ∈ 0..15):
//
//	i&1 → parent SD.AutoInherited
//	i&2 → AutoInheritReq (request flag; not on our model — ignored)
//	i&4 → parent SD.Protected
//	i&8 → parent ACE has ACE4_INHERITED_ACE pre-set
//
// Key invariant under test (MS-DTYP §2.5.3.4.2 / Samba `set_inherited_sd`
// + tflags table in source4/torture/smb2/acls.c): the child ACE has
// ACE4_INHERITED_ACE iff parent.AutoInherited (after canonicalization).
// The parent ACE's pre-existing INHERITED_ACE bit is NOT propagated
// independently — Samba's tflags shows it is set on the child ONLY when
// the parent SD has both AUTO_INHERITED and AUTO_INHERIT_REQ set, which
// canonicalize down to parent.AutoInherited.
//
// After #521 PR 7 (Bug J), the implementation enforces this directly:
// ComputeInheritedACL sets the bit only when parent.AutoInherited.
func TestComputeInheritedACL_InheritanceFlagsMatrix(t *testing.T) {
	for i := 0; i < 16; i++ {
		i := i
		autoInherited := (i & 1) != 0
		protected := (i & 4) != 0
		aceHasInheritedBit := (i & 8) != 0
		// Bug J: child gets INHERITED_ACE iff parent.AutoInherited.
		// Independent of whether the parent ACE pre-carried the bit
		// (aceHasInheritedBit only feeds the parent shape for building
		// the input; it does not influence the expected child outcome).
		expectChildInherited := autoInherited

		for _, tc := range []struct {
			name        string
			isDirectory bool
		}{
			{"file", false},
			{"dir", true},
		} {
			tc := tc
			t.Run(formatMatrixName(i, tc.name), func(t *testing.T) {
				parent := buildParentACLWithFlags(
					autoInherited,
					protected,
					ACE4_FILE_INHERIT_ACE|ACE4_DIRECTORY_INHERIT_ACE,
					aceHasInheritedBit,
				)

				result := ComputeInheritedACL(parent, tc.isDirectory, Creator{})
				if result == nil {
					t.Fatalf("i=%d %s: expected non-nil result (parent OI|CI grants both file and dir inherit)", i, tc.name)
				}
				if len(result.ACEs) != 1 {
					t.Fatalf("i=%d %s: expected 1 child ACE, got %d", i, tc.name, len(result.ACEs))
				}

				gotInherited := result.ACEs[0].IsInherited()
				if gotInherited != expectChildInherited {
					t.Errorf("i=%d %s: child INHERITED_ACE=%v, want %v (parent AI=%v ace.I=%v)",
						i, tc.name, gotInherited, expectChildInherited, autoInherited, aceHasInheritedBit)
				}
				// Trustee assertion: parent uses non-CREATOR principal
				// (EVERYONE@), so no substitution happens.
				if result.ACEs[0].Who != SpecialEveryone {
					t.Errorf("i=%d %s: child ACE Who=%q, want %q (no substitution for non-CREATOR principal)",
						i, tc.name, result.ACEs[0].Who, SpecialEveryone)
				}

				// AutoInherited propagation (already correct under P6-2,
				// asserted here as a non-skipped sanity check).
				if result.AutoInherited != autoInherited {
					t.Errorf("i=%d %s: child SD.AutoInherited=%v, want %v",
						i, tc.name, result.AutoInherited, autoInherited)
				}
				// Protected is per-SD; never inherited.
				if result.Protected {
					t.Errorf("i=%d %s: child SD.Protected must always be false, got true", i, tc.name)
				}
			})
		}
	}
}

func formatMatrixName(i int, kind string) string {
	return fmt.Sprintf("i=%02d_%s", i, kind)
}

// TestComputeInheritedACL_InheritanceACEFlagMatrix mirrors smbtorture's
// `test_inheritance` from source4/torture/smb2/acls.c. It walks the 16
// combinations of inheritance-related ACE flags on a single parent ACE
// (OI, CI, NP, IO) and asserts the resulting child ACE's flag layout
// against the MS-DTYP §2.5.3.4 truth table (Samba reference:
// source3/lib/util_sd.c::sec_ace_inherit).
//
// To isolate this matrix from the SD-control-flag bug exercised in
// TestComputeInheritedACL_InheritanceFlagsMatrix, the parent SD is set
// with AutoInherited=true throughout. The child ACE is therefore always
// expected to carry ACE4_INHERITED_ACE when it exists.
//
// Dir-child rows 1 and 9 (parent OI-only / IO|OI) produce a dir-child
// ACE with OI|INHERIT_ONLY so the dir does not gain the right itself but
// continues to propagate to file grandchildren. This was Bug C, fixed in
// #521 PR 3.
func TestComputeInheritedACL_InheritanceACEFlagMatrix(t *testing.T) {
	const (
		OI = ACE4_FILE_INHERIT_ACE
		CI = ACE4_DIRECTORY_INHERIT_ACE
		NP = ACE4_NO_PROPAGATE_INHERIT_ACE
		IO = ACE4_INHERIT_ONLY_ACE
		I  = ACE4_INHERITED_ACE
	)

	// hasACE encodes the expected child outcome:
	//   present=false → no inherited ACE (ComputeInheritedACL returns nil)
	//   present=true  → exactly one inherited ACE with `flags` (INHERITED_ACE
	//                   bit included when AutoInherited propagates).
	type expected struct {
		present bool
		flags   uint32
	}

	type row struct {
		parentFlags uint32
		file        expected
		dir         expected
		skipDirPR   string // non-empty → skip the dir subtest with this reason
	}

	rows := []row{
		/* 0  none           */ {0, expected{}, expected{}, ""},
		/* 1  OI             */ {OI, expected{true, I}, expected{true, OI | IO | I}, ""},
		/* 2  CI             */ {CI, expected{}, expected{true, CI | I}, ""},
		/* 3  OI|CI          */ {OI | CI, expected{true, I}, expected{true, OI | CI | I}, ""},
		/* 4  NP             */ {NP, expected{}, expected{}, ""},
		/* 5  NP|OI          */ {NP | OI, expected{true, I}, expected{}, ""},
		/* 6  NP|CI          */ {NP | CI, expected{}, expected{true, I}, ""},
		/* 7  NP|OI|CI       */ {NP | OI | CI, expected{true, I}, expected{true, I}, ""},
		/* 8  IO             */ {IO, expected{}, expected{}, ""},
		/* 9  IO|OI          */ {IO | OI, expected{true, I}, expected{true, OI | IO | I}, ""},
		/* 10 IO|CI          */ {IO | CI, expected{}, expected{true, CI | I}, ""},
		/* 11 IO|OI|CI       */ {IO | OI | CI, expected{true, I}, expected{true, OI | CI | I}, ""},
		/* 12 IO|NP          */ {IO | NP, expected{}, expected{}, ""},
		/* 13 IO|NP|OI       */ {IO | NP | OI, expected{true, I}, expected{}, ""},
		/* 14 IO|NP|CI       */ {IO | NP | CI, expected{}, expected{true, I}, ""},
		/* 15 IO|NP|OI|CI    */ {IO | NP | OI | CI, expected{true, I}, expected{true, I}, ""},
	}

	for n, r := range rows {
		n, r := n, r
		t.Run(fmt.Sprintf("row_%02d_file", n), func(t *testing.T) {
			parent := buildParentACLWithFlags(true /*AutoInherited*/, false, r.parentFlags, false)
			result := ComputeInheritedACL(parent, false /*isDir*/, Creator{})
			assertInheritedFlags(t, n, "file", result, r.file)
		})
		t.Run(fmt.Sprintf("row_%02d_dir", n), func(t *testing.T) {
			if r.skipDirPR != "" {
				t.Skip(r.skipDirPR)
			}
			parent := buildParentACLWithFlags(true /*AutoInherited*/, false, r.parentFlags, false)
			result := ComputeInheritedACL(parent, true /*isDir*/, Creator{})
			assertInheritedFlags(t, n, "dir", result, r.dir)
		})
	}
}

func assertInheritedFlags(t *testing.T, row int, kind string, result *ACL, want struct {
	present bool
	flags   uint32
},
) {
	t.Helper()
	if !want.present {
		if result != nil && len(result.ACEs) > 0 {
			t.Errorf("row %d %s: expected no inherited ACE, got %d ACE(s), first flags=0x%x",
				row, kind, len(result.ACEs), result.ACEs[0].Flag)
		}
		return
	}
	if result == nil {
		t.Fatalf("row %d %s: expected 1 inherited ACE, got nil ACL", row, kind)
	}
	if len(result.ACEs) != 1 {
		t.Fatalf("row %d %s: expected 1 inherited ACE, got %d", row, kind, len(result.ACEs))
	}
	if result.ACEs[0].Flag != want.flags {
		t.Errorf("row %d %s: child ACE flags=0x%x, want 0x%x",
			row, kind, result.ACEs[0].Flag, want.flags)
	}
	// Non-CREATOR principal: must be preserved exactly. The matrix uses
	// EVERYONE@ via buildParentACLWithFlags.
	if result.ACEs[0].Who != SpecialEveryone {
		t.Errorf("row %d %s: child ACE Who=%q, want %q (no substitution for non-CREATOR)",
			row, kind, result.ACEs[0].Who, SpecialEveryone)
	}
}

// TestComputeInheritedACL_CreatorMatrix_DirChild walks the CREATOR-specific
// inheritance shapes on a directory child, checking BOTH per-ACE trustee
// and flags. Bug H + Bug I from the post-PR #524 investigation:
//   - OI-only on dir (applies=false): single preserved ACE with CREATOR
//     trustee KEPT (no substitution), flag OI|IO+INHERITED.
//   - CI on dir + CREATOR (applies=true): TWO ACEs. Resolved sibling has
//     all inheritance flags CLEARED (flag = INHERITED only), trustee
//     substituted. Preserved has CREATOR kept, flag = parent OI/CI
//     bits + IO + INHERITED.
func TestComputeInheritedACL_CreatorMatrix_DirChild(t *testing.T) {
	const (
		OI = ACE4_FILE_INHERIT_ACE
		CI = ACE4_DIRECTORY_INHERIT_ACE
		IO = ACE4_INHERIT_ONLY_ACE
		I  = ACE4_INHERITED_ACE
	)

	type expectedACE struct {
		who  string
		flag uint32
	}
	cases := []struct {
		name        string
		parentFlags uint32
		expect      []expectedACE
	}{
		{
			name:        "OI-only / applies=false",
			parentFlags: OI,
			// Bug H: principal preserved, NOT substituted.
			expect: []expectedACE{
				{who: SpecialCreatorOwner, flag: OI | IO | I},
			},
		},
		{
			name:        "CI-only / applies=true",
			parentFlags: CI,
			// Bug I: resolved sibling flag = INHERITED only (all
			// inheritance bits cleared); preserved keeps CREATOR.
			expect: []expectedACE{
				{who: "1001@localdomain", flag: I},
				{who: SpecialCreatorOwner, flag: CI | IO | I},
			},
		},
		{
			name:        "OI|CI / applies=true",
			parentFlags: OI | CI,
			expect: []expectedACE{
				{who: "1001@localdomain", flag: I},
				{who: SpecialCreatorOwner, flag: OI | CI | IO | I},
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			parent := &ACL{
				AutoInherited: true,
				ACEs: []ACE{
					{
						Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
						Flag:       tc.parentFlags,
						AccessMask: ACE4_WRITE_DATA,
						Who:        SpecialCreatorOwner,
					},
				},
			}
			result := ComputeInheritedACL(parent, true, Creator{UID: 1001})
			if result == nil {
				t.Fatal("expected non-nil result")
			}
			if len(result.ACEs) != len(tc.expect) {
				t.Fatalf("got %d ACEs, want %d (%v)", len(result.ACEs), len(tc.expect), result.ACEs)
			}
			for i, want := range tc.expect {
				got := result.ACEs[i]
				if got.Who != want.who {
					t.Errorf("ACE[%d].Who=%q, want %q", i, got.Who, want.who)
				}
				if got.Flag != want.flag {
					t.Errorf("ACE[%d].Flag=0x%x, want 0x%x", i, got.Flag, want.flag)
				}
			}
		})
	}
}

// TestComputeInheritedACL_CreatorOwnerCI_DualEmit_Dir asserts that a parent
// ACE with CREATOR_OWNER + OI|CI emits TWO ACEs onto a directory child:
//  1. The resolved owner ACE that applies at the new dir ONLY. Per Samba
//     desc_expand_generic (libcli/security/create_descriptor.c), the
//     resolved sibling has ALL inheritance flags cleared — only the
//     INHERITED_ACE bit may be present. It does NOT propagate further;
//     propagation to grandchildren happens via the preserved CREATOR.
//  2. The preserved CREATOR_OWNER ACE with CI|OI|INHERIT_ONLY so
//     grandchild directories continue to substitute against THEIR own
//     creator.
//
// Mirrors Samba calculate_inherited_from_parent (libcli/security/
// create_descriptor.c).
func TestComputeInheritedACL_CreatorOwnerCI_DualEmit_Dir(t *testing.T) {
	parentACL := &ACL{
		AutoInherited: true,
		ACEs: []ACE{
			{
				Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Flag:       ACE4_FILE_INHERIT_ACE | ACE4_DIRECTORY_INHERIT_ACE,
				AccessMask: ACE4_WRITE_DATA,
				Who:        SpecialCreatorOwner,
			},
		},
	}

	result := ComputeInheritedACL(parentACL, true, Creator{UID: 1001})
	if result == nil {
		t.Fatal("expected non-nil result for dir child")
	}
	if len(result.ACEs) != 2 {
		t.Fatalf("expected 2 ACEs (resolved + preserved CREATOR), got %d", len(result.ACEs))
	}

	// ACE 0: resolved owner — Samba clears ALL inheritance flags on the
	// resolved sibling; only INHERITED_ACE may remain. The resolved ACE
	// applies at this object only.
	resolved := result.ACEs[0]
	if resolved.Who != "1001@localdomain" {
		t.Errorf("resolved ACE: expected Who=1001@localdomain, got %q", resolved.Who)
	}
	wantResolvedFlag := uint32(ACE4_INHERITED_ACE)
	if resolved.Flag != wantResolvedFlag {
		t.Errorf("resolved ACE: flag=0x%x, want 0x%x (INHERITED only — Samba clears inheritance bits on resolved sibling)", resolved.Flag, wantResolvedFlag)
	}

	// ACE 1: preserved CREATOR_OWNER — CI|OI|IO + INHERITED.
	preserved := result.ACEs[1]
	if preserved.Who != SpecialCreatorOwner {
		t.Errorf("preserved ACE: expected Who=%q, got %q", SpecialCreatorOwner, preserved.Who)
	}
	wantPreservedFlag := uint32(ACE4_FILE_INHERIT_ACE | ACE4_DIRECTORY_INHERIT_ACE | ACE4_INHERIT_ONLY_ACE | ACE4_INHERITED_ACE)
	if preserved.Flag != wantPreservedFlag {
		t.Errorf("preserved ACE: flag=0x%x, want 0x%x (OI|CI|IO|INHERITED)", preserved.Flag, wantPreservedFlag)
	}
	if preserved.Type != ACE4_ACCESS_ALLOWED_ACE_TYPE {
		t.Errorf("preserved ACE: type=%d, want ALLOW", preserved.Type)
	}
	if preserved.AccessMask != ACE4_WRITE_DATA {
		t.Errorf("preserved ACE: mask=0x%x, want 0x%x", preserved.AccessMask, ACE4_WRITE_DATA)
	}
}

// TestComputeInheritedACL_CreatorGroupCI_DualEmit_Dir is the CREATOR_GROUP
// counterpart of the test above. CI-only on parent (no OI) — preserved
// ACE should not include OI.
func TestComputeInheritedACL_CreatorGroupCI_DualEmit_Dir(t *testing.T) {
	parentACL := &ACL{
		AutoInherited: true,
		ACEs: []ACE{
			{
				Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Flag:       ACE4_DIRECTORY_INHERIT_ACE,
				AccessMask: ACE4_READ_DATA,
				Who:        SpecialCreatorGroup,
			},
		},
	}

	result := ComputeInheritedACL(parentACL, true, Creator{UID: 1001, GID: 2002})
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if len(result.ACEs) != 2 {
		t.Fatalf("expected 2 ACEs, got %d", len(result.ACEs))
	}

	resolved := result.ACEs[0]
	if resolved.Who != "2002@localdomain" {
		t.Errorf("resolved ACE: expected Who=2002@localdomain, got %q", resolved.Who)
	}
	// Resolved sibling has ALL inheritance flags cleared (Samba
	// desc_expand_generic); only INHERITED_ACE remains.
	wantResolvedFlag := uint32(ACE4_INHERITED_ACE)
	if resolved.Flag != wantResolvedFlag {
		t.Errorf("resolved ACE: flag=0x%x, want 0x%x (INHERITED only)", resolved.Flag, wantResolvedFlag)
	}

	preserved := result.ACEs[1]
	if preserved.Who != SpecialCreatorGroup {
		t.Errorf("preserved ACE: expected Who=%q, got %q", SpecialCreatorGroup, preserved.Who)
	}
	// No OI on parent => no OI on preserved ACE.
	wantPreservedFlag := uint32(ACE4_DIRECTORY_INHERIT_ACE | ACE4_INHERIT_ONLY_ACE | ACE4_INHERITED_ACE)
	if preserved.Flag != wantPreservedFlag {
		t.Errorf("preserved ACE: flag=0x%x, want 0x%x (CI|IO|INHERITED)", preserved.Flag, wantPreservedFlag)
	}
}

// TestComputeInheritedACL_CreatorOwner_FileChild_NoDualEmit verifies that
// file children are leaves and never receive the preserved CREATOR ACE.
func TestComputeInheritedACL_CreatorOwner_FileChild_NoDualEmit(t *testing.T) {
	parentACL := &ACL{
		AutoInherited: true,
		ACEs: []ACE{
			{
				Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Flag:       ACE4_FILE_INHERIT_ACE | ACE4_DIRECTORY_INHERIT_ACE,
				AccessMask: ACE4_WRITE_DATA,
				Who:        SpecialCreatorOwner,
			},
		},
	}

	result := ComputeInheritedACL(parentACL, false, Creator{UID: 1001})
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if len(result.ACEs) != 1 {
		t.Fatalf("expected exactly 1 ACE on file child (no dual emit), got %d", len(result.ACEs))
	}
	if result.ACEs[0].Who != "1001@localdomain" {
		t.Errorf("expected resolved owner, got Who=%q", result.ACEs[0].Who)
	}
}

// TestComputeInheritedACL_CreatorOwner_OIOnly_NoDualEmit_Dir verifies that
// when the parent ACE has OI only (no CI), the dir child receives a SINGLE
// preserved ACE as OI|INHERIT_ONLY with the principal kept as
// CREATOR_OWNER. The ACE does not apply at this dir; it propagates to file
// grandchildren and substitution happens at THAT grandchild's create time,
// not now. This is Bug H from #521 PR 5 — previously we substituted the
// principal here, which leaked the dir owner identity onto the
// grandchild's "creator".
func TestComputeInheritedACL_CreatorOwner_OIOnlyDir_PreservesPrincipal(t *testing.T) {
	parentACL := &ACL{
		AutoInherited: true,
		ACEs: []ACE{
			{
				Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Flag:       ACE4_FILE_INHERIT_ACE,
				AccessMask: ACE4_WRITE_DATA,
				Who:        SpecialCreatorOwner,
			},
		},
	}

	result := ComputeInheritedACL(parentACL, true, Creator{UID: 1001})
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if len(result.ACEs) != 1 {
		t.Fatalf("expected exactly 1 ACE (no dual emit without CI), got %d", len(result.ACEs))
	}
	ace := result.ACEs[0]
	wantFlag := uint32(ACE4_FILE_INHERIT_ACE | ACE4_INHERIT_ONLY_ACE | ACE4_INHERITED_ACE)
	if ace.Flag != wantFlag {
		t.Errorf("expected flag 0x%x (OI|IO|INHERITED), got 0x%x", wantFlag, ace.Flag)
	}
	// Principal MUST stay CREATOR_OWNER (Bug H fix). Substitution is
	// deferred to grandchild create time.
	if ace.Who != SpecialCreatorOwner {
		t.Errorf("expected preserved CreatorOwner@, got Who=%q (Bug H regression)", ace.Who)
	}
}

// TestComputeInheritedACL_MaxACECount_CapEnforced verifies that the
// running result of ComputeInheritedACL never exceeds MaxACECount even
// when the parent has many inheritable ACEs and a subset trigger
// CREATOR dual-emission. FIFO truncation rule: earlier-in-parent ACEs
// are preserved over later ones (matches Samba behavior under cap
// pressure).
func TestComputeInheritedACL_MaxACECount_CapEnforced(t *testing.T) {
	// Build a parent ACL with MaxACECount+5 inheritable ACEs. Sprinkle
	// CREATOR_OWNER + CI entries (which dual-emit on a dir child) so
	// the cumulative resolved+preserved count would explode past
	// MaxACECount if the cap were not enforced across both append
	// paths. The first ACE is a uniquely-named "first@example.com"
	// sentinel so we can assert FIFO preservation.
	// Use a distinct per-index bit pattern in AccessMask so we can
	// verify FIFO preservation regardless of CREATOR substitution
	// rewriting the Who field. Bit 26 (0x04000000) is a reserved bit
	// outside the GENERIC_* and MAXIMUM_ALLOWED / ACCESS_SYSTEM_SECURITY
	// ranges, so it passes through ExpandGenericMask unchanged. The low
	// bits carry the parent index. This way:
	//   - resolved CREATOR ACEs keep their (index-tagged) mask
	//   - preserved CREATOR ACEs likewise keep the tagged mask
	//   - we can detect leak of late-parent ACEs by mask alone
	tag := func(i int) uint32 { return uint32(0x04000000) | uint32(i) }

	aces := make([]ACE, 0, MaxACECount+5)
	aces = append(aces, ACE{
		Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
		Flag:       ACE4_FILE_INHERIT_ACE | ACE4_DIRECTORY_INHERIT_ACE,
		AccessMask: tag(0),
		Who:        "first@example.com",
	})
	for i := 1; i < MaxACECount+5; i++ {
		if i%3 == 0 {
			// CREATOR_OWNER + CI => dual-emit on dir child.
			aces = append(aces, ACE{
				Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Flag:       ACE4_DIRECTORY_INHERIT_ACE,
				AccessMask: tag(i),
				Who:        SpecialCreatorOwner,
			})
		} else {
			aces = append(aces, ACE{
				Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Flag:       ACE4_DIRECTORY_INHERIT_ACE,
				AccessMask: tag(i),
				Who:        fmt.Sprintf("user%d@example.com", i),
			})
		}
	}
	parentACL := &ACL{ACEs: aces}

	result := ComputeInheritedACL(parentACL, true, Creator{UID: 1001})
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if len(result.ACEs) > MaxACECount {
		t.Fatalf("result ACE count %d exceeds MaxACECount %d", len(result.ACEs), MaxACECount)
	}
	// We expect the cap to be saturated given the input size.
	if len(result.ACEs) != MaxACECount {
		t.Errorf("expected result saturated at MaxACECount=%d, got %d", MaxACECount, len(result.ACEs))
	}

	// FIFO: the first parent ACE must be the first child ACE (tag 0).
	if result.ACEs[0].AccessMask != tag(0) || result.ACEs[0].Who != "first@example.com" {
		t.Errorf("FIFO preservation broken: expected first ACE tag=0x%x Who=first@example.com, got tag=0x%x Who=%q",
			tag(0), result.ACEs[0].AccessMask, result.ACEs[0].Who)
	}

	// Compute the highest parent index whose tag appears in the result.
	// Under FIFO truncation, no late-parent ACE should leak past the
	// earlier ACEs we had room for.
	resultTags := make(map[uint32]bool, len(result.ACEs))
	for _, a := range result.ACEs {
		resultTags[a.AccessMask] = true
	}
	// The last parent ACE's tag must NOT be present.
	if resultTags[tag(len(aces)-1)] {
		t.Errorf("FIFO violation: last parent ACE (tag=0x%x) leaked into truncated child",
			tag(len(aces)-1))
	}
	// Sanity: tag(0) (the first parent ACE) MUST be present.
	if !resultTags[tag(0)] {
		t.Errorf("FIFO violation: first parent ACE (tag=0x%x) missing from truncated child", tag(0))
	}
}

// TestComputeInheritedACL_GenericExpansion_File asserts that a parent ACE
// carrying GENERIC_ALL inherits onto a file child with the generic bit
// expanded to FILE_ALL_ACCESS (0x001f01ff) — mirroring Samba
// desc_expand_generic (libcli/security/create_descriptor.c). Regression
// guard for smbtorture acls INHERITANCE / INHERITFLAGS / SDFLAGSVSCHOWN,
// which previously observed 0x000e0002 because the generic bit leaked
// onto the child's effective DACL.
func TestComputeInheritedACL_GenericExpansion_File(t *testing.T) {
	parentACL := &ACL{
		AutoInherited: true,
		ACEs: []ACE{
			{
				Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Flag:       ACE4_FILE_INHERIT_ACE,
				AccessMask: 0x10000000, // GENERIC_ALL
				Who:        SpecialEveryone,
			},
		},
	}

	result := ComputeInheritedACL(parentACL, false, Creator{})
	if result == nil {
		t.Fatal("expected non-nil result for file child")
	}
	if len(result.ACEs) != 1 {
		t.Fatalf("expected 1 ACE, got %d", len(result.ACEs))
	}
	const wantMask uint32 = 0x001f01ff // FILE_ALL_ACCESS
	if got := result.ACEs[0].AccessMask; got != wantMask {
		t.Errorf("AccessMask=0x%08x, want 0x%08x (FILE_ALL_ACCESS)", got, wantMask)
	}
	if result.ACEs[0].AccessMask&0xf0000000 != 0 {
		t.Errorf("generic bits leaked into effective ACE: 0x%08x", result.ACEs[0].AccessMask)
	}
}

// TestComputeInheritedACL_GenericExpansion_DirPreservesInheritOnly verifies
// that on a directory child, the resolved sibling (effective) gets generic
// bits expanded, while the preserved CREATOR ACE (INHERIT_ONLY) retains its
// raw generic mask so expansion fires later against the eventual leaf's
// GenericMapping.
func TestComputeInheritedACL_GenericExpansion_DirPreservesInheritOnly(t *testing.T) {
	parentACL := &ACL{
		AutoInherited: true,
		ACEs: []ACE{
			{
				Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Flag:       ACE4_FILE_INHERIT_ACE | ACE4_DIRECTORY_INHERIT_ACE,
				AccessMask: 0x10000000, // GENERIC_ALL
				Who:        SpecialCreatorOwner,
			},
		},
	}

	result := ComputeInheritedACL(parentACL, true, Creator{UID: 1001})
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if len(result.ACEs) != 2 {
		t.Fatalf("expected 2 ACEs (resolved + preserved CREATOR), got %d", len(result.ACEs))
	}

	resolved := result.ACEs[0]
	if resolved.Flag&ACE4_INHERIT_ONLY_ACE != 0 {
		t.Errorf("resolved sibling unexpectedly INHERIT_ONLY (flag=0x%x)", resolved.Flag)
	}
	const wantResolved uint32 = 0x001f01ff // FILE_ALL_ACCESS
	if resolved.AccessMask != wantResolved {
		t.Errorf("resolved AccessMask=0x%08x, want 0x%08x", resolved.AccessMask, wantResolved)
	}

	preserved := result.ACEs[1]
	if preserved.Flag&ACE4_INHERIT_ONLY_ACE == 0 {
		t.Errorf("preserved ACE missing INHERIT_ONLY (flag=0x%x)", preserved.Flag)
	}
	if preserved.AccessMask != 0x10000000 {
		t.Errorf("preserved AccessMask=0x%08x, want raw GENERIC_ALL preserved for grandchild expansion", preserved.AccessMask)
	}
}

// TestComputeInheritedACL_GenericExpansion_NonGenericPassesThrough asserts
// that ACEs without generic bits are unchanged by the post-loop expansion
// pass.
func TestComputeInheritedACL_GenericExpansion_NonGenericPassesThrough(t *testing.T) {
	const explicitMask = ACE4_READ_DATA | ACE4_WRITE_DATA | ACE4_READ_ATTRIBUTES
	parentACL := &ACL{
		AutoInherited: true,
		ACEs: []ACE{
			{
				Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Flag:       ACE4_FILE_INHERIT_ACE,
				AccessMask: explicitMask,
				Who:        SpecialEveryone,
			},
		},
	}

	result := ComputeInheritedACL(parentACL, false, Creator{})
	if result == nil || len(result.ACEs) != 1 {
		t.Fatalf("expected 1 ACE, got %+v", result)
	}
	if result.ACEs[0].AccessMask != explicitMask {
		t.Errorf("non-generic AccessMask mutated: got 0x%08x, want 0x%08x",
			result.ACEs[0].AccessMask, explicitMask)
	}
}

// TestComputeInheritedACL_GenericExpansion_Case3PreservedSkipsExpand covers
// the case where a parent OI-only ACE on a non-CREATOR principal lands on a
// dir child: ComputeInheritedACL emits a single preserved INHERIT_ONLY ACE
// (no resolved sibling) with the parent's raw AccessMask intact, so a future
// grandchild file inherits the same generic bits and expands them against
// the leaf's GenericMapping at that step.
//
// The post-loop expansion pass must skip this ACE because it is INHERIT_ONLY.
// Regression guard against any refactor that clears INHERIT_ONLY on the
// Case-3 preserved emission path.
func TestComputeInheritedACL_GenericExpansion_Case3PreservedSkipsExpand(t *testing.T) {
	parentACL := &ACL{
		AutoInherited: true,
		ACEs: []ACE{
			{
				Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Flag:       ACE4_FILE_INHERIT_ACE, // OI only — does not apply at dir child
				AccessMask: 0x10000000,            // GENERIC_ALL
				Who:        "user@example.com",
			},
		},
	}

	result := ComputeInheritedACL(parentACL, true, Creator{})
	if result == nil {
		t.Fatal("expected non-nil result for dir child with OI-only parent ACE")
	}
	if len(result.ACEs) != 1 {
		t.Fatalf("expected 1 preserved INHERIT_ONLY ACE, got %d", len(result.ACEs))
	}
	preserved := result.ACEs[0]
	if preserved.Flag&ACE4_INHERIT_ONLY_ACE == 0 {
		t.Errorf("Case-3 preserved ACE missing INHERIT_ONLY (flag=0x%x)", preserved.Flag)
	}
	if preserved.AccessMask != 0x10000000 {
		t.Errorf("Case-3 AccessMask=0x%08x, want raw GENERIC_ALL retained for grandchild expansion",
			preserved.AccessMask)
	}
}

func TestPropagateACL_Directory(t *testing.T) {
	existingACL := &ACL{ACEs: []ACE{
		{Type: ACE4_ACCESS_DENIED_ACE_TYPE, AccessMask: ACE4_DELETE, Who: "bob@example.com"},
		{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, Flag: ACE4_INHERITED_ACE, AccessMask: ACE4_READ_DATA, Who: SpecialEveryone},
	}}

	newParentACL := &ACL{
		AutoInherited: true,
		ACEs: []ACE{
			{
				Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Flag:       ACE4_DIRECTORY_INHERIT_ACE,
				AccessMask: ACE4_READ_DATA | ACE4_WRITE_DATA,
				Who:        SpecialGroup,
			},
		},
	}

	result := PropagateACL(newParentACL, existingACL, true, Creator{})

	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if len(result.ACEs) != 2 {
		t.Fatalf("expected 2 ACEs, got %d", len(result.ACEs))
	}

	// First: explicit DENY for bob.
	if result.ACEs[0].Who != "bob@example.com" {
		t.Error("expected explicit ACE preserved first")
	}
	// Second: inherited ALLOW for GROUP@.
	if result.ACEs[1].Who != SpecialGroup || !result.ACEs[1].IsInherited() {
		t.Error("expected new inherited ACE for GROUP@")
	}
}

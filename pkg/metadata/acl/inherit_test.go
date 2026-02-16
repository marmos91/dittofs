package acl

import "testing"

func TestComputeInheritedACL_FileInheritOnChildFile(t *testing.T) {
	parentACL := &ACL{ACEs: []ACE{
		{
			Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
			Flag:       ACE4_FILE_INHERIT_ACE,
			AccessMask: ACE4_READ_DATA,
			Who:        SpecialEveryone,
		},
	}}

	result := ComputeInheritedACL(parentACL, false)

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
	parentACL := &ACL{ACEs: []ACE{
		{
			Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
			Flag:       ACE4_DIRECTORY_INHERIT_ACE,
			AccessMask: ACE4_READ_DATA | ACE4_EXECUTE,
			Who:        SpecialGroup,
		},
	}}

	result := ComputeInheritedACL(parentACL, true)

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
	parentACL := &ACL{ACEs: []ACE{
		{
			Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
			Flag:       ACE4_DIRECTORY_INHERIT_ACE | ACE4_NO_PROPAGATE_INHERIT_ACE,
			AccessMask: ACE4_READ_DATA,
			Who:        SpecialOwner,
		},
	}}

	result := ComputeInheritedACL(parentACL, true)

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
	parentACL := &ACL{ACEs: []ACE{
		{
			Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
			Flag:       ACE4_DIRECTORY_INHERIT_ACE | ACE4_INHERIT_ONLY_ACE,
			AccessMask: ACE4_WRITE_DATA,
			Who:        SpecialEveryone,
		},
	}}

	result := ComputeInheritedACL(parentACL, true)

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
	result := ComputeInheritedACL(nil, false)
	if result != nil {
		t.Error("expected nil result for nil parent ACL")
	}

	result = ComputeInheritedACL(nil, true)
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

	result := ComputeInheritedACL(parentACL, false)
	if result != nil {
		t.Error("expected nil for file when no FILE_INHERIT flag")
	}

	result = ComputeInheritedACL(parentACL, true)
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
	fileResult := ComputeInheritedACL(parentACL, false)
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

	// Directory child: should get ACE 1 (DIRECTORY_INHERIT) and ACE 2 (both).
	dirResult := ComputeInheritedACL(parentACL, true)
	if dirResult == nil {
		t.Fatal("expected non-nil result for directory")
	}
	if len(dirResult.ACEs) != 2 {
		t.Fatalf("expected 2 ACEs for directory, got %d", len(dirResult.ACEs))
	}
	if dirResult.ACEs[0].Who != SpecialOwner {
		t.Errorf("dir ACE 0: expected OWNER@, got %s", dirResult.ACEs[0].Who)
	}
	if dirResult.ACEs[1].Who != SpecialGroup {
		t.Errorf("dir ACE 1: expected GROUP@, got %s", dirResult.ACEs[1].Who)
	}
}

func TestComputeInheritedACL_FileInheritClearsAllFlags(t *testing.T) {
	// FILE_INHERIT + DIRECTORY_INHERIT: when inherited by a file,
	// ALL inheritance flags are cleared.
	parentACL := &ACL{ACEs: []ACE{
		{
			Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
			Flag:       ACE4_FILE_INHERIT_ACE | ACE4_DIRECTORY_INHERIT_ACE,
			AccessMask: ACE4_READ_DATA,
			Who:        SpecialEveryone,
		},
	}}

	result := ComputeInheritedACL(parentACL, false)
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

	result := ComputeInheritedACL(parentACL, false)
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

	newParentACL := &ACL{ACEs: []ACE{
		{
			Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
			Flag:       ACE4_FILE_INHERIT_ACE,
			AccessMask: ACE4_READ_DATA | ACE4_EXECUTE,
			Who:        SpecialOwner,
		},
	}}

	result := PropagateACL(newParentACL, existingACL, false)

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

	result := PropagateACL(parentACL, nil, false)
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

	result := PropagateACL(nil, existingACL, false)

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
	result := PropagateACL(nil, nil, false)
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
	result := PropagateACL(newParentACL, existingACL, false)
	if result != nil {
		t.Error("expected nil result when all inherited ACEs removed and no new ones")
	}
}

func TestPropagateACL_Directory(t *testing.T) {
	existingACL := &ACL{ACEs: []ACE{
		{Type: ACE4_ACCESS_DENIED_ACE_TYPE, AccessMask: ACE4_DELETE, Who: "bob@example.com"},
		{Type: ACE4_ACCESS_ALLOWED_ACE_TYPE, Flag: ACE4_INHERITED_ACE, AccessMask: ACE4_READ_DATA, Who: SpecialEveryone},
	}}

	newParentACL := &ACL{ACEs: []ACE{
		{
			Type:       ACE4_ACCESS_ALLOWED_ACE_TYPE,
			Flag:       ACE4_DIRECTORY_INHERIT_ACE,
			AccessMask: ACE4_READ_DATA | ACE4_WRITE_DATA,
			Who:        SpecialGroup,
		},
	}}

	result := PropagateACL(newParentACL, existingACL, true)

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

package acl

import (
	"fmt"
	"testing"
)

// allowACE returns the first ALLOW ACE with the given Who, or nil. It reuses
// the package test helper findACE(aces, type, who).
func allowACE(a *ACL, who string) *ACE {
	return findACE(a.ACEs, ACE4_ACCESS_ALLOWED_ACE_TYPE, who)
}

func TestLocalDomainPrincipal(t *testing.T) {
	got := LocalDomainPrincipal(1000)
	want := fmt.Sprintf("1000%s", localDomainSuffix)
	if got != want {
		t.Fatalf("LocalDomainPrincipal(1000) = %q, want %q", got, want)
	}
}

func TestBuildShareRootACL_BaselineACEs(t *testing.T) {
	a := BuildShareRootACL(GrantNone, nil)

	if a.Source != ACLSourceShareGrant {
		t.Errorf("Source = %q, want %q", a.Source, ACLSourceShareGrant)
	}
	for _, who := range []string{SpecialOwner, SpecialSystem, SpecialAdministrators} {
		ace := allowACE(a, who)
		if ace == nil {
			t.Fatalf("missing baseline ACE for %q", who)
		}
		if ace.AccessMask != FullAccessMask {
			t.Errorf("%s mask = %#x, want FullAccessMask %#x", who, ace.AccessMask, FullAccessMask)
		}
		if ace.Type != ACE4_ACCESS_ALLOWED_ACE_TYPE {
			t.Errorf("%s is not an ALLOW ACE", who)
		}
		if ace.Flag&(ACE4_FILE_INHERIT_ACE|ACE4_DIRECTORY_INHERIT_ACE) == 0 {
			t.Errorf("%s ACE missing inheritance flags", who)
		}
	}
	// No grants, default none → no EVERYONE@ ACE (secure default preserved).
	if allowACE(a, SpecialEveryone) != nil {
		t.Errorf("unexpected EVERYONE@ ACE with default-permission none")
	}
}

func TestBuildShareRootACL_UserGrantLevels(t *testing.T) {
	tests := []struct {
		level     GrantLevel
		wantRead  bool
		wantWrite bool
		wantACL   bool // WRITE_ACL (admin/full only)
		wantACE   bool
	}{
		{GrantNone, false, false, false, false},
		{GrantRead, true, false, false, true},
		{GrantReadWrite, true, true, false, true},
		{GrantAdmin, true, true, true, true},
	}
	for _, tc := range tests {
		a := BuildShareRootACL(GrantNone, []RootGrant{{ID: 1000, Level: tc.level}})
		ace := allowACE(a, LocalDomainPrincipal(1000))
		if tc.wantACE != (ace != nil) {
			t.Fatalf("level %v: ACE present = %v, want %v", tc.level, ace != nil, tc.wantACE)
		}
		if ace == nil {
			continue
		}
		if got := ace.AccessMask&ACE4_READ_DATA != 0; got != tc.wantRead {
			t.Errorf("level %v: READ_DATA = %v, want %v", tc.level, got, tc.wantRead)
		}
		if got := ace.AccessMask&ACE4_WRITE_DATA != 0; got != tc.wantWrite {
			t.Errorf("level %v: WRITE_DATA = %v, want %v", tc.level, got, tc.wantWrite)
		}
		if got := ace.AccessMask&ACE4_WRITE_ACL != 0; got != tc.wantACL {
			t.Errorf("level %v: WRITE_ACL = %v, want %v", tc.level, got, tc.wantACL)
		}
	}
}

func TestBuildShareRootACL_DefaultPermissionEveryone(t *testing.T) {
	a := BuildShareRootACL(GrantRead, nil)
	ace := allowACE(a, SpecialEveryone)
	if ace == nil {
		t.Fatal("expected EVERYONE@ ACE for default-permission read")
	}
	if ace.AccessMask&ACE4_READ_DATA == 0 {
		t.Error("EVERYONE@ ACE missing READ_DATA")
	}
	if ace.AccessMask&ACE4_WRITE_DATA != 0 {
		t.Error("EVERYONE@ ACE should not grant WRITE_DATA for default read")
	}
}

func TestBuildShareRootACL_DeterministicOrder(t *testing.T) {
	grants := []RootGrant{
		{ID: 2000, IsGroup: true, Level: GrantRead},
		{ID: 1005, Level: GrantReadWrite},
		{ID: 1001, Level: GrantRead},
	}
	a1 := BuildShareRootACL(GrantNone, grants)
	a2 := BuildShareRootACL(GrantNone, grants)
	if len(a1.ACEs) != len(a2.ACEs) {
		t.Fatalf("ACE count differs across builds: %d vs %d", len(a1.ACEs), len(a2.ACEs))
	}
	for i := range a1.ACEs {
		if a1.ACEs[i].Who != a2.ACEs[i].Who {
			t.Fatalf("ACE order not deterministic at %d: %q vs %q", i, a1.ACEs[i].Who, a2.ACEs[i].Who)
		}
	}
	// Users (1001, 1005) must precede the group (2000) among projected grants.
	idxUser := -1
	idxGroup := -1
	for i, ace := range a1.ACEs {
		if ace.Who == LocalDomainPrincipal(1005) {
			idxUser = i
		}
		if ace.Who == LocalDomainPrincipal(2000) {
			idxGroup = i
		}
	}
	if idxUser == -1 || idxGroup == -1 || idxUser > idxGroup {
		t.Errorf("expected user ACEs before group ACEs (user=%d group=%d)", idxUser, idxGroup)
	}
}

// TestBuildShareRootACL_EvaluatesEndToEnd is the crux: a grant projected onto
// the root must actually be honored by the ACL evaluator for a user who is
// neither the root owner (uid 0) nor in its group. This is the exact scenario
// that previously failed — share grant said write, POSIX mode bits on the
// uid-0 root denied it.
func TestBuildShareRootACL_EvaluatesEndToEnd(t *testing.T) {
	const rootOwner = uint32(0) // share root is owned by uid 0

	t.Run("granted read-write user can write", func(t *testing.T) {
		a := BuildShareRootACL(GrantNone, []RootGrant{{ID: 1000, Level: GrantReadWrite}})
		ctx := &EvaluateContext{UID: 1000, GID: 1000, FileOwnerUID: rootOwner}
		if !Evaluate(a, ctx, ACE4_WRITE_DATA) {
			t.Error("read-write grantee was denied WRITE_DATA on a uid-0 root")
		}
		if !Evaluate(a, ctx, ACE4_READ_DATA) {
			t.Error("read-write grantee was denied READ_DATA")
		}
	})

	t.Run("read-only user cannot write", func(t *testing.T) {
		a := BuildShareRootACL(GrantNone, []RootGrant{{ID: 1000, Level: GrantRead}})
		ctx := &EvaluateContext{UID: 1000, GID: 1000, FileOwnerUID: rootOwner}
		if !Evaluate(a, ctx, ACE4_READ_DATA) {
			t.Error("read grantee was denied READ_DATA")
		}
		if Evaluate(a, ctx, ACE4_WRITE_DATA) {
			t.Error("read grantee was incorrectly allowed WRITE_DATA")
		}
	})

	t.Run("ungranted user denied with default none", func(t *testing.T) {
		a := BuildShareRootACL(GrantNone, []RootGrant{{ID: 1000, Level: GrantReadWrite}})
		ctx := &EvaluateContext{UID: 2000, GID: 2000, FileOwnerUID: rootOwner}
		if Evaluate(a, ctx, ACE4_READ_DATA) {
			t.Error("ungranted user got READ_DATA despite default-permission none")
		}
		if Evaluate(a, ctx, ACE4_WRITE_DATA) {
			t.Error("ungranted user got WRITE_DATA despite default-permission none")
		}
	})

	t.Run("default-permission read grants everyone read but not write", func(t *testing.T) {
		a := BuildShareRootACL(GrantRead, nil)
		ctx := &EvaluateContext{UID: 2000, GID: 2000, FileOwnerUID: rootOwner}
		if !Evaluate(a, ctx, ACE4_READ_DATA) {
			t.Error("default-read did not grant EVERYONE read")
		}
		if Evaluate(a, ctx, ACE4_WRITE_DATA) {
			t.Error("default-read incorrectly granted write")
		}
	})

	t.Run("group grant honored via supplementary GID", func(t *testing.T) {
		a := BuildShareRootACL(GrantNone, []RootGrant{{ID: 2000, IsGroup: true, Level: GrantReadWrite}})
		ctx := &EvaluateContext{UID: 3000, GID: 100, GIDs: []uint32{2000}, FileOwnerUID: rootOwner}
		if !Evaluate(a, ctx, ACE4_WRITE_DATA) {
			t.Error("member of granted group was denied WRITE_DATA")
		}
	})

	t.Run("root owner retains control via OWNER@", func(t *testing.T) {
		a := BuildShareRootACL(GrantNone, nil)
		ctx := &EvaluateContext{UID: 500, GID: 500, FileOwnerUID: 500}
		if !Evaluate(a, ctx, ACE4_WRITE_DATA) {
			t.Error("file owner was denied WRITE_DATA via OWNER@")
		}
	})
}

func TestBuildShareRootACL_ValidACL(t *testing.T) {
	a := BuildShareRootACL(GrantReadWrite, []RootGrant{
		{ID: 1001, Level: GrantRead},
		{ID: 2000, IsGroup: true, Level: GrantReadWrite},
	})
	if err := ValidateACL(a); err != nil {
		t.Fatalf("BuildShareRootACL produced an invalid ACL: %v", err)
	}
}

package handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/identity"
)

// Regression tests for #1317: SMB Kerberos must accept an AD/LDAP user that the
// identity resolver matched even when no local control-plane account exists,
// while still failing closed for disabled local accounts and unresolved
// principals.

func TestResolveSessionUser_LocalEnabledWins(t *testing.T) {
	uid := uint32(1000)
	local := &models.User{Username: "alice", UID: &uid, Enabled: true}
	resolved := &identity.ResolvedIdentity{Username: "alice", UID: 10001, GID: 10000, Found: true}

	got, ok := resolveSessionUser(local, nil, resolved)
	if !ok || got != local {
		t.Fatalf("local enabled account must win: ok=%v got=%v", ok, got)
	}
}

func TestResolveSessionUser_DirectoryResolvedWhenNoLocalAccount(t *testing.T) {
	resolved := &identity.ResolvedIdentity{
		Username: "alice", UID: 10001, GID: 10000,
		SID: "S-1-5-21-1-2-3-1104", GroupSIDs: []string{"S-1-5-21-1-2-3-513"}, Found: true,
	}

	got, ok := resolveSessionUser(nil, models.ErrUserNotFound, resolved)
	if !ok {
		t.Fatal("expected directory-resolved identity to be accepted")
	}
	if got.Username != "alice" || got.UID == nil || *got.UID != 10001 {
		t.Fatalf("synthesized user wrong: %+v", got)
	}
	// getUserIdentity must derive the resolved UID + primary GID.
	uid, gid := getUserIdentity(got)
	if uid != 10001 || gid != 10000 {
		t.Fatalf("getUserIdentity = %d/%d, want 10001/10000", uid, gid)
	}
	if got.SID != "S-1-5-21-1-2-3-1104" || len(got.GroupSIDs) != 1 {
		t.Fatalf("SID/GroupSIDs not carried: %+v", got)
	}
}

// TestSynthUserFromResolved_CarriesSupplementaryGIDs verifies the synthesized
// user lists the primary GID first (so getUserIdentity picks it) and also
// carries the full resolved supplementary set, deduplicating the primary
// (#1327).
func TestSynthUserFromResolved_CarriesSupplementaryGIDs(t *testing.T) {
	resolved := &identity.ResolvedIdentity{
		Username: "alice", UID: 10001, GID: 10000,
		GIDs:  []uint32{10000, 10007, 10042}, // includes primary + two nested groups
		Found: true,
	}

	got := synthUserFromResolved(resolved)

	// Primary GID must be first for getUserIdentity.
	uid, gid := getUserIdentity(got)
	if uid != 10001 || gid != 10000 {
		t.Fatalf("getUserIdentity = %d/%d, want 10001/10000", uid, gid)
	}
	// All distinct GIDs present exactly once (primary not duplicated).
	gotGIDs := map[uint32]int{}
	for _, g := range got.Groups {
		if g.GID != nil {
			gotGIDs[*g.GID]++
		}
	}
	for _, want := range []uint32{10000, 10007, 10042} {
		if gotGIDs[want] != 1 {
			t.Fatalf("GID %d appears %d times, want 1: %+v", want, gotGIDs[want], gotGIDs)
		}
	}
	if len(gotGIDs) != 3 {
		t.Fatalf("expected 3 distinct GIDs, got %d: %+v", len(gotGIDs), gotGIDs)
	}
}

// TestBuildAuthContextFromUser_PopulatesSupplementaryGIDs verifies the SMB auth
// context carries Identity.GIDs from the user's full group set so POSIX-mode
// group checks (HasGID) honor secondary / nested-AD groups, matching NFS (#1327).
func TestBuildAuthContextFromUser_PopulatesSupplementaryGIDs(t *testing.T) {
	user := synthUserFromResolved(&identity.ResolvedIdentity{
		Username: "alice", UID: 10001, GID: 10000,
		GIDs: []uint32{10000, 10007, 10042}, Found: true,
	})

	authCtx := BuildAuthContextFromUser(&SMBHandlerContext{Context: context.Background()}, user)

	if authCtx.Identity.GID == nil || *authCtx.Identity.GID != 10000 {
		t.Fatalf("primary GID = %v, want 10000", authCtx.Identity.GID)
	}
	for _, g := range []uint32{10007, 10042} {
		if !authCtx.Identity.HasGID(g) {
			t.Fatalf("HasGID(%d) = false, want true (supplementary group dropped)", g)
		}
	}
	if authCtx.Identity.HasGID(99999) {
		t.Fatal("HasGID(99999) = true, want false")
	}
}

func TestResolveSessionUser_DisabledLocalAccountFailsClosed(t *testing.T) {
	uid := uint32(1000)
	disabled := &models.User{Username: "alice", UID: &uid, Enabled: false}
	resolved := &identity.ResolvedIdentity{Username: "alice", UID: 10001, GID: 10000, Found: true}

	// A disabled local account is an explicit block and must NOT be overridden
	// by the directory identity.
	if _, ok := resolveSessionUser(disabled, nil, resolved); ok {
		t.Fatal("disabled local account must fail closed")
	}
}

func TestResolveSessionUser_UnresolvedPrincipalFailsClosed(t *testing.T) {
	if _, ok := resolveSessionUser(nil, models.ErrUserNotFound, &identity.ResolvedIdentity{Found: false}); ok {
		t.Fatal("unresolved principal must fail closed (not guest)")
	}
	if _, ok := resolveSessionUser(nil, errors.New("store down"), nil); ok {
		t.Fatal("nil resolver + no local user must fail closed")
	}
}

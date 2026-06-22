package handlers

import (
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

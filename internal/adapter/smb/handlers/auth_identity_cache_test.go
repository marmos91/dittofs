package handlers

import (
	"slices"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/session"
)

// reprimePAC mirrors what primeAuthContext does: copy the session's current PAC
// group SIDs onto the request context before building the auth context.
func reprimePAC(ctx *SMBHandlerContext, sess *session.Session) {
	ctx.PACGroupSIDs, _ = sess.PACIdentity()
}

func TestAuthIdentityCache_HitReturnsSamePointer(t *testing.T) {
	user := benchAuthUser()
	sess := session.NewSessionWithUser(1, "10.0.0.1:445", user, "")
	ctx := benchAuthCtx(user, sess)
	ctx.PACGroupSIDs = nil // no PAC; identity comes purely from the user

	first := BuildAuthContextFromUser(ctx, user).Identity
	second := BuildAuthContextFromUser(ctx, user).Identity
	if first != second {
		t.Fatalf("expected cache hit to reuse the same *Identity, got distinct pointers")
	}
}

func TestAuthIdentityCache_OpenerSnapshotNotCached(t *testing.T) {
	user := benchAuthUser()
	sess := session.NewSessionWithUser(1, "10.0.0.1:445", user, "")
	ctx := benchAuthCtx(user, sess)

	// A handle-bound op passes a different (frozen opener) user than the
	// session-current ctx.User. It must not hit or populate the session cache.
	opener := benchAuthUser() // distinct pointer
	a := BuildAuthContextFromUser(ctx, opener).Identity
	b := BuildAuthContextFromUser(ctx, opener).Identity
	if a == b {
		t.Fatalf("opener snapshot must not be cached on the session")
	}
	// And the real session user must still cache normally.
	c := BuildAuthContextFromUser(ctx, user).Identity
	d := BuildAuthContextFromUser(ctx, user).Identity
	if c != d {
		t.Fatalf("session-current user should cache")
	}
}

func TestAuthIdentityCache_InvalidatedOnPACRefresh(t *testing.T) {
	user := benchAuthUser()
	sess := session.NewSessionWithUser(1, "10.0.0.1:445", user, "")
	sess.SetPACIdentity([]string{"S-1-5-21-OLD"}, user.SID)
	ctx := benchAuthCtx(user, sess)
	reprimePAC(ctx, sess)

	before := BuildAuthContextFromUser(ctx, user).Identity
	if !slices.Contains(before.GroupSIDs, "S-1-5-21-OLD") {
		t.Fatalf("expected old PAC SID before refresh, got %v", before.GroupSIDs)
	}

	// Kerberos re-auth refreshes the PAC with a new transitive group set.
	sess.SetPACIdentity([]string{"S-1-5-21-NEW"}, user.SID)
	reprimePAC(ctx, sess)

	after := BuildAuthContextFromUser(ctx, user).Identity
	if after == before {
		t.Fatalf("PAC refresh must invalidate the cached identity")
	}
	if slices.Contains(after.GroupSIDs, "S-1-5-21-OLD") {
		t.Fatalf("stale group SID survived PAC refresh: %v", after.GroupSIDs)
	}
	if !slices.Contains(after.GroupSIDs, "S-1-5-21-NEW") {
		t.Fatalf("expected refreshed PAC SID, got %v", after.GroupSIDs)
	}
}

func TestAuthIdentityCache_InvalidatedOnPACClear(t *testing.T) {
	user := benchAuthUser()
	sess := session.NewSessionWithUser(1, "10.0.0.1:445", user, "")
	sess.SetPACIdentity([]string{"S-1-5-21-OLD"}, user.SID)
	ctx := benchAuthCtx(user, sess)
	reprimePAC(ctx, sess)

	before := BuildAuthContextFromUser(ctx, user).Identity
	if !slices.Contains(before.GroupSIDs, "S-1-5-21-OLD") {
		t.Fatalf("expected old PAC SID before clear, got %v", before.GroupSIDs)
	}

	// NTLM re-auth clears any Kerberos PAC carried from the prior auth.
	sess.SetPACIdentity(nil, "")
	reprimePAC(ctx, sess)

	after := BuildAuthContextFromUser(ctx, user).Identity
	if after == before {
		t.Fatalf("PAC clear must invalidate the cached identity")
	}
	if slices.Contains(after.GroupSIDs, "S-1-5-21-OLD") {
		t.Fatalf("stale group SID survived PAC clear: %v", after.GroupSIDs)
	}
}

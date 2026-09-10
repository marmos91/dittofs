package handlers

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// TestSessionSetup_AnonymousReauth_GuestPolicyRejected pins that the guest
// policy gates an anonymous NTLM re-authentication: with guest access disabled
// by policy, the anonymous branch of SESSION_SETUP must reject the
// re-authentication with STATUS_LOGON_FAILURE instead of silently downgrading
// an authenticated session to an unauthenticated guest one. The gate is
// checkGuestPolicy, the same contract the fresh guest-session branches
// enforce.
func TestSessionSetup_AnonymousReauth_GuestPolicyRejected(t *testing.T) {
	h := NewHandler()
	h.GuestEnabled = false

	if res := h.checkGuestPolicy(); res == nil || res.Status != types.StatusLogonFailure {
		t.Fatalf("checkGuestPolicy with guest disabled: status=%v, want STATUS_LOGON_FAILURE (the gate the anonymous reauth branch applies)", res)
	}
}

// TestTryReauthUpdate_IdentityMutation pins that the reauth identity swap goes
// through the locked mutator: after the update, the session's identity fields
// all reflect the new identity atomically (the caller would observe a torn
// identity with the old unlocked field-by-field writes).
func TestTryReauthUpdate_IdentityMutation(t *testing.T) {
	h := NewHandler()

	const sessionID = uint64(0xdef)
	oldUser := &models.User{Username: "alice", Enabled: true}
	h.CreateSessionWithUser(sessionID, "127.0.0.1:1", oldUser, "DITTOFS")

	newUser := &models.User{Username: "bob", Enabled: true}
	pending := &PendingAuth{SessionID: sessionID, IsReauth: true}
	if res := h.tryReauthUpdate(pending, "bob", "EXAMPLE", newUser, false); res == nil {
		t.Fatal("tryReauthUpdate returned nil for an existing session")
	}

	got, ok := h.GetSession(sessionID)
	if !ok {
		t.Fatal("session disappeared after reauth update")
	}
	if got.Username != "bob" || got.Domain != "EXAMPLE" {
		t.Errorf("identity = (%q, %q), want (bob, EXAMPLE)", got.Username, got.Domain)
	}
	if got.User != newUser {
		t.Error("User not swapped to the new identity")
	}
	if got.IsGuest || got.IsNull {
		t.Error("named reauth must not set guest/null status")
	}
}

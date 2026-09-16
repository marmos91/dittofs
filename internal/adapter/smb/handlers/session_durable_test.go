package handlers

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// TestSessionSetup_UnknownNonzeroSessionIDRejected locks in the session-table
// rule for SESSION_SETUP: a request carrying a nonzero SessionId that matches
// no session in the session table must fail rather than authenticate under the
// client-supplied ID. A server that accepted such a request would attach an
// authenticated session to an ID an attacker chose, and a later client
// legitimately minted that same ID would collide with it.
func TestSessionSetup_UnknownNonzeroSessionIDRejected(t *testing.T) {
	h := NewHandler()
	ctx := newTestContext(99999) // no session with this ID exists

	ntlm := validNTLMNegotiateMessage()
	body := buildSessionSetupRequestBody(ntlm)

	result, err := h.SessionSetup(ctx, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != types.StatusUserSessionDeleted {
		t.Fatalf("status = 0x%x, want StatusUserSessionDeleted (0x%x)",
			result.Status, types.StatusUserSessionDeleted)
	}
	// The client-supplied ID must not have been adopted: no pending auth may
	// exist under it, and no session may have been minted for it.
	if _, ok := h.GetPendingAuth(99999, ctx.ConnID); ok {
		t.Error("pending auth must not be stored under the rejected client-supplied SessionID")
	}
	if _, ok := h.GetSession(99999); ok {
		t.Error("no session may exist under the rejected client-supplied SessionID")
	}
}

// TestSessionSetup_ReauthNonzeroSessionIDStillAccepted pins the sibling case:
// a nonzero SessionId that DOES match an existing session remains a valid
// re-authentication attempt (MoreProcessingRequired, pending auth stored).
func TestSessionSetup_ReauthNonzeroSessionIDStillAccepted(t *testing.T) {
	h := NewHandler()
	sess := h.CreateSession("127.0.0.1:12345", false, "testuser", "DOMAIN")
	ctx := newTestContext(sess.SessionID)

	ntlm := validNTLMNegotiateMessage()
	body := buildSessionSetupRequestBody(ntlm)

	result, err := h.SessionSetup(ctx, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != types.StatusMoreProcessingRequired {
		t.Fatalf("status = 0x%x, want StatusMoreProcessingRequired", result.Status)
	}
	if _, ok := h.GetPendingAuth(sess.SessionID, ctx.ConnID); !ok {
		t.Error("pending auth should be stored for the re-authentication attempt")
	}
}

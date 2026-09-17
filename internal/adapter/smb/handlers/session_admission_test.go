package handlers

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// logoffDuringAdmission installs a hook that runs a LOGOFF's retirement of the
// given session on a separate goroutine, and blocks until it has completed,
// immediately before the next admitted transition takes the session's admission
// lock. That is the interleaving the issue names — LOGOFF landing between a
// SESSION_SETUP's decision that the session may authenticate and the point the
// mechanism stores its state — forced deterministically rather than raced for.
//
// The retirement is MarkLoggedOff, which is exactly what the LOGOFF handler
// performs at that seam: it takes the same lock the admission holds. The hook
// is cleared on cleanup so a later call in the same test is not re-hooked.
func logoffDuringAdmission(t *testing.T, h *Handler, sess *session.Session) {
	t.Helper()
	done := make(chan struct{})
	h.admitTransitionHook = func() {
		// One-shot: a transition that retries must not fire the LOGOFF again.
		h.admitTransitionHook = nil
		go func() {
			sess.MarkLoggedOff()
			close(done)
		}()
		<-done
	}
	t.Cleanup(func() { h.admitTransitionHook = nil })
}

// TestSessionSetup_RefusesAHandshakeLogoffLandsDuringAdmission drives a TYPE_1
// on an existing session while a LOGOFF retires it in the instant between the
// decision that the session may authenticate and the store of the handshake's
// pending state. Before the admission point those were two steps: the LOGOFF
// slipped between them, the PendingAuth was armed on a retired session, and the
// TYPE_3 that followed could finish a handshake whose response the client could
// not verify. Now the store is admitted under the lock MarkLoggedOff takes, so
// the LOGOFF either wins and the handshake is refused, or the store lands first
// and the LOGOFF's own teardown clears it — the order a LOGOFF a moment later
// would have produced.
func TestSessionSetup_RefusesAHandshakeLogoffLandsDuringAdmission(t *testing.T) {
	h := NewHandler()
	h.NtlmEnabled = true
	h.Registry = newTestRuntime(t, nil)

	const sessionID = uint64(0xa11d0ffe)
	sess := h.CreateSessionWithUser(sessionID, "127.0.0.1:1",
		&models.User{Username: "alice", Enabled: true}, "")
	ctx := newTestContext(sessionID)

	logoffDuringAdmission(t, h, sess)

	res, err := h.SessionSetup(ctx, buildSessionSetupRequestBody(validNTLMNegotiateMessage()))
	if err != nil {
		t.Fatalf("SESSION_SETUP returned error: %v", err)
	}
	if res.Status != types.StatusUserSessionDeleted {
		t.Errorf("status = 0x%08x, want STATUS_USER_SESSION_DELETED (0x%08x): the handshake was "+
			"armed on a session the LOGOFF had already retired",
			uint32(res.Status), uint32(types.StatusUserSessionDeleted))
	}
	if _, stored := h.GetPendingAuth(sessionID, ctx.ConnID); stored {
		t.Error("a PendingAuth was stored across the LOGOFF: the admission point did not " +
			"cover the decision-to-arm span")
	}
}

// TestSessionSetup_RefusesAReauthLogoffLandsDuringAdmission drives the same
// interleaving against a TYPE_3 that completes a re-authentication. The
// completion publishes a fresh identity onto the session, and before the
// admission point that write was a separate step from the caller's LoggedOff
// check: a LOGOFF in between let the retired session take a new identity and
// answer with a success the client could not verify. The write is now admitted,
// so the LOGOFF either precedes it — the completion answers as a deleted
// session and the identity is untouched — or follows it, which is the order a
// LOGOFF a moment later would have produced.
func TestSessionSetup_RefusesAReauthLogoffLandsDuringAdmission(t *testing.T) {
	f := newReauthFixture(t, nil)

	logoffDuringAdmission(t, f.h, mustSession(t, f.h, f.sessionID))

	// An anonymous TYPE_3 on a re-auth handshake routes through tryReauthUpdate,
	// the write the admission point covers. Guest policy is the default
	// (enabled, signing not required), so the completion is otherwise accepted.
	type3Body := buildSessionSetupRequestBody(buildNTLMAuthenticateForTest("", "DOMAIN", nil))
	res, err := f.h.SessionSetup(f.ctx, type3Body)
	if err != nil {
		t.Fatalf("SESSION_SETUP TYPE_3 returned error: %v", err)
	}
	if res.Status != types.StatusUserSessionDeleted {
		t.Errorf("status = 0x%08x, want STATUS_USER_SESSION_DELETED (0x%08x): a re-authentication "+
			"published a fresh identity onto a session the LOGOFF had already retired",
			uint32(res.Status), uint32(types.StatusUserSessionDeleted))
	}

	got, ok := f.h.GetSession(f.sessionID)
	if !ok {
		t.Fatal("session disappeared from the manager; in-flight signing would break")
	}
	if got.Username != "alice" {
		t.Errorf("Username = %q, want alice: the refused re-authentication replaced the "+
			"retired session's identity anyway", got.Username)
	}
	if !got.LoggedOff.Load() {
		t.Error("session is not LoggedOff after the LOGOFF: the re-authentication resurrected it")
	}
}

// TestSessionAdmission_RefusesEveryTransitionAfterLogoff pins the admission
// point's own contract: once the session is retired, no admitted transition
// runs, and a transition that would have been refused has no effect. It is the
// sequential form of the concurrent assertion above — the lock makes "refused
// before" and "refused after" the same answer.
func TestSessionAdmission_RefusesEveryTransitionAfterLogoff(t *testing.T) {
	h := NewHandler()
	sess := h.CreateSession("127.0.0.1:1", false, "alice", "")
	sess.MarkLoggedOff()

	ran := false
	if h.admitSessionTransition(sess, func() { ran = true }) {
		t.Error("admitSessionTransition admitted a transition on a logged-off session")
	}
	if ran {
		t.Error("the refused transition ran anyway: the decision and the act are not one step")
	}
}

func mustSession(t *testing.T, h *Handler, sessionID uint64) *session.Session {
	t.Helper()
	sess, ok := h.GetSession(sessionID)
	if !ok {
		t.Fatalf("session %d not found", sessionID)
	}
	return sess
}

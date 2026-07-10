package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/auth/netlogon"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	pkgidentity "github.com/marmos91/dittofs/pkg/identity"
)

// seedPassthroughSession creates a primary session for a directory-resolved
// (NETLOGON pass-through) domain user — one with no local control-plane account
// but a stamped Windows SID — mirroring what tryNetlogonFallback produces on the
// origin channel. This is the session an additional multichannel connection
// binds to.
func seedPassthroughSession(t *testing.T, h *Handler, sessionID uint64, username, sid string) {
	t.Helper()
	uid := uint32(10001)
	user := &models.User{
		Username: username,
		UID:      &uid,
		SID:      sid,
		Enabled:  true,
	}
	sess := h.CreateSessionWithUser(sessionID, "127.0.0.1:1", user, "CORP")
	sess.OriginConnID = 1
	sess.Dialect = types.Dialect0311
}

// driveBindTYPE3 stores a binding PendingAuth (IsBinding, non-reauth) targeting
// an existing session and drives completeNTLMAuth with an NTLM TYPE_3 for a
// domain user that has no local account, exercising the #1632 bind pass-through
// path. Returns the handler result and the connection's ConnID.
func driveBindTYPE3(t *testing.T, h *Handler, sessionID uint64, username, domain string) (*HandlerResult, uint64) {
	t.Helper()
	ctx := newTestContext(sessionID)
	ctx.ConnCryptoState = &mockCryptoState{dialect: types.Dialect0311}

	var challenge [8]byte
	copy(challenge[:], []byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88})
	h.StorePendingAuth(&PendingAuth{
		SessionID:        sessionID,
		ConnID:           ctx.ConnID,
		ClientAddr:       ctx.ClientAddr,
		CreatedAt:        time.Now(),
		ServerChallenge:  challenge,
		IsBinding:        true,
		BindingSessionID: sessionID,
	})

	ntResp := make([]byte, 24)
	for i := range ntResp {
		ntResp[i] = byte(i + 1)
	}
	token := buildNTLMAuthenticateForTest(username, domain, ntResp)
	result, err := h.completeNTLMAuth(ctx, token)
	if err != nil {
		t.Fatalf("bind TYPE_3 returned error: %v", err)
	}
	return result, ctx.ConnID
}

// TestSessionBind_DomainUserPassthrough (Test A): a domain pass-through user
// (no local account) can bind an additional SMB3 channel. On a local miss the
// binding TYPE_3 is validated via NETLOGON, the DC-resolved identity matches the
// existing session's user, and a signing channel is registered — no
// STATUS_LOGON_FAILURE on the spare connection.
func TestSessionBind_DomainUserPassthrough(t *testing.T) {
	const sessionID = uint64(0x0bad0bad)
	const sid = "S-1-5-21-1111111111-2222222222-3333333333-1001"
	groupSIDs := []string{"S-1-5-21-1111111111-2222222222-3333333333-513"}

	var baseKey [16]byte
	for i := range baseKey {
		baseKey[i] = byte(0xA0 + i)
	}

	nl := &fakeNetlogon{res: &netlogon.LogonResult{
		SessionBaseKey: baseKey,
		UserSID:        sid,
		GroupSIDs:      groupSIDs,
		Username:       "alice",
		DomainName:     "CORP",
	}}
	provider := &fakeIdentityProvider{
		name:    "netlogon",
		wantSID: sid,
		resolved: &pkgidentity.ResolvedIdentity{
			Username:  "alice",
			UID:       10001,
			GID:       10001,
			SID:       sid,
			GroupSIDs: groupSIDs,
			Found:     true,
		},
	}
	h := newNetlogonFallbackHandler(t, nl, provider)
	seedPassthroughSession(t, h, sessionID, "alice", sid)

	result, connID := driveBindTYPE3(t, h, sessionID, "alice", "CORP")

	if result.Status.IsError() {
		t.Fatalf("expected bind success, got status 0x%08x", uint32(result.Status))
	}
	if !result.IsBinding {
		t.Error("result.IsBinding = false, want true (channel bind response)")
	}
	if !nl.called {
		t.Fatal("NETLOGON authenticator was never called on the bind local miss")
	}

	// A channel must have been registered on the existing session for this conn.
	sess, ok := h.GetSession(sessionID)
	if !ok {
		t.Fatalf("session 0x%x vanished", sessionID)
	}
	if ch := sess.GetChannel(connID); ch == nil {
		t.Fatalf("no channel registered for connID %d after bind", connID)
	} else if ch.Signer == nil || len(ch.SigningKey) == 0 {
		t.Error("bound channel has no signing key derived from the DC SessionBaseKey")
	}
	// The bind must not have replaced the session's identity.
	if sess.User == nil || sess.User.SID != sid {
		t.Errorf("session user SID = %v, want %q (identity must be preserved)", sess.User, sid)
	}
}

// TestSessionBind_DomainUserIdentityMismatch (Test B): when the DC resolves the
// bind to a DIFFERENT identity than the one that owns the session, the bind is
// rejected with STATUS_ACCESS_DENIED and no channel is registered. This is the
// security boundary — a bind must not attach a different identity.
func TestSessionBind_DomainUserIdentityMismatch(t *testing.T) {
	const sessionID = uint64(0x0bad0bad)
	const sessSID = "S-1-5-21-1111111111-2222222222-3333333333-1001" // session owner
	const bindSID = "S-1-5-21-1111111111-2222222222-3333333333-2002" // different user

	var baseKey [16]byte
	for i := range baseKey {
		baseKey[i] = byte(0xB0 + i)
	}

	// DC authenticates the bind as a different SID than the session owner.
	nl := &fakeNetlogon{res: &netlogon.LogonResult{
		SessionBaseKey: baseKey,
		UserSID:        bindSID,
		Username:       "mallory",
		DomainName:     "CORP",
	}}
	provider := &fakeIdentityProvider{
		name:    "netlogon",
		wantSID: bindSID,
		resolved: &pkgidentity.ResolvedIdentity{
			Username: "mallory",
			UID:      20002,
			GID:      20002,
			SID:      bindSID,
			Found:    true,
		},
	}
	h := newNetlogonFallbackHandler(t, nl, provider)
	seedPassthroughSession(t, h, sessionID, "alice", sessSID)

	result, connID := driveBindTYPE3(t, h, sessionID, "mallory", "CORP")

	if result.Status != types.StatusAccessDenied {
		t.Fatalf("status = 0x%08x, want STATUS_ACCESS_DENIED", uint32(result.Status))
	}
	if !nl.called {
		t.Fatal("NETLOGON authenticator was never called")
	}
	sess, ok := h.GetSession(sessionID)
	if !ok {
		t.Fatalf("session 0x%x vanished", sessionID)
	}
	if ch := sess.GetChannel(connID); ch != nil {
		t.Fatal("a channel was registered despite an identity mismatch (security boundary violated)")
	}
	// The original session identity must be untouched.
	if sess.User == nil || sess.User.SID != sessSID {
		t.Errorf("session user SID = %v, want %q (must be preserved after rejected bind)", sess.User, sessSID)
	}
}

// TestSessionBind_DomainUserFailClosed: a NETLOGON error on the bind path must
// yield STATUS_LOGON_FAILURE with no channel registered — the primary session
// is left intact and the spare connection simply fails.
func TestSessionBind_DomainUserFailClosed(t *testing.T) {
	const sessionID = uint64(0x0bad0bad)
	const sid = "S-1-5-21-1111111111-2222222222-3333333333-1001"

	nl := &fakeNetlogon{err: context.DeadlineExceeded}
	h := newNetlogonFallbackHandler(t, nl, &fakeIdentityProvider{name: "netlogon"})
	seedPassthroughSession(t, h, sessionID, "alice", sid)

	result, connID := driveBindTYPE3(t, h, sessionID, "alice", "CORP")

	if result.Status != types.StatusLogonFailure {
		t.Fatalf("status = 0x%08x, want STATUS_LOGON_FAILURE", uint32(result.Status))
	}
	if !nl.called {
		t.Fatal("NETLOGON authenticator was never called")
	}
	sess, ok := h.GetSession(sessionID)
	if !ok {
		t.Fatalf("session 0x%x vanished (bind must not destroy the session)", sessionID)
	}
	if ch := sess.GetChannel(connID); ch != nil {
		t.Fatal("a channel was registered on NETLOGON failure (fail-closed violated)")
	}
}

// TestSessionBind_NetlogonDisabled: with no authenticator injected, a domain
// user that misses locally on the bind path yields STATUS_LOGON_FAILURE with no
// channel (no fallback attempted at all).
func TestSessionBind_NetlogonDisabled(t *testing.T) {
	const sessionID = uint64(0x0bad0bad)
	const sid = "S-1-5-21-1111111111-2222222222-3333333333-1001"

	h := newNetlogonFallbackHandler(t, nil, nil)
	seedPassthroughSession(t, h, sessionID, "alice", sid)

	result, connID := driveBindTYPE3(t, h, sessionID, "alice", "CORP")

	if result.Status != types.StatusLogonFailure {
		t.Fatalf("status = 0x%08x, want STATUS_LOGON_FAILURE", uint32(result.Status))
	}
	if sess, ok := h.GetSession(sessionID); ok {
		if ch := sess.GetChannel(connID); ch != nil {
			t.Fatal("a channel was registered with no NETLOGON authenticator")
		}
	}
}

// TestReauth_DoesNotUseNetlogonFallback (Test C): a re-authentication for a
// domain user with no local account must NOT consult the NETLOGON fallback (a
// re-auth must not resurrect a passed-through identity). It fails closed with
// STATUS_LOGON_FAILURE and the authenticator is never called.
func TestReauth_DoesNotUseNetlogonFallback(t *testing.T) {
	const sessionID = uint64(0x0bad0bad)
	const sid = "S-1-5-21-1111111111-2222222222-3333333333-1001"

	var baseKey [16]byte
	nl := &fakeNetlogon{res: &netlogon.LogonResult{
		SessionBaseKey: baseKey,
		UserSID:        sid,
		Username:       "alice",
		DomainName:     "CORP",
	}}
	provider := &fakeIdentityProvider{
		name:    "netlogon",
		wantSID: sid,
		resolved: &pkgidentity.ResolvedIdentity{
			Username: "alice", UID: 10001, GID: 10001, SID: sid, Found: true,
		},
	}
	h := newNetlogonFallbackHandler(t, nl, provider)
	// An existing authenticated session that the re-auth targets.
	seedPassthroughSession(t, h, sessionID, "alice", sid)

	ctx := newTestContext(sessionID)
	ctx.ConnCryptoState = &mockCryptoState{dialect: types.Dialect0311}

	var challenge [8]byte
	copy(challenge[:], []byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88})
	h.StorePendingAuth(&PendingAuth{
		SessionID:       sessionID,
		ConnID:          ctx.ConnID,
		ClientAddr:      ctx.ClientAddr,
		CreatedAt:       time.Now(),
		ServerChallenge: challenge,
		IsReauth:        true,
	})

	ntResp := make([]byte, 24)
	for i := range ntResp {
		ntResp[i] = byte(i + 1)
	}
	token := buildNTLMAuthenticateForTest("alice", "CORP", ntResp)
	result, err := h.completeNTLMAuth(ctx, token)
	if err != nil {
		t.Fatalf("reauth TYPE_3 returned error: %v", err)
	}

	if result.Status != types.StatusLogonFailure {
		t.Fatalf("status = 0x%08x, want STATUS_LOGON_FAILURE", uint32(result.Status))
	}
	if nl.called {
		t.Fatal("NETLOGON fallback was consulted on a re-auth (must be excluded)")
	}
}

// TestBindIdentityMatchesSession covers the identity match that guards channel
// binding. The security-critical case is the SID-vs-empty escalation: a SID-less
// local account must never bind onto a SID-bearing (domain) session by matching
// on username alone.
func TestBindIdentityMatchesSession(t *testing.T) {
	const sidA = "S-1-5-21-1-2-3-1107"
	const sidB = "S-1-5-21-1-2-3-1200"
	mk := func(username, sid string) *session.Session {
		return &session.Session{User: &models.User{Username: username, SID: sid}}
	}
	tests := []struct {
		name string
		sess *session.Session
		auth *models.User
		want bool
	}{
		{"same SID matches", mk("alice", sidA), &models.User{Username: "alice", SID: sidA}, true},
		{"different SID rejected", mk("alice", sidA), &models.User{Username: "alice", SID: sidB}, false},
		{"SID session vs SID-less local same name rejected (escalation)", mk("alice", sidA), &models.User{Username: "alice", SID: ""}, false},
		{"SID-less bind onto SID session rejected regardless of name", mk("alice", sidA), &models.User{Username: "ALICE", SID: ""}, false},
		{"SID bind onto SID-less session rejected", mk("alice", ""), &models.User{Username: "alice", SID: sidA}, false},
		{"local-vs-local same name (case-insensitive) matches", mk("alice", ""), &models.User{Username: "ALICE", SID: ""}, true},
		{"local-vs-local different name rejected", mk("alice", ""), &models.User{Username: "bob", SID: ""}, false},
		{"nil auth user rejected", mk("alice", sidA), nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := bindIdentityMatchesSession(tt.sess, tt.auth); got != tt.want {
				t.Fatalf("bindIdentityMatchesSession = %v, want %v", got, tt.want)
			}
		})
	}
}

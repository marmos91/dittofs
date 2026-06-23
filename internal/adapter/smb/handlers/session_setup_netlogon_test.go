package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/auth/netlogon"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	pkgidentity "github.com/marmos91/dittofs/pkg/identity"
)

// fakeNetlogon is a stub NetlogonAuthenticator that returns a canned result.
type fakeNetlogon struct {
	res    *netlogon.LogonResult
	err    error
	called bool
}

func (f *fakeNetlogon) NetworkLogon(_ context.Context, _ netlogon.NetworkLogonRequest) (*netlogon.LogonResult, error) {
	f.called = true
	return f.res, f.err
}

// fakeIdentityProvider resolves a single SID to a fixed UID/GID so the domain
// user fallback in completeNTLMAuth can synthesize a session for an AD user
// that has no local control-plane account.
type fakeIdentityProvider struct {
	name     string
	wantSID  string
	resolved *pkgidentity.ResolvedIdentity
}

func (p *fakeIdentityProvider) Name() string { return p.name }

func (p *fakeIdentityProvider) CanResolve(cred *pkgidentity.Credential) bool {
	return cred.ExternalID == p.wantSID
}

func (p *fakeIdentityProvider) Resolve(_ context.Context, cred *pkgidentity.Credential) (*pkgidentity.ResolvedIdentity, error) {
	if cred.ExternalID == p.wantSID {
		return p.resolved, nil
	}
	return &pkgidentity.ResolvedIdentity{Found: false}, nil
}

// newNetlogonFallbackHandler builds a Handler wired with an empty (in-memory)
// user store so the local NTLM lookup MISSES for a domain user, plus the given
// netlogon authenticator. When provider is non-nil it is installed on the
// centralized identity resolver so the DC-returned SID resolves to a UID/GID.
func newNetlogonFallbackHandler(t *testing.T, nl netlogon.NetlogonAuthenticator, provider pkgidentity.IdentityProvider) *Handler {
	t.Helper()
	h := NewHandler()
	h.NtlmEnabled = true
	h.Registry = runtime.New(newInMemoryStoreForTest(t))
	h.NetlogonAuth = nl
	if provider != nil {
		h.SetIdentityResolver(pkgidentity.NewResolver(pkgidentity.WithProvider(provider)))
	}
	return h
}

// driveDomainTYPE3 stores a fresh (non-reauth, non-binding) PendingAuth with a
// known ServerChallenge and drives completeNTLMAuth with an NTLM TYPE_3 for a
// domain user that has no local account. Returns the handler result.
func driveDomainTYPE3(t *testing.T, h *Handler, username, domain string) (*HandlerResult, uint64) {
	t.Helper()
	const sessionID = uint64(0x0bad0bad)
	ctx := newTestContext(sessionID)

	var challenge [8]byte
	copy(challenge[:], []byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88})
	h.StorePendingAuth(&PendingAuth{
		SessionID:       sessionID,
		ClientAddr:      ctx.ClientAddr,
		CreatedAt:       time.Now(),
		ServerChallenge: challenge,
		ConnID:          ctx.ConnID,
	})

	// A 24+ byte NtChallengeResponse is required so the NETLOGON fallback has
	// something to forward, but here the local store has no matching user, so
	// the NTLMv2 path is never reached locally — the fallback consumes it.
	ntResp := make([]byte, 24)
	for i := range ntResp {
		ntResp[i] = byte(i + 1)
	}
	body := buildSessionSetupRequestBody(buildNTLMAuthenticateForTest(username, domain, ntResp))
	result, err := h.SessionSetup(ctx, body)
	if err != nil {
		t.Fatalf("SESSION_SETUP TYPE_3 returned error: %v", err)
	}
	return result, sessionID
}

// TestCompleteNTLMAuth_DomainUserPassthrough (Test A): on a local miss, a
// successful NETLOGON network logon synthesizes a domain-user session with the
// DC-provided SIDs and a signing key derived from the DC SessionBaseKey.
func TestCompleteNTLMAuth_DomainUserPassthrough(t *testing.T) {
	const sid = "S-1-5-21-1111111111-2222222222-3333333333-1001"
	groupSIDs := []string{"S-1-5-21-1111111111-2222222222-3333333333-513"}
	uid := uint32(10001)
	gid := uint32(10001)

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
			UID:       uid,
			GID:       gid,
			SID:       sid,
			GroupSIDs: groupSIDs,
			Found:     true,
		},
	}
	h := newNetlogonFallbackHandler(t, nl, provider)

	result, sessionID := driveDomainTYPE3(t, h, "alice", "CORP")

	if result.Status.IsError() {
		t.Fatalf("expected success, got status 0x%08x", uint32(result.Status))
	}
	if !nl.called {
		t.Fatal("NETLOGON authenticator was never called on local miss")
	}

	// A session must have been created for the resolved domain user.
	sess, ok := h.GetSession(sessionID)
	if !ok {
		t.Fatalf("no session created for sessionID 0x%x", sessionID)
	}
	if sess.IsGuest {
		t.Fatal("domain-user fallback must not produce a guest session")
	}
	if sess.User == nil || sess.User.UID == nil || *sess.User.UID != uid {
		t.Fatalf("session user UID = %v, want %d", sess.User, uid)
	}
	if sess.User.SID != sid {
		t.Errorf("session user SID = %q, want %q", sess.User.SID, sid)
	}

	// PAC identity must be stamped from the DC result (groupSIDs, userSID).
	pacGroupSIDs, pacUserSID := sess.PACIdentity()
	if pacUserSID != sid {
		t.Errorf("PAC user SID = %q, want %q", pacUserSID, sid)
	}
	if len(pacGroupSIDs) != len(groupSIDs) || (len(pacGroupSIDs) > 0 && pacGroupSIDs[0] != groupSIDs[0]) {
		t.Errorf("PAC group SIDs = %v, want %v", pacGroupSIDs, groupSIDs)
	}

	// Signing must be configured (signing key present) from the DC base key.
	cs := sess.GetCryptoState()
	if cs == nil || len(cs.SigningKey) == 0 {
		t.Error("session signing key not configured from DC SessionBaseKey")
	}
}

// TestCompleteNTLMAuth_DomainUserFailClosed (Test B): a NETLOGON error must
// fall through to STATUS_LOGON_FAILURE with NO session created (never guest).
func TestCompleteNTLMAuth_DomainUserFailClosed(t *testing.T) {
	nl := &fakeNetlogon{err: context.DeadlineExceeded}
	// Provider present but irrelevant: the error short-circuits before resolve.
	h := newNetlogonFallbackHandler(t, nl, &fakeIdentityProvider{name: "netlogon"})

	result, sessionID := driveDomainTYPE3(t, h, "alice", "CORP")

	if result.Status != types.StatusLogonFailure {
		t.Fatalf("status = 0x%08x, want LOGON_FAILURE", uint32(result.Status))
	}
	if !nl.called {
		t.Fatal("NETLOGON authenticator was never called")
	}
	if sess, ok := h.GetSession(sessionID); ok {
		t.Fatalf("a session was created on NETLOGON failure (fail-closed violated): user=%v guest=%v", sess.User, sess.IsGuest)
	}
}

// TestCompleteNTLMAuth_NetlogonDisabled (Test C): with no authenticator
// injected, a domain user that misses locally yields the unchanged
// STATUS_LOGON_FAILURE (no fallback, no guest).
func TestCompleteNTLMAuth_NetlogonDisabled(t *testing.T) {
	h := newNetlogonFallbackHandler(t, nil, nil)

	result, _ := driveDomainTYPE3(t, h, "alice", "CORP")

	if result.Status != types.StatusLogonFailure {
		t.Fatalf("status = 0x%08x, want LOGON_FAILURE", uint32(result.Status))
	}
}

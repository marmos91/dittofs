package handlers

import (
	"context"
	"encoding/binary"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/auth"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// buildNTLMAuthenticateForTest builds an NTLM Type 3 (AUTHENTICATE) message
// containing the given username and domain in UTF-16LE plus an optional
// NtChallengeResponse blob. Use an empty ntResponse to skip NTLMv2 validation
// (drives the "no NT response → user lookup only" path); pass a 24+ byte blob
// to force ValidateNTLMv2Response to run (and, with garbage bytes, fail).
func buildNTLMAuthenticateForTest(username, domain string, ntResponse []byte) []byte {
	usernameBytes := encodeUTF16LE(username)
	domainBytes := encodeUTF16LE(domain)

	payloadOffset := 88
	domainOffset := payloadOffset
	userOffset := domainOffset + len(domainBytes)
	ntRespOffset := userOffset + len(usernameBytes)
	totalLen := ntRespOffset + len(ntResponse)

	msg := make([]byte, totalLen)

	copy(msg[0:8], auth.Signature)
	binary.LittleEndian.PutUint32(msg[8:12], uint32(auth.Authenticate))

	// NtChallengeResponse fields (length/maxLen/offset at 20..28)
	binary.LittleEndian.PutUint16(msg[20:22], uint16(len(ntResponse)))
	binary.LittleEndian.PutUint16(msg[22:24], uint16(len(ntResponse)))
	binary.LittleEndian.PutUint32(msg[24:28], uint32(ntRespOffset))

	// DomainName fields (length/maxLen/offset at 28..36)
	binary.LittleEndian.PutUint16(msg[28:30], uint16(len(domainBytes)))
	binary.LittleEndian.PutUint16(msg[30:32], uint16(len(domainBytes)))
	binary.LittleEndian.PutUint32(msg[32:36], uint32(domainOffset))

	// UserName fields (length/maxLen/offset at 36..44)
	binary.LittleEndian.PutUint16(msg[36:38], uint16(len(usernameBytes)))
	binary.LittleEndian.PutUint16(msg[38:40], uint16(len(usernameBytes)))
	binary.LittleEndian.PutUint32(msg[40:44], uint32(userOffset))

	// NegotiateFlags at offset 60 — Unicode strings, NTLM.
	binary.LittleEndian.PutUint32(msg[60:64], uint32(auth.FlagUnicode|auth.FlagNTLM))

	copy(msg[domainOffset:], domainBytes)
	copy(msg[userOffset:], usernameBytes)
	copy(msg[ntRespOffset:], ntResponse)

	return msg
}

// newInMemoryStoreForTest stands up an in-memory SQLite control-plane store
// suitable for wiring into runtime.New(...). Tests use this when they need
// completeNTLMAuth to take the userStore != nil branch (user-not-found,
// user-disabled, NTLMv2-validation-failure).
func newInMemoryStoreForTest(t *testing.T) store.Store {
	t.Helper()
	cfg := store.Config{
		Type:   "sqlite",
		SQLite: store.SQLiteConfig{Path: ":memory:"},
	}
	s, err := store.New(&cfg)
	if err != nil {
		t.Fatalf("store.New(:memory:): %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// reauthFixture sets up a pre-authenticated SMB2 session with a pending
// CHANGE_NOTIFY watcher and primes a re-auth handshake (TYPE_1) so the test
// can drive TYPE_3 with any payload. The returned cb fields capture the
// notify completion status so the caller can assert STATUS_NOTIFY_CLEANUP.
type reauthFixture struct {
	h            *Handler
	ctx          *SMBHandlerContext
	sessionID    uint64
	notifyFired  *atomic.Bool
	notifyStatus *atomic.Uint32
	authedUser   *models.User
}

// newReauthFixture seeds Handler state for the failed-re-auth scenarios.
// store may be nil; pass a real store when the test needs the userStore != nil
// branch in completeNTLMAuth.
func newReauthFixture(t *testing.T, cpStore store.Store) *reauthFixture {
	t.Helper()
	h := NewHandler()
	h.NotifyRegistry = NewNotifyRegistry()
	h.NtlmEnabled = true
	h.Registry = runtime.New(cpStore)

	const sessionID = uint64(0xdeadbeef)
	authedUser := &models.User{Username: "alice", Enabled: true}
	sess := h.CreateSessionWithUser(sessionID, "127.0.0.1:1", authedUser, "")
	if sess.LoggedOff.Load() {
		t.Fatal("seed session unexpectedly starts logged-off")
	}

	notifyFired := &atomic.Bool{}
	notifyStatus := &atomic.Uint32{}
	var fileID [16]byte
	copy(fileID[:], []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08})
	notify := &PendingNotify{
		FileID:    fileID,
		SessionID: sessionID,
		MessageID: 100,
		AsyncId:   200,
		WatchPath: "/share/dir",
		ShareName: "share",
		AsyncCallback: func(sid, mid, aid uint64, resp *ChangeNotifyResponse) error {
			notifyFired.Store(true)
			notifyStatus.Store(uint32(resp.GetStatus()))
			return nil
		},
	}
	if err := h.NotifyRegistry.Register(notify); err != nil {
		t.Fatalf("Register pending notify: %v", err)
	}

	// Drive TYPE_1 (NEGOTIATE) so the Handler stores a PendingAuth keyed to
	// this session with IsReauth=true.
	ctx := newTestContext(sessionID)
	negResult, err := h.SessionSetup(ctx, buildSessionSetupRequestBody(validNTLMNegotiateMessage()))
	if err != nil {
		t.Fatalf("SESSION_SETUP TYPE_1 returned error: %v", err)
	}
	if negResult.Status != types.StatusMoreProcessingRequired {
		t.Fatalf("TYPE_1 status = 0x%x, want MORE_PROCESSING_REQUIRED (0x%x)",
			negResult.Status, types.StatusMoreProcessingRequired)
	}
	pending, ok := h.GetPendingAuth(sessionID, ctx.ConnID)
	if !ok {
		t.Fatal("TYPE_1 did not store PendingAuth for re-auth")
	}
	if !pending.IsReauth {
		t.Fatal("PendingAuth.IsReauth must be true on re-auth handshake")
	}

	return &reauthFixture{
		h:            h,
		ctx:          ctx,
		sessionID:    sessionID,
		notifyFired:  notifyFired,
		notifyStatus: notifyStatus,
		authedUser:   authedUser,
	}
}

// assertSessionDestroyedAndNotifyCleaned verifies the post-conditions every
// failed re-auth path MUST satisfy per MS-SMB2 §3.3.5.5.3:
//
//   - TYPE_3 response status is STATUS_LOGON_FAILURE (no guest downgrade).
//   - Session record remains in the manager (so in-flight handler goroutines
//     can still sign their responses) but is flagged LoggedOff so subsequent
//     ops are rejected with STATUS_USER_SESSION_DELETED via prepareDispatch.
//   - Pending CHANGE_NOTIFY completes with STATUS_NOTIFY_CLEANUP and the
//     watcher is unregistered.
func (f *reauthFixture) assertSessionDestroyedAndNotifyCleaned(t *testing.T, status types.Status) {
	t.Helper()

	if status != types.StatusLogonFailure {
		t.Errorf("TYPE_3 status = 0x%x, want LOGON_FAILURE (0x%x) — failed re-auth must not downgrade to guest",
			uint32(status), uint32(types.StatusLogonFailure))
	}

	gotSess, exists := f.h.GetSession(f.sessionID)
	if !exists {
		t.Fatal("session disappeared from manager after failed re-auth; in-flight signing would break")
	}
	if !gotSess.LoggedOff.Load() {
		t.Error("session not flagged LoggedOff after failed re-auth; subsequent ops will not return STATUS_USER_SESSION_DELETED")
	}
	if gotSess.IsGuest {
		t.Error("session was downgraded to guest after failed re-auth — must not happen per MS-SMB2 §3.3.5.5.3")
	}

	// notify cleanup is dispatched on a goroutine — settle briefly.
	deadline := time.Now().Add(2 * time.Second)
	for !f.notifyFired.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !f.notifyFired.Load() {
		t.Fatal("pending CHANGE_NOTIFY was not completed after failed re-auth (expected STATUS_NOTIFY_CLEANUP)")
	}
	if got := types.Status(f.notifyStatus.Load()); got != types.StatusNotifyCleanup {
		t.Errorf("CHANGE_NOTIFY completion status = 0x%08x, want STATUS_NOTIFY_CLEANUP (0x%08x)",
			uint32(got), uint32(types.StatusNotifyCleanup))
	}
	if f.h.NotifyRegistry.WatcherCount() != 0 {
		t.Errorf("notify watcher still registered after failed re-auth: %d", f.h.NotifyRegistry.WatcherCount())
	}
}

// TestSessionSetup_FailedReauth_NoUserStore_DestroysSession exercises the
// "no UserStore configured" tail of completeNTLMAuth — `runtime.New(nil)`
// returns a nil userStore from GetUserStore, so the per-user lookup block
// is skipped entirely and execution falls through to the final
// `if pending.IsReauth { destroySessionOnReauthFailure(...) }` guard.
//
// This is the smbtorture invalid-reauth wire flow at its simplest: the
// TYPE_3 carries a username DittoFS could not authenticate against (because
// there's no userStore to authenticate against), so the session MUST be
// destroyed rather than silently downgraded to guest.
//
// Reference Samba test: source4/torture/smb2/notify.c::
// torture_smb2_notify_invalid_reauth (smb2.notify.invalid-reauth, #473).
func TestSessionSetup_FailedReauth_NoUserStore_DestroysSession(t *testing.T) {
	f := newReauthFixture(t, nil)

	type3Body := buildSessionSetupRequestBody(
		buildNTLMAuthenticateForTest("__none__invalid__none__", "__none__invalid__none__", nil),
	)
	result, err := f.h.SessionSetup(f.ctx, type3Body)
	if err != nil {
		t.Fatalf("SESSION_SETUP TYPE_3 returned error: %v", err)
	}

	f.assertSessionDestroyedAndNotifyCleaned(t, result.Status)
}

// TestSessionSetup_FailedReauth_UnknownUser_DestroysSession covers the
// userStore.GetUser() returns ErrUserNotFound branch: a real UserStore is
// wired, but the principal in the TYPE_3 does not exist. Execution still
// falls through to the final IsReauth guard (the lookup error logs at debug
// and continues), and the session MUST be destroyed.
func TestSessionSetup_FailedReauth_UnknownUser_DestroysSession(t *testing.T) {
	f := newReauthFixture(t, newInMemoryStoreForTest(t))

	type3Body := buildSessionSetupRequestBody(
		buildNTLMAuthenticateForTest("nosuchuser", "WORKGROUP", nil),
	)
	result, err := f.h.SessionSetup(f.ctx, type3Body)
	if err != nil {
		t.Fatalf("SESSION_SETUP TYPE_3 returned error: %v", err)
	}

	f.assertSessionDestroyedAndNotifyCleaned(t, result.Status)
}

// TestSessionSetup_FailedReauth_UserDisabled_DestroysSession covers the
// "user found but !user.Enabled" branch (session_setup.go:919-923). This is
// the only branch that calls destroySessionOnReauthFailure from INSIDE the
// userStore != nil block (not via the fall-through tail), so it must be
// exercised separately to defend against future refactors that accidentally
// re-route disabled users to guest.
func TestSessionSetup_FailedReauth_UserDisabled_DestroysSession(t *testing.T) {
	cpStore := newInMemoryStoreForTest(t)
	// Create then disable. GORM substitutes the `default:true` on Create
	// when the bool is its zero value (same gotcha as the per-share ACL
	// canonicalization toggle), so the disabled state must be applied via
	// UpdateUser, which uses Select(...) and respects explicit false.
	disabled := &models.User{Username: "bob", Enabled: true}
	disabled.SetNTHashFromPassword("anyhash")
	if _, err := cpStore.CreateUser(context.Background(), disabled); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	disabled.Enabled = false
	if err := cpStore.UpdateUser(context.Background(), disabled); err != nil {
		t.Fatalf("UpdateUser(Enabled=false): %v", err)
	}

	f := newReauthFixture(t, cpStore)

	type3Body := buildSessionSetupRequestBody(
		buildNTLMAuthenticateForTest("bob", "WORKGROUP", nil),
	)
	result, err := f.h.SessionSetup(f.ctx, type3Body)
	if err != nil {
		t.Fatalf("SESSION_SETUP TYPE_3 returned error: %v", err)
	}

	f.assertSessionDestroyedAndNotifyCleaned(t, result.Status)
}

// TestSessionSetup_FailedReauth_NTLMv2ValidationFails_DestroysSession covers
// the wrong-password branch: a real enabled user with an NT hash, but the
// TYPE_3 carries a NtChallengeResponse that doesn't validate against
// (ServerChallenge, ClientBlob, NTLMv2Hash). Hits the explicit
// destroySessionOnReauthFailure at session_setup.go:819 rather than the
// fall-through tail — this is the path that already existed before #473;
// it's pinned here so the guarantee survives.
func TestSessionSetup_FailedReauth_NTLMv2ValidationFails_DestroysSession(t *testing.T) {
	cpStore := newInMemoryStoreForTest(t)
	enabled := &models.User{
		Username: "carol",
		Enabled:  true,
	}
	enabled.SetNTHashFromPassword("the-real-password")
	if _, err := cpStore.CreateUser(context.Background(), enabled); err != nil {
		t.Fatalf("CreateUser(enabled): %v", err)
	}

	f := newReauthFixture(t, cpStore)

	// A structurally valid NTLMv2_RESPONSE (NTProofStr || CLIENT_CHALLENGE)
	// that fails the HMAC compare. The CLIENT_CHALLENGE follows MS-NLMP
	// §2.2.2.7: RespType=0x01, HiRespType=0x01, fixed 28-byte header, then a
	// single MsvAvEOL (AvId=0, AvLen=0) — so the malformed-structure gate
	// added for smb2.session.ntlmssp_bug14932 passes and execution still
	// reaches the HMAC check that returns ErrAuthenticationFailed.
	bogusNTResponse := make([]byte, 16+28+4)
	for i := 0; i < 16; i++ {
		bogusNTResponse[i] = 0xFF // bogus NTProofStr (the value HMAC compares against)
	}
	bogusNTResponse[16] = 0x01 // RespType
	bogusNTResponse[17] = 0x01 // HiRespType
	// Trailing 4 bytes after the 28-byte header are MsvAvEOL (already zero).

	type3Body := buildSessionSetupRequestBody(
		buildNTLMAuthenticateForTest("carol", "WORKGROUP", bogusNTResponse),
	)
	result, err := f.h.SessionSetup(f.ctx, type3Body)
	if err != nil {
		t.Fatalf("SESSION_SETUP TYPE_3 returned error: %v", err)
	}

	f.assertSessionDestroyedAndNotifyCleaned(t, result.Status)
}

// TestSessionSetup_FailedReauth_UnparseableType3_DestroysSession covers the
// ParseAuthenticate failure branch (session_setup.go:715-729). An NTLM TYPE_3
// header so short it fails to parse must also destroy the session — without
// the IsReauth gate added in #473 this branch fell through to
// createGuestSessionWithID and silently downgraded the existing identity.
func TestSessionSetup_FailedReauth_UnparseableType3_DestroysSession(t *testing.T) {
	f := newReauthFixture(t, nil)

	// Valid NTLM signature + TYPE_3 type but missing all the field tables —
	// ParseAuthenticate will reject it.
	truncated := make([]byte, 12)
	copy(truncated[0:8], auth.Signature)
	binary.LittleEndian.PutUint32(truncated[8:12], uint32(auth.Authenticate))

	type3Body := buildSessionSetupRequestBody(truncated)
	result, err := f.h.SessionSetup(f.ctx, type3Body)
	if err != nil {
		t.Fatalf("SESSION_SETUP TYPE_3 returned error: %v", err)
	}

	f.assertSessionDestroyedAndNotifyCleaned(t, result.Status)
}

// TestSessionSetup_UserWithoutNTHash_Rejected is the negative control for the
// no-NT-hash authentication bypass (audit #1132 HIGH). An enabled user that
// has no NT hash configured (no password ever set) must NOT be granted an
// authenticated session merely because a client knows the username — that is a
// missing-authentication / privilege-escalation vulnerability.
//
// Before the fix, completeNTLMAuth's "transitional mode" branch created a full
// Session with IsGuest=false and User=<the real user>, granting the user's
// identity and all share/file permissions with no credential check. The fix
// rejects the logon with STATUS_LOGON_FAILURE and creates no authenticated
// session.
func TestSessionSetup_UserWithoutNTHash_Rejected(t *testing.T) {
	cpStore := newInMemoryStoreForTest(t)
	// Enabled user with NO NT hash (no SetNTHashFromPassword call) -> hasNTHash
	// is false in completeNTLMAuth.
	noHash := &models.User{Username: "dave", Enabled: true}
	if _, err := cpStore.CreateUser(context.Background(), noHash); err != nil {
		t.Fatalf("CreateUser(no hash): %v", err)
	}
	if _, ok := noHash.GetNTHash(); ok {
		t.Fatal("test precondition: user unexpectedly has an NT hash")
	}

	h := NewHandler()
	h.NtlmEnabled = true
	h.Registry = runtime.New(cpStore)

	// Fresh session: NEGOTIATE (TYPE_1) then AUTHENTICATE (TYPE_3).
	ctx1 := newTestContext(0)
	negResult, err := h.SessionSetup(ctx1, buildSessionSetupRequestBody(validNTLMNegotiateMessage()))
	if err != nil {
		t.Fatalf("NEGOTIATE error: %v", err)
	}
	if negResult.Status != types.StatusMoreProcessingRequired {
		t.Fatalf("NEGOTIATE status = 0x%x, want MORE_PROCESSING_REQUIRED", negResult.Status)
	}
	sessionID := ctx1.SessionID

	// AUTHENTICATE with the username but a present NtChallengeResponse blob.
	// hasNTHash is false, so the validation block is skipped and execution
	// reaches the no-credential branch regardless of the response bytes.
	ntResponse := make([]byte, 16+28+4)
	ntResponse[16] = 0x01
	ntResponse[17] = 0x01
	ctx2 := newTestContext(sessionID)
	result, err := h.SessionSetup(ctx2, buildSessionSetupRequestBody(
		buildNTLMAuthenticateForTest("dave", "WORKGROUP", ntResponse),
	))
	if err != nil {
		t.Fatalf("AUTHENTICATE error: %v", err)
	}

	if result.Status != types.StatusLogonFailure {
		t.Errorf("AUTHENTICATE status = 0x%x, want STATUS_LOGON_FAILURE — a user with no NT hash must not authenticate",
			uint32(result.Status))
	}

	// No authenticated session for this user may exist.
	if sess, ok := h.GetSession(sessionID); ok && !sess.IsGuest && sess.User != nil {
		t.Errorf("authenticated session created for credential-less user %q (User=%+v); the no-NT-hash bypass is still open",
			sess.Username, sess.User)
	}
}

// TestTryReauthUpdate_ClearsKerberosPACIdentity is the regression net for the
// privilege-escalation class where a session first authenticates via Kerberos
// (its PAC carries AD group SIDs, possibly privileged) and is then re-
// authenticated via NTLM as a lower-privileged or anonymous user. The NTLM
// reauth carries no in-band PAC, so the session's AD credential set MUST revert
// to empty — otherwise the new identity would inherit the prior principal's PAC
// group SIDs and gain access on SID-keyed ACLs it should no longer have.
func TestTryReauthUpdate_ClearsKerberosPACIdentity(t *testing.T) {
	h := NewHandler()

	const sessionID = uint64(0xc0ffee)
	kerbUser := &models.User{Username: "alice", Enabled: true}
	sess := h.CreateSessionWithUser(sessionID, "127.0.0.1:1", kerbUser, "DITTOFS")

	// Simulate the prior Kerberos auth having stamped PAC identity on the session.
	sess.PACGroupSIDs = []string{
		"S-1-5-21-1111111111-2222222222-3333333333-512", // Domain Admins
		"S-1-5-21-1111111111-2222222222-3333333333-513", // Domain Users
	}
	sess.PACUserSID = "S-1-5-21-1111111111-2222222222-3333333333-1001"

	// Drive an NTLM reauth onto the same session as a different, lower-priv user.
	ntlmUser := &models.User{Username: "bob", Enabled: true}
	pending := &PendingAuth{SessionID: sessionID, IsReauth: true}
	if res := h.tryReauthUpdate(pending, "bob", "DITTOFS", ntlmUser, false); res == nil {
		t.Fatal("tryReauthUpdate returned nil for an existing session")
	}

	got, ok := h.GetSession(sessionID)
	if !ok {
		t.Fatal("session disappeared after reauth update")
	}
	if got.Username != "bob" {
		t.Errorf("Username = %q, want bob", got.Username)
	}
	if len(got.PACGroupSIDs) != 0 {
		t.Errorf("PACGroupSIDs not cleared after NTLM reauth: %v — stale AD groups leak privilege", got.PACGroupSIDs)
	}
	if got.PACUserSID != "" {
		t.Errorf("PACUserSID not cleared after NTLM reauth: %q", got.PACUserSID)
	}
}

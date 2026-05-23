package handlers

import (
	"encoding/binary"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/auth"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// encodeUTF16LEForTest encodes an ASCII string as UTF-16LE for embedding in
// an NTLM AUTHENTICATE message.
func encodeUTF16LEForTest(s string) []byte {
	buf := make([]byte, len(s)*2)
	for i, r := range s {
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(r))
	}
	return buf
}

// buildMinimalNTLMAuthenticate builds an NTLM Type 3 (AUTHENTICATE) message
// containing the given username and domain in UTF-16LE. Suitable for triggering
// the user-not-found / re-auth-fails code path in completeNTLMAuth — no
// NtChallengeResponse is included, so NTLMv2 validation does not run; the
// flow lands on "user not found in UserStore" (which is the spec-equivalent
// outcome for the invalid credentials the smbtorture invalid-reauth test
// sends).
func buildMinimalNTLMAuthenticate(username, domain string) []byte {
	usernameBytes := encodeUTF16LEForTest(username)
	domainBytes := encodeUTF16LEForTest(domain)

	payloadOffset := 88
	domainOffset := payloadOffset
	userOffset := domainOffset + len(domainBytes)

	msg := make([]byte, userOffset+len(usernameBytes))

	copy(msg[0:8], auth.Signature)
	binary.LittleEndian.PutUint32(msg[8:12], uint32(auth.Authenticate))

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

	return msg
}

// TestSessionSetup_FailedReauth_DestroysSessionAndCleansNotify mirrors the
// smbtorture invalid-reauth wire flow at the unit-test boundary:
//
//  1. An authenticated session exists with a pending CHANGE_NOTIFY watcher.
//  2. The client re-authenticates that session with invalid credentials —
//     SESSION_SETUP carries an NTLM TYPE_1 first (re-auth handshake start),
//     then a TYPE_3 with a username the UserStore does not know.
//  3. The TYPE_3 must return STATUS_LOGON_FAILURE (per MS-SMB2 §3.3.5.5.3)
//     and must NOT downgrade the session to guest.
//  4. The pending CHANGE_NOTIFY MUST complete with STATUS_NOTIFY_CLEANUP.
//  5. The session MUST be flagged LoggedOff so subsequent ops are rejected
//     with STATUS_USER_SESSION_DELETED via prepareDispatch.
//
// Reference Samba test: source4/torture/smb2/notify.c::
// torture_smb2_notify_invalid_reauth (the smb2.notify.invalid-reauth case
// tracked under issue #473).
func TestSessionSetup_FailedReauth_DestroysSessionAndCleansNotify(t *testing.T) {
	h := NewHandler()
	h.NotifyRegistry = NewNotifyRegistry()
	h.NtlmEnabled = true
	// completeNTLMAuth dereferences h.Registry.GetUserStore(); supply an
	// empty Runtime so the "unknown user → fall through" path is exercised
	// without needing a real backing store.
	h.Registry = runtime.New(nil)

	// Stand up an authenticated session as if a prior SESSION_SETUP had
	// succeeded. We do not need a real UserStore entry to exist — the test
	// drives the re-auth failure through the "no NTLMv2 validation possible
	// → user not found" branch.
	const sessionID = uint64(0xdeadbeef)
	authedUser := &models.User{Username: "alice", Enabled: true}
	sess := h.CreateSessionWithUser(sessionID, "127.0.0.1:1", authedUser, "")
	if sess.LoggedOff.Load() {
		t.Fatal("seed session unexpectedly starts logged-off")
	}

	// Register a pending CHANGE_NOTIFY on a directory handle belonging to the
	// session. The async callback records the status it was invoked with so
	// the test can assert STATUS_NOTIFY_CLEANUP.
	var notifyFired atomic.Bool
	var notifyStatus atomic.Uint32
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

	// Drive the re-auth TYPE_1 (NEGOTIATE) so the Handler stores a
	// PendingAuth with IsReauth=true keyed to this session.
	ctx := newTestContext(sessionID)
	negotiateBody := buildSessionSetupRequestBody(validNTLMNegotiateMessage())
	negResult, err := h.SessionSetup(ctx, negotiateBody)
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

	// Drive the re-auth TYPE_3 (AUTHENTICATE) with a username the UserStore
	// does not know — mirrors smbtorture's "__none__invalid__none__" creds.
	type3Body := buildSessionSetupRequestBody(
		buildMinimalNTLMAuthenticate("__none__invalid__none__", "__none__invalid__none__"),
	)
	authResult, err := h.SessionSetup(ctx, type3Body)
	if err != nil {
		t.Fatalf("SESSION_SETUP TYPE_3 returned error: %v", err)
	}

	// (a) Re-auth must fail with STATUS_LOGON_FAILURE — never silently
	// downgrade to a guest session.
	if authResult.Status != types.StatusLogonFailure {
		t.Errorf("TYPE_3 status = 0x%x, want LOGON_FAILURE (0x%x) — failed re-auth must not downgrade to guest",
			uint32(authResult.Status), uint32(types.StatusLogonFailure))
	}

	// (b) The session record must remain in the manager (so in-flight
	// goroutines can sign their responses) but be flagged LoggedOff so
	// prepareDispatch rejects subsequent requests with USER_SESSION_DELETED.
	gotSess, exists := h.GetSession(sessionID)
	if !exists {
		t.Fatal("session disappeared from manager after failed re-auth; in-flight signing would break")
	}
	if !gotSess.LoggedOff.Load() {
		t.Error("session not flagged LoggedOff after failed re-auth; subsequent ops will not return STATUS_USER_SESSION_DELETED")
	}
	if gotSess.IsGuest {
		t.Error("session was downgraded to guest after failed re-auth — must not happen per MS-SMB2 §3.3.5.5.3")
	}

	// (c) The pending CHANGE_NOTIFY must have been completed with
	// STATUS_NOTIFY_CLEANUP. The cleanup is dispatched on a goroutine, so
	// allow a brief settle window before failing — the test deadlocks would
	// otherwise mask the real issue.
	deadline := time.Now().Add(2 * time.Second)
	for !notifyFired.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !notifyFired.Load() {
		t.Fatal("pending CHANGE_NOTIFY was not completed after failed re-auth (expected STATUS_NOTIFY_CLEANUP)")
	}
	if got := types.Status(notifyStatus.Load()); got != types.StatusNotifyCleanup {
		t.Errorf("CHANGE_NOTIFY completion status = 0x%08x, want STATUS_NOTIFY_CLEANUP (0x%08x)",
			uint32(got), uint32(types.StatusNotifyCleanup))
	}
	if h.NotifyRegistry.WatcherCount() != 0 {
		t.Errorf("notify watcher still registered after failed re-auth: %d", h.NotifyRegistry.WatcherCount())
	}
}

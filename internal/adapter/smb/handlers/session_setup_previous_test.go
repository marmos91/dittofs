package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/auth"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// sessionSetupBodyWithPrevious is a SESSION_SETUP body naming prevID in
// PreviousSessionId.
func sessionSetupBodyWithPrevious(securityBuffer []byte, prevID uint64) []byte {
	body := buildSessionSetupRequestBody(securityBuffer)
	binary.LittleEndian.PutUint64(body[sessionSetupPreviousSessionIDOffset:], prevID)
	return body
}

// validNTLMv2Response is an NTLMv2 response that proves password for
// username/domain against serverChallenge.
func validNTLMv2Response(password, username, domain string, serverChallenge [8]byte) []byte {
	blob := make([]byte, 28+4) // NTLMv2_CLIENT_CHALLENGE header + MsvAvEOL
	blob[0], blob[1] = 0x01, 0x01
	hash := auth.ComputeNTLMv2Hash(auth.ComputeNTHash(password), username, domain)
	mac := hmac.New(md5.New, hash[:])
	mac.Write(serverChallenge[:])
	mac.Write(blob)
	return append(mac.Sum(nil), blob...)
}

// previousSessionFixture holds alice's established session and a handler
// whose user store knows alice, bob and a separate case-distinct ALICE.
type previousSessionFixture struct {
	h      *Handler
	prevID uint64
}

func newPreviousSessionFixture(t *testing.T) *previousSessionFixture {
	t.Helper()
	cpStore := newInMemoryStoreForTest(t)
	for _, name := range []string{"alice", "bob", "ALICE"} {
		u := &models.User{Username: name, Enabled: true}
		u.SetNTHashFromPassword(name + "-pw")
		if _, err := cpStore.CreateUser(context.Background(), u); err != nil {
			t.Fatalf("CreateUser(%s): %v", name, err)
		}
	}
	h := NewHandler()
	h.NtlmEnabled = true
	h.Registry = newTestRuntime(t, cpStore)

	alice, err := cpStore.GetUser(context.Background(), "alice")
	if err != nil {
		t.Fatalf("GetUser(alice): %v", err)
	}
	prevID := h.GenerateSessionID()
	h.CreateSessionWithUser(prevID, "127.0.0.1:1", alice, "")
	return &previousSessionFixture{h: h, prevID: prevID}
}

// signIn runs a NEGOTIATE/AUTHENTICATE exchange naming the fixture's session
// as PreviousSessionId on both legs and returns the final status. An empty
// username sends an anonymous AUTHENTICATE.
func (f *previousSessionFixture) signIn(t *testing.T, username, password string) types.Status {
	t.Helper()
	ctx1 := newTestContext(0)
	neg, err := f.h.SessionSetup(ctx1, sessionSetupBodyWithPrevious(validNTLMNegotiateMessage(), f.prevID))
	if err != nil || neg.Status != types.StatusMoreProcessingRequired {
		t.Fatalf("NEGOTIATE: status=0x%x err=%v", neg.Status, err)
	}
	pending, ok := f.h.GetPendingAuth(ctx1.SessionID, ctx1.ConnID)
	if !ok {
		t.Fatal("NEGOTIATE stored no pending auth")
	}
	var ntResponse []byte
	if username != "" {
		ntResponse = validNTLMv2Response(password, username, "WORKGROUP", pending.ServerChallenge)
	}
	ctx2 := newTestContext(ctx1.SessionID)
	res, err := f.h.SessionSetup(ctx2, sessionSetupBodyWithPrevious(
		buildNTLMAuthenticateForTest(username, "WORKGROUP", ntResponse), f.prevID))
	if err != nil {
		t.Fatalf("AUTHENTICATE: %v", err)
	}
	return res.Status
}

func (f *previousSessionFixture) prevLoggedOff(t *testing.T) bool {
	t.Helper()
	sess, ok := f.h.GetSession(f.prevID)
	return !ok || sess.LoggedOff.Load()
}

// The previous session survives every SESSION_SETUP that does not end in a
// sign-in as its own user, and is torn down by one that does (MS-SMB2
// 3.3.5.5.3).
func TestSessionSetup_PreviousSessionID_TeardownNeedsSameUserSignIn(t *testing.T) {
	t.Run("unanswered NEGOTIATE leaves it", func(t *testing.T) {
		f := newPreviousSessionFixture(t)
		ctx := newTestContext(0)
		if _, err := f.h.SessionSetup(ctx, sessionSetupBodyWithPrevious(validNTLMNegotiateMessage(), f.prevID)); err != nil {
			t.Fatalf("NEGOTIATE: %v", err)
		}
		if f.prevLoggedOff(t) {
			t.Fatal("previous session logged off by a SESSION_SETUP that authenticated nobody")
		}
	})

	t.Run("wrong password leaves it", func(t *testing.T) {
		f := newPreviousSessionFixture(t)
		if st := f.signIn(t, "alice", "not-alice-pw"); st != types.StatusLogonFailure {
			t.Fatalf("status = 0x%x, want LOGON_FAILURE", st)
		}
		if f.prevLoggedOff(t) {
			t.Fatal("previous session logged off by a failed sign-in")
		}
	})

	t.Run("anonymous sign-in leaves it", func(t *testing.T) {
		f := newPreviousSessionFixture(t)
		if st := f.signIn(t, "", ""); st != types.StatusSuccess {
			t.Fatalf("status = 0x%x, want SUCCESS", st)
		}
		if f.prevLoggedOff(t) {
			t.Fatal("previous session logged off by an anonymous sign-in")
		}
	})

	t.Run("other user's sign-in leaves it", func(t *testing.T) {
		f := newPreviousSessionFixture(t)
		if st := f.signIn(t, "bob", "bob-pw"); st != types.StatusSuccess {
			t.Fatalf("status = 0x%x, want SUCCESS", st)
		}
		if f.prevLoggedOff(t) {
			t.Fatal("previous session logged off by another user's sign-in")
		}
	})

	t.Run("case-distinct account's sign-in leaves it", func(t *testing.T) {
		f := newPreviousSessionFixture(t)
		if st := f.signIn(t, "ALICE", "ALICE-pw"); st != types.StatusSuccess {
			t.Fatalf("status = 0x%x, want SUCCESS", st)
		}
		if f.prevLoggedOff(t) {
			t.Fatal("previous session logged off by a different account whose name differs only by case")
		}
	})

	t.Run("same user's sign-in tears it down", func(t *testing.T) {
		f := newPreviousSessionFixture(t)
		if st := f.signIn(t, "alice", "alice-pw"); st != types.StatusSuccess {
			t.Fatalf("status = 0x%x, want SUCCESS", st)
		}
		if !f.prevLoggedOff(t) {
			t.Fatal("previous session survived its own user's reconnect")
		}
	})
}

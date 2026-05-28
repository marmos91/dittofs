package handlers

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// buildBindRequestBody builds a SESSION_SETUP request body with the binding
// flag set. Otherwise identical to buildSessionSetupRequestBody.
func buildBindRequestBody() []byte {
	body := buildSessionSetupRequestBody(nil)
	body[sessionSetupFlagsOffset] = SMB2_SESSION_FLAG_BINDING
	// Zero PreviousSessionID explicitly so parsing matches the test scenario.
	binary.LittleEndian.PutUint64(body[16:24], 0)
	return body
}

func TestSessionSetup_BindRejectsZeroSessionID(t *testing.T) {
	h := NewHandler()
	ctx := newTestContext(0) // SessionID=0

	result, err := h.SessionSetup(ctx, buildBindRequestBody())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != types.StatusInvalidParameter {
		t.Fatalf("status=0x%x, want StatusInvalidParameter (0x%x)", result.Status, types.StatusInvalidParameter)
	}
}

func TestSessionSetup_BindRejectsMissingSession(t *testing.T) {
	h := NewHandler()
	ctx := newTestContext(99999) // session does not exist

	result, err := h.SessionSetup(ctx, buildBindRequestBody())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != types.StatusUserSessionDeleted {
		t.Fatalf("status=0x%x, want StatusUserSessionDeleted (0x%x)", result.Status, types.StatusUserSessionDeleted)
	}
}

func TestSessionSetup_BindRejectsDialectBelow300(t *testing.T) {
	h := NewHandler()
	sess := h.CreateSession("127.0.0.1:1", false, "alice", "WORKGROUP")
	ctx := newTestContext(sess.SessionID)
	ctx.ConnCryptoState = &mockCryptoState{dialect: types.Dialect0210} // SMB 2.1

	result, err := h.SessionSetup(ctx, buildBindRequestBody())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != types.StatusRequestNotAccepted {
		t.Fatalf("status=0x%x, want StatusRequestNotAccepted (0x%x)", result.Status, types.StatusRequestNotAccepted)
	}
}

func TestSessionSetup_BindRejectsGuestSession(t *testing.T) {
	h := NewHandler()
	sess := h.CreateSession("127.0.0.1:1", true, "", "") // guest
	ctx := newTestContext(sess.SessionID)
	ctx.ConnCryptoState = &mockCryptoState{dialect: types.Dialect0311}

	result, err := h.SessionSetup(ctx, buildBindRequestBody())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != types.StatusNotSupported {
		t.Fatalf("status=0x%x, want StatusNotSupported (0x%x)", result.Status, types.StatusNotSupported)
	}
}

func TestSessionSetup_BindRejectsNullSession(t *testing.T) {
	h := NewHandler()
	// Create a null session: no username, not guest.
	sess := h.CreateSession("127.0.0.1:1", false, "", "")
	if !sess.IsNull {
		t.Fatalf("expected sess.IsNull=true")
	}
	ctx := newTestContext(sess.SessionID)
	ctx.ConnCryptoState = &mockCryptoState{dialect: types.Dialect0311}

	result, err := h.SessionSetup(ctx, buildBindRequestBody())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != types.StatusNotSupported {
		t.Fatalf("status=0x%x, want StatusNotSupported (0x%x)", result.Status, types.StatusNotSupported)
	}
}

// TestSessionSetup_RejectsNonBindingOnUnboundConnection_SMB3 verifies that a
// non-binding SESSION_SETUP for an existing 3.x session arriving on a
// connection that is neither the session's origin nor a previously bound
// channel returns STATUS_USER_SESSION_DELETED (MS-SMB2 §3.3.5.5 step 1).
// Mirrors the assertion at smbtorture session.c:2799 in
// test_session_bind_negative_smbXtoX after a bind has been rejected on the
// same transport.
func TestSessionSetup_RejectsNonBindingOnUnboundConnection_SMB3(t *testing.T) {
	for _, dialect := range []types.Dialect{types.Dialect0300, types.Dialect0302, types.Dialect0311} {
		t.Run(dialect.String(), func(t *testing.T) {
			h := NewHandler()
			sess := h.CreateSession("127.0.0.1:1", false, "alice", "WORKGROUP")
			sess.OriginConnID = 1 // session was created on ConnID=1

			// Build a context on a different ConnID with no bound channel.
			ctx := NewSMBHandlerContext(context.Background(),
				"127.0.0.1:99", sess.SessionID, 0, 2)
			ctx.ConnID = 2
			ctx.ConnCryptoState = &mockCryptoState{dialect: dialect}

			// Plain non-binding SESSION_SETUP body (no BIND flag).
			body := buildSessionSetupRequestBody(nil)

			result, err := h.SessionSetup(ctx, body)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.Status != types.StatusUserSessionDeleted {
				t.Fatalf("status=0x%x, want StatusUserSessionDeleted (0x%x)", result.Status, types.StatusUserSessionDeleted)
			}
		})
	}
}

// TestSessionSetup_AllowsNonBindingOnBoundChannel_SMB3 verifies the inverse —
// a non-binding SESSION_SETUP on a connection that already holds a channel
// for the session (e.g. for re-authentication) bypasses the cross-connection
// gate and proceeds into the auth flow.
func TestSessionSetup_AllowsNonBindingOnBoundChannel_SMB3(t *testing.T) {
	h := NewHandler()
	sess := h.CreateSession("127.0.0.1:1", false, "alice", "WORKGROUP")
	sess.OriginConnID = 1

	// Register ConnID=2 as a bound channel.
	sess.AddChannel(&session.Channel{ConnID: 2, Dialect: types.Dialect0311})

	ctx := NewSMBHandlerContext(context.Background(),
		"127.0.0.1:99", sess.SessionID, 0, 2)
	ctx.ConnID = 2
	ctx.ConnCryptoState = &mockCryptoState{dialect: types.Dialect0311}

	body := buildSessionSetupRequestBody(nil)

	result, err := h.SessionSetup(ctx, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No NTLM token + bound channel reaches the guest-session path.
	if result.Status == types.StatusUserSessionDeleted {
		t.Fatalf("status=StatusUserSessionDeleted, bound channel should bypass the cross-connection gate")
	}
}

// TestSessionSetup_Bind_InvalidWithoutNTLM verifies that a bind request on
// any SMB 3.x dialect reaches handleNTLMNegotiateBinding (past the dialect
// gate) and rejects with StatusInvalidParameter when no NTLM TYPE_1 token
// is present. Covers both 3.0 and 3.1.1 since they share the code path
// after the dialect branch; a full NTLM handshake test requires user-store
// setup and is covered by the smbtorture flow.
func TestSessionSetup_Bind_InvalidWithoutNTLM(t *testing.T) {
	for _, dialect := range []types.Dialect{types.Dialect0300, types.Dialect0302, types.Dialect0311} {
		t.Run(dialect.String(), func(t *testing.T) {
			h := NewHandler()
			sess := h.CreateSession("127.0.0.1:1", false, "alice", "WORKGROUP")
			ctx := newTestContext(sess.SessionID)
			ctx.ConnCryptoState = &mockCryptoState{dialect: dialect}

			result, err := h.SessionSetup(ctx, buildBindRequestBody())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.Status != types.StatusInvalidParameter {
				t.Fatalf("status=0x%x, want StatusInvalidParameter (no NTLM token in bind req)", result.Status)
			}
		})
	}
}

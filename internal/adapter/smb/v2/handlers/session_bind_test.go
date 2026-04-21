package handlers

import (
	"encoding/binary"
	"testing"

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

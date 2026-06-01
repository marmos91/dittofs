package handlers

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/internal/adapter/smb/signing"
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

// TestSessionSetup_BindRejectsGMACSessionWithNonGMACChannel covers Samba
// smb2_sesssetup.c:724-729: once the session has negotiated AES-128-GMAC,
// a bind on a channel that did not also negotiate GMAC must return
// REQUEST_OUT_OF_SEQUENCE. Mirrors smbtorture bind_negative_smb3signG*.
func TestSessionSetup_BindRejectsGMACSessionWithNonGMACChannel(t *testing.T) {
	h := NewHandler()
	sess := h.CreateSession("127.0.0.1:1", false, "alice", "WORKGROUP")
	sess.Dialect = types.Dialect0311
	sess.SigningAlgo = signing.SigningAlgAESGMAC

	ctx := newTestContext(sess.SessionID)
	ctx.ConnCryptoState = &mockCryptoState{
		dialect:            types.Dialect0311,
		signingAlgorithmId: signing.SigningAlgAESCMAC,
	}

	result, err := h.SessionSetup(ctx, buildBindRequestBody())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != types.StatusRequestOutOfSequence {
		t.Fatalf("status=0x%x, want StatusRequestOutOfSequence (0x%x)", result.Status, types.StatusRequestOutOfSequence)
	}
}

// TestSessionSetup_BindRejectsGMACChannelWithNonGMACSession covers Samba
// smb2_sesssetup.c:730-735: a channel that negotiated GMAC cannot bind to a
// session whose signing algorithm is something else — must return
// NOT_SUPPORTED. Mirrors smbtorture bind_negative_smb3sign[CH]toG /
// bind_negative_smb3sneXtoG.
func TestSessionSetup_BindRejectsGMACChannelWithNonGMACSession(t *testing.T) {
	h := NewHandler()
	sess := h.CreateSession("127.0.0.1:1", false, "alice", "WORKGROUP")
	sess.Dialect = types.Dialect0311
	sess.SigningAlgo = signing.SigningAlgAESCMAC

	ctx := newTestContext(sess.SessionID)
	ctx.ConnCryptoState = &mockCryptoState{
		dialect:            types.Dialect0311,
		signingAlgorithmId: signing.SigningAlgAESGMAC,
	}

	result, err := h.SessionSetup(ctx, buildBindRequestBody())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != types.StatusNotSupported {
		t.Fatalf("status=0x%x, want StatusNotSupported (0x%x)", result.Status, types.StatusNotSupported)
	}
}

// TestSessionSetup_BindRejectsDialectMismatch covers Samba
// smb2_sesssetup.c:752-757: bind must reject a channel whose negotiated
// dialect differs from the session's. Mirrors smbtorture
// bind_negative_smb2to3* (session 2.x ↔ channel 3.x) and
// bind_negative_smb3to3* (session 3.0.2 ↔ channel 3.1.1).
func TestSessionSetup_BindRejectsDialectMismatch(t *testing.T) {
	h := NewHandler()
	sess := h.CreateSession("127.0.0.1:1", false, "alice", "WORKGROUP")
	sess.Dialect = types.Dialect0302
	sess.SigningAlgo = signing.SigningAlgAESCMAC

	ctx := newTestContext(sess.SessionID)
	ctx.ConnCryptoState = &mockCryptoState{
		dialect:            types.Dialect0311,
		signingAlgorithmId: signing.SigningAlgAESCMAC,
	}

	result, err := h.SessionSetup(ctx, buildBindRequestBody())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != types.StatusInvalidParameter {
		t.Fatalf("status=0x%x, want StatusInvalidParameter (0x%x)", result.Status, types.StatusInvalidParameter)
	}
}

// TestSessionSetup_BindRejectsCipherMismatch covers Samba
// smb2_sesssetup.c:759-764: the cipher negotiated on the bound channel must
// match the session's. Mirrors smbtorture bind_negative_smb3encGtoC*.
func TestSessionSetup_BindRejectsCipherMismatch(t *testing.T) {
	h := NewHandler()
	sess := h.CreateSession("127.0.0.1:1", false, "alice", "WORKGROUP")
	sess.Dialect = types.Dialect0311
	sess.SigningAlgo = signing.SigningAlgAESCMAC
	sess.CipherId = types.CipherAES128GCM

	ctx := newTestContext(sess.SessionID)
	ctx.ConnCryptoState = &mockCryptoState{
		dialect:            types.Dialect0311,
		signingAlgorithmId: signing.SigningAlgAESCMAC,
		cipherId:           types.CipherAES128CCM,
	}

	result, err := h.SessionSetup(ctx, buildBindRequestBody())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != types.StatusInvalidParameter {
		t.Fatalf("status=0x%x, want StatusInvalidParameter (0x%x)", result.Status, types.StatusInvalidParameter)
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

// TestSessionSetup_BindRoutesKerberosToken verifies that a SPNEGO/Kerberos
// AP-REQ presented on a binding SESSION_SETUP is routed to the Kerberos bind
// path (completeKerberosBind), not the NTLM negotiate handshake that rejected
// any non-NTLM token before #686. With no KerberosService configured the krb
// path returns STATUS_LOGON_FAILURE — distinct from the NTLM-path
// STATUS_INVALID_PARAMETER, which is what proves the routing. The full
// authenticated bind and the identity-mismatch ACCESS_DENIED path require a
// live KDC and are covered by smbtorture smb2.session.bind1/bind2/bind_invalid_auth.
func TestSessionSetup_BindRoutesKerberosToken(t *testing.T) {
	h := NewHandler()
	// No KerberosService configured -> completeKerberosBind returns logon failure.
	sess := h.CreateSession("127.0.0.1:1", false, "alice", "WORKGROUP")
	ctx := newTestContext(sess.SessionID)
	ctx.ConnCryptoState = &mockCryptoState{dialect: types.Dialect0311}

	dummyAPReq := []byte{0x30, 0x05, 0x01, 0x02, 0x03, 0x04, 0x05}
	body := buildSessionSetupRequestBody(wrapKerberosInSPNEGO(dummyAPReq))
	body[sessionSetupFlagsOffset] = SMB2_SESSION_FLAG_BINDING
	binary.LittleEndian.PutUint64(body[16:24], 0)

	result, err := h.SessionSetup(ctx, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != types.StatusLogonFailure {
		t.Fatalf("status=0x%x, want StatusLogonFailure (Kerberos bind routing; NTLM path would return InvalidParameter)", result.Status)
	}
}

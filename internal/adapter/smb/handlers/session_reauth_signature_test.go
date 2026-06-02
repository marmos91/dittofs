package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/header"
	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/internal/adapter/smb/signing"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// buildSignedReauthRequest builds the raw wire bytes (header + body) of a
// non-binding SESSION_SETUP request targeting sessionID, signs the header+body
// with signKey using CMAC, and sets the SMB2_FLAGS_SIGNED bit. When signKey is
// nil the request is left with the SIGNED bit set but a zero signature (the
// "fresh-init client signed with a key the server cannot match" case).
func buildSignedReauthRequest(t *testing.T, sessionID uint64, signKey []byte) []byte {
	t.Helper()

	body := buildSessionSetupRequestBody(wrapInSPNEGO(validNTLMNegotiateMessage()))
	hdr := &header.SMB2Header{
		Command:   types.SMB2SessionSetup,
		Flags:     types.FlagSigned,
		MessageID: 4,
		SessionID: sessionID,
	}
	raw := append(hdr.Encode(), body...)

	if signKey != nil {
		signer := signing.NewCMACSigner(signKey)
		sig := signer.Sign(raw)
		copy(raw[48:64], sig[:])
	}
	return raw
}

// newReauthContext builds a handler context on connID carrying the raw request
// bytes, mirroring how the dispatch layer threads ctx.RawRequest.
func newReauthContext(sessionID uint64, connID uint64, raw []byte) *SMBHandlerContext {
	ctx := NewSMBHandlerContext(context.Background(), "127.0.0.1:99", sessionID, 0, connID)
	ctx.ConnID = connID
	ctx.ConnCryptoState = &mockCryptoState{dialect: types.Dialect0311}
	ctx.RawRequest = raw
	return ctx
}

// addSignedChannel registers a bound channel on sess for connID with a CMAC
// signer keyed by channelKey.
func addSignedChannel(sess *session.Session, connID uint64, channelKey []byte) {
	sess.AddChannel(&session.Channel{
		ConnID:      connID,
		Dialect:     types.Dialect0311,
		SigningAlgo: signing.SigningAlgAESCMAC,
		SigningKey:  channelKey,
		Signer:      signing.NewCMACSigner(channelKey),
	})
}

// TestSessionSetup_ReauthOnBoundChannel_BadSignature_AccessDenied verifies the
// MS-SMB2 §3.3.5.2.4 gate (Samba smb2_server.c:3189-3255, has_channel==true):
// a non-binding re-auth SESSION_SETUP that arrives on a bound channel and is
// signed with a key that does not match the channel key is rejected with
// STATUS_ACCESS_DENIED — the case smb2.session.bind_negative_smb3sign{CtoH,HtoC}
// exercises after a successful CMAC<->HMAC bind.
func TestSessionSetup_ReauthOnBoundChannel_BadSignature_AccessDenied(t *testing.T) {
	channelKey := make([]byte, 16)
	for i := range channelKey {
		channelKey[i] = 0x11
	}
	wrongKey := make([]byte, 16)
	for i := range wrongKey {
		wrongKey[i] = 0x22
	}

	h := NewHandler()
	sess := h.CreateSession("127.0.0.1:1", false, "alice", "WORKGROUP")
	sess.OriginConnID = 1
	addSignedChannel(sess, 2, channelKey)

	raw := buildSignedReauthRequest(t, sess.SessionID, wrongKey)
	ctx := newReauthContext(sess.SessionID, 2, raw)

	result, err := h.SessionSetup(ctx, raw[64:])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != types.StatusAccessDenied {
		t.Fatalf("status=0x%x, want StatusAccessDenied (0x%x)", result.Status, types.StatusAccessDenied)
	}
}

// TestSessionSetup_ReauthOnBoundChannel_GoodSignature_Proceeds verifies the
// inverse: a re-auth request correctly signed with the channel key passes the
// signature gate and proceeds into the NTLM handshake (interim
// STATUS_MORE_PROCESSING_REQUIRED), so a legitimate channel re-auth is not
// broken.
func TestSessionSetup_ReauthOnBoundChannel_GoodSignature_Proceeds(t *testing.T) {
	channelKey := make([]byte, 16)
	for i := range channelKey {
		channelKey[i] = 0x33
	}

	h := NewHandler()
	sess := h.CreateSession("127.0.0.1:1", false, "alice", "WORKGROUP")
	sess.OriginConnID = 1
	addSignedChannel(sess, 2, channelKey)

	raw := buildSignedReauthRequest(t, sess.SessionID, channelKey)
	ctx := newReauthContext(sess.SessionID, 2, raw)

	result, err := h.SessionSetup(ctx, raw[64:])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status == types.StatusAccessDenied {
		t.Fatalf("status=StatusAccessDenied, a correctly-signed channel re-auth must not be rejected by the signature gate")
	}
}

// TestSessionSetup_ReauthOnOriginConnection_NoChannel_NotGated verifies the
// gate is scoped to bound channels: a re-auth on the origin connection (which
// has no Channel entry) is not signature-gated here, preserving the existing
// reauth1-6 behaviour.
func TestSessionSetup_ReauthOnOriginConnection_NoChannel_NotGated(t *testing.T) {
	wrongKey := make([]byte, 16)
	for i := range wrongKey {
		wrongKey[i] = 0x44
	}

	h := NewHandler()
	sess := h.CreateSession("127.0.0.1:1", false, "alice", "WORKGROUP")
	sess.OriginConnID = 1 // origin connection; no bound Channel registered

	raw := buildSignedReauthRequest(t, sess.SessionID, wrongKey)
	ctx := newReauthContext(sess.SessionID, 1, raw)

	result, err := h.SessionSetup(ctx, raw[64:])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status == types.StatusAccessDenied {
		t.Fatalf("status=StatusAccessDenied, origin-connection re-auth must not be signature-gated")
	}
}

// buildBindingRequest builds the raw wire bytes of a SESSION_SETUP request with
// the SMB2_SESSION_FLAG_BINDING flag set, carrying a Kerberos AP-REQ so it would
// route to completeKerberosBind if it passed the bind gates.
func buildBindingRequest(t *testing.T, sessionID uint64) []byte {
	t.Helper()
	dummyAPReq := []byte{0x30, 0x05, 0x01, 0x02, 0x03, 0x04, 0x05}
	body := buildSessionSetupRequestBody(wrapKerberosInSPNEGO(dummyAPReq))
	body[sessionSetupFlagsOffset] = SMB2_SESSION_FLAG_BINDING
	hdr := &header.SMB2Header{
		Command:   types.SMB2SessionSetup,
		MessageID: 5,
		SessionID: sessionID,
	}
	return append(hdr.Encode(), body...)
}

// TestSessionSetup_RebindOnBoundConnection_AccessDenied verifies that a second
// SESSION_SETUP_BINDING on a connection that already owns a bound channel is
// rejected with STATUS_ACCESS_DENIED — the smbtorture re-bind at session.c:2839
// (the final rejection in test_session_bind_negative_smb3sign{CtoH,HtoC}{s,d}).
// Without the gate, AddChannel silently replaces the live channel and answers
// STATUS_SUCCESS.
func TestSessionSetup_RebindOnBoundConnection_AccessDenied(t *testing.T) {
	channelKey := make([]byte, 16)
	for i := range channelKey {
		channelKey[i] = 0x66
	}

	h := NewHandler()
	sess := h.CreateSession("127.0.0.1:1", false, "alice", "WORKGROUP")
	sess.OriginConnID = 1
	addSignedChannel(sess, 2, channelKey) // connection 2 already bound

	raw := buildBindingRequest(t, sess.SessionID)
	ctx := newReauthContext(sess.SessionID, 2, raw)

	result, err := h.SessionSetup(ctx, raw[64:])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != types.StatusAccessDenied {
		t.Fatalf("status=0x%x, want StatusAccessDenied (0x%x)", result.Status, types.StatusAccessDenied)
	}
}

// TestSessionSetup_FirstBindOnUnboundConnection_NotRejectedByRebindGate verifies
// the re-bind gate is scoped to connections that ALREADY have a channel: the
// normal first bind (no channel yet on the connection) must not be rejected by
// this gate. With no KerberosService configured the bind proceeds and fails
// later with LOGON_FAILURE — the point is only that it is NOT ACCESS_DENIED from
// the re-bind gate.
func TestSessionSetup_FirstBindOnUnboundConnection_NotRejectedByRebindGate(t *testing.T) {
	h := NewHandler() // no KerberosService
	sess := h.CreateSession("127.0.0.1:1", false, "alice", "WORKGROUP")
	sess.OriginConnID = 1 // no channel registered on connection 2

	raw := buildBindingRequest(t, sess.SessionID)
	ctx := newReauthContext(sess.SessionID, 2, raw)

	result, err := h.SessionSetup(ctx, raw[64:])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status == types.StatusAccessDenied {
		t.Fatalf("status=StatusAccessDenied, a first bind on an unbound connection must not be rejected by the re-bind gate")
	}
}

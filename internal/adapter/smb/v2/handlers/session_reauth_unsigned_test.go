package handlers

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/header"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// buildUnsignedReauthRequest builds the raw wire bytes of a non-binding
// SESSION_SETUP that is NOT signed (no SMB2_FLAGS_SIGNED, zero signature). This
// models the smbtorture fresh-init client (smb2_session_init) that targets an
// existing SessionID on a bound transport without any session keys, so it
// cannot sign the request. Per MS-SMB2 §3.3.5.2.4 a signing-required session
// must reject this with STATUS_ACCESS_DENIED on a bound channel.
func buildUnsignedReauthRequest(sessionID uint64) []byte {
	body := buildSessionSetupRequestBody(wrapKerberosInSPNEGO([]byte{0x30, 0x05, 0x01, 0x02, 0x03, 0x04, 0x05}))
	hdr := &header.SMB2Header{
		Command:   types.SMB2SessionSetup,
		MessageID: 4,
		SessionID: sessionID,
	}
	return append(hdr.Encode(), body...)
}

// TestSessionSetup_ReauthOnBoundChannel_Unsigned_AccessDenied reproduces
// smbtorture smb2.session.bind_negative_smb3sign{CtoH,HtoC}{s,d} line 2842:
// after a successful CMAC<->HMAC bind, the harness re-runs a non-binding
// SESSION_SETUP from a fresh client with NO session keys (so the request is
// unsigned) on the bound transport. A signing-required session MUST reject it
// with STATUS_ACCESS_DENIED. DittoFS previously returned STATUS_OK because the
// signature gate short-circuited on the unsigned request.
func TestSessionSetup_ReauthOnBoundChannel_Unsigned_AccessDenied(t *testing.T) {
	channelKey := make([]byte, 16)
	for i := range channelKey {
		channelKey[i] = 0x55
	}

	h := NewHandler()
	sess := h.CreateSession("127.0.0.1:1", false, "alice", "WORKGROUP")
	sess.OriginConnID = 1
	// Deliberately do NOT call EnableSigning: the bound channel's existence
	// alone must drive the rejection. Before the fix this case slipped through
	// because the gate keyed off sess.CryptoState.SigningRequired, which the
	// Kerberos session-setup path could leave unset.
	addSignedChannel(sess, 2, channelKey)

	raw := buildUnsignedReauthRequest(sess.SessionID)
	ctx := newReauthContext(sess.SessionID, 2, raw)

	result, err := h.SessionSetup(ctx, raw[64:])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != types.StatusAccessDenied {
		t.Fatalf("status=0x%08x, want StatusAccessDenied (0x%08x): an unsigned re-auth on a signing-required bound channel must be rejected",
			result.Status, types.StatusAccessDenied)
	}
}

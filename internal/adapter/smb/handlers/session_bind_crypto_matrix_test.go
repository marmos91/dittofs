package handlers

import (
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/internal/adapter/smb/signing"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// This file locks in the SMB2 session-bind crypto-negotiation matrix
// (MS-SMB2 §3.3.5.5.2) against the exact reject status each smbtorture
// bind_negative_* test asserts, and proves the matrix is applied identically
// regardless of the auth mechanism the bind request carries (NTLM vs
// Kerberos).
//
// The matrix lives in handleSessionBind, which runs BEFORE the request's
// security buffer is examined to choose between the NTLM challenge handshake
// and the single-shot Kerberos AP-REQ. A bind that fails the matrix is
// rejected before either completion path (completeSessionBind /
// completeKerberosBind) is reached, so the same from→to signing/cipher
// transition produces the same NTSTATUS on both transports. The 18 Kerberos
// bind_negative_* known-failure rows could only differ from the NTLM path if
// the matrix were bypassed for Kerberos — these tests assert it is not.
//
// Reject statuses are taken verbatim from
// source4/torture/smb2/session.c (the bind_reject_status argument each
// test_session_bind_negative_smb3{sign,enc,sne}XtoY passes to
// test_session_bind_negative_smbXtoX):
//
//	session→channel signing/cipher    smbtorture tests              expected
//	--------------------------------  ---------------------------  -----------------------
//	GMAC → CMAC/HMAC (signing)        smb3signGtoC/GtoH,           REQUEST_OUT_OF_SEQUENCE
//	                                  smb3sneGtoC/GtoH
//	CMAC/HMAC → GMAC (signing)        smb3signCtoG/HtoG,           NOT_SUPPORTED
//	                                  smb3sneCtoG/HtoG
//	GCM → CCM (cipher)                smb3encGtoC                  INVALID_PARAMETER
//	CMAC ↔ HMAC, no GMAC (signing)    smb3signCtoH/HtoC            OK (bind accepted)
//
// The CtoH/HtoC accept case is covered separately by the
// verifyReauthChannelSignature path (session_reauth_signature_test.go); here
// it is asserted only that the matrix does NOT reject it.

// bindMatrixCase describes one session→channel crypto transition and the
// NTSTATUS the bind must produce.
type bindMatrixCase struct {
	name string

	// Session (first channel) negotiated parameters.
	sessSigningAlgo uint16
	sessCipher      uint16

	// New channel (bind connection) negotiated parameters.
	connSigningAlgo uint16
	connCipher      uint16

	// wantStatus is the NTSTATUS handleSessionBind must return for the
	// rejected transition. types.StatusSuccess means the matrix accepts the
	// bind (it then proceeds to the auth-method completion).
	wantStatus types.Status
}

// bindCryptoMatrixCases enumerates every signing/cipher transition exercised
// by the smbtorture bind_negative_smb3{sign,enc,sne} family, keyed to the
// Samba reject status. All run on SMB 3.1.1 (the dialect those tests force).
func bindCryptoMatrixCases() []bindMatrixCase {
	const (
		gmac = signing.SigningAlgAESGMAC
		cmac = signing.SigningAlgAESCMAC
		hmac = signing.SigningAlgHMACSHA256
		gcm  = types.CipherAES128GCM
		ccm  = types.CipherAES128CCM
	)
	return []bindMatrixCase{
		// session=GMAC, channel downgrades signing → REQUEST_OUT_OF_SEQUENCE
		// (smb3signGtoCs/d, smb3signGtoHs/d, smb3sneGtoCs/d, smb3sneGtoHs/d).
		{"signGtoC", gmac, 0, cmac, 0, types.StatusRequestOutOfSequence},
		{"signGtoH", gmac, 0, hmac, 0, types.StatusRequestOutOfSequence},
		{"sneGtoC", gmac, gcm, cmac, ccm, types.StatusRequestOutOfSequence},
		{"sneGtoH", gmac, gcm, hmac, ccm, types.StatusRequestOutOfSequence},

		// session=non-GMAC, channel upgrades to GMAC → NOT_SUPPORTED
		// (smb3signCtoGs/d, smb3signHtoGs/d, smb3sneCtoGs/d, smb3sneHtoGs/d).
		{"signCtoG", cmac, 0, gmac, 0, types.StatusNotSupported},
		{"signHtoG", hmac, 0, gmac, 0, types.StatusNotSupported},
		{"sneCtoG", cmac, ccm, gmac, gcm, types.StatusNotSupported},
		{"sneHtoG", hmac, ccm, gmac, gcm, types.StatusNotSupported},

		// cipher mismatch with matched signing → INVALID_PARAMETER
		// (smb3encGtoCs/d). GMAC is excluded so the cipher check (step 7) is
		// the first to fire rather than the GMAC-symmetry check (steps 3-4).
		{"encGtoC", cmac, gcm, cmac, ccm, types.StatusInvalidParameter},

		// CMAC ↔ HMAC, neither side GMAC → bind ACCEPTED (smb3signCtoHs/d,
		// smb3signHtoCs/d). The reauth that follows is rejected by
		// verifyReauthChannelSignature, not by the bind matrix.
		{"signCtoH_accepted", cmac, 0, hmac, 0, types.StatusSuccess},
		{"signHtoC_accepted", hmac, 0, cmac, 0, types.StatusSuccess},
	}
}

// newBindMatrixContext builds a binding SESSION_SETUP context for the new
// channel with the given negotiated signing algorithm and cipher.
func newBindMatrixContext(sessionID uint64, signingAlgo, cipher uint16) *SMBHandlerContext {
	ctx := newTestContext(sessionID)
	ctx.ConnCryptoState = &mockCryptoState{
		dialect:            types.Dialect0311,
		signingAlgorithmId: signingAlgo,
		cipherId:           cipher,
	}
	return ctx
}

// newBindMatrixSession creates an authenticated session whose first channel
// negotiated the given signing algorithm and cipher.
func newBindMatrixSession(h *Handler, signingAlgo, cipher uint16) *session.Session {
	sess := h.CreateSession("127.0.0.1:1", false, "alice", "WORKGROUP")
	sess.Dialect = types.Dialect0311
	sess.SigningAlgo = signingAlgo
	sess.CipherId = cipher
	return sess
}

// TestSessionBind_CryptoMatrix_NTLMToken asserts every signing/cipher
// transition rejects with the smbtorture-mandated status when the bind
// request carries an NTLM token. An NTLM TYPE_1 is supplied so an accepted
// bind reaches the negotiate handshake (MORE_PROCESSING_REQUIRED) rather than
// being rejected for a missing token — distinguishing a matrix accept from a
// matrix reject.
func TestSessionBind_CryptoMatrix_NTLMToken(t *testing.T) {
	for _, tc := range bindCryptoMatrixCases() {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandler()
			sess := newBindMatrixSession(h, tc.sessSigningAlgo, tc.sessCipher)
			ctx := newBindMatrixContext(sess.SessionID, tc.connSigningAlgo, tc.connCipher)

			// Bind request body carrying a valid NTLM TYPE_1.
			body := buildSessionSetupRequestBody(wrapInSPNEGO(validNTLMNegotiateMessage()))
			body[sessionSetupFlagsOffset] = SMB2_SESSION_FLAG_BINDING
			binary.LittleEndian.PutUint64(body[16:24], 0)

			result, err := h.SessionSetup(ctx, body)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tc.wantStatus == types.StatusSuccess {
				// Matrix accepted: an NTLM bind proceeds to the challenge
				// handshake (interim MORE_PROCESSING_REQUIRED). It must NOT be
				// rejected with any of the matrix statuses.
				if result.Status != types.StatusMoreProcessingRequired {
					t.Fatalf("accepted bind: status=0x%08x, want MORE_PROCESSING_REQUIRED (0x%08x)",
						result.Status, types.StatusMoreProcessingRequired)
				}
				return
			}
			if result.Status != tc.wantStatus {
				t.Fatalf("status=0x%08x, want 0x%08x", result.Status, tc.wantStatus)
			}
		})
	}
}

// TestSessionBind_CryptoMatrix_KerberosToken asserts the SAME transitions
// reject with the SAME statuses when the bind request carries a Kerberos
// AP-REQ. This proves the crypto matrix is enforced before the auth-method
// branch and is therefore identical on the Kerberos path (#686). A matrix
// reject must never reach completeKerberosBind; a matrix accept reaches it and
// — with no KerberosService configured in the unit harness — returns
// LOGON_FAILURE, which is distinct from every matrix reject status and so
// proves the matrix let the request through.
func TestSessionBind_CryptoMatrix_KerberosToken(t *testing.T) {
	for _, tc := range bindCryptoMatrixCases() {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandler() // no KerberosService
			sess := newBindMatrixSession(h, tc.sessSigningAlgo, tc.sessCipher)
			ctx := newBindMatrixContext(sess.SessionID, tc.connSigningAlgo, tc.connCipher)

			// Bind request body carrying a SPNEGO-wrapped Kerberos AP-REQ.
			dummyAPReq := []byte{0x30, 0x05, 0x01, 0x02, 0x03, 0x04, 0x05}
			body := buildSessionSetupRequestBody(wrapKerberosInSPNEGO(dummyAPReq))
			body[sessionSetupFlagsOffset] = SMB2_SESSION_FLAG_BINDING
			binary.LittleEndian.PutUint64(body[16:24], 0)

			result, err := h.SessionSetup(ctx, body)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tc.wantStatus == types.StatusSuccess {
				// Matrix accepted: routed to completeKerberosBind, which fails
				// with LOGON_FAILURE (no KerberosService). The point is only
				// that it is NOT a matrix-reject status.
				if result.Status != types.StatusLogonFailure {
					t.Fatalf("accepted bind: status=0x%08x, want LOGON_FAILURE (Kerberos completion; 0x%08x)",
						result.Status, types.StatusLogonFailure)
				}
				return
			}
			if result.Status != tc.wantStatus {
				t.Fatalf("status=0x%08x, want 0x%08x (matrix must reject before Kerberos completion)",
					result.Status, tc.wantStatus)
			}
		})
	}
}

// TestSessionBind_CryptoMatrix_ParityAcrossAuthMechanisms asserts the matrix
// produces the same accept/reject decision boundary for NTLM and Kerberos
// bind requests across the full transition table. This is the explicit
// regression guard for #686: any future change that routes the Kerberos bind
// around handleSessionBind would diverge the two columns and fail here.
func TestSessionBind_CryptoMatrix_ParityAcrossAuthMechanisms(t *testing.T) {
	for _, tc := range bindCryptoMatrixCases() {
		t.Run(tc.name, func(t *testing.T) {
			reject := tc.wantStatus != types.StatusSuccess

			// NTLM column.
			hN := NewHandler()
			sN := newBindMatrixSession(hN, tc.sessSigningAlgo, tc.sessCipher)
			ctxN := newBindMatrixContext(sN.SessionID, tc.connSigningAlgo, tc.connCipher)
			bodyN := buildSessionSetupRequestBody(wrapInSPNEGO(validNTLMNegotiateMessage()))
			bodyN[sessionSetupFlagsOffset] = SMB2_SESSION_FLAG_BINDING
			binary.LittleEndian.PutUint64(bodyN[16:24], 0)
			resN, _ := hN.SessionSetup(ctxN, bodyN)

			// Kerberos column.
			hK := NewHandler()
			sK := newBindMatrixSession(hK, tc.sessSigningAlgo, tc.sessCipher)
			ctxK := newBindMatrixContext(sK.SessionID, tc.connSigningAlgo, tc.connCipher)
			dummyAPReq := []byte{0x30, 0x05, 0x01, 0x02, 0x03, 0x04, 0x05}
			bodyK := buildSessionSetupRequestBody(wrapKerberosInSPNEGO(dummyAPReq))
			bodyK[sessionSetupFlagsOffset] = SMB2_SESSION_FLAG_BINDING
			binary.LittleEndian.PutUint64(bodyK[16:24], 0)
			resK, _ := hK.SessionSetup(ctxK, bodyK)

			if reject {
				// Both transports must reject with the identical matrix status.
				if resN.Status != tc.wantStatus || resK.Status != tc.wantStatus {
					t.Fatalf("matrix reject diverged: ntlm=0x%08x kerberos=0x%08x want=0x%08x",
						resN.Status, resK.Status, tc.wantStatus)
				}
				return
			}
			// Both transports must accept (and diverge only in the auth-method
			// completion that follows the shared matrix).
			if resN.Status == types.StatusRequestOutOfSequence ||
				resN.Status == types.StatusNotSupported ||
				resN.Status == types.StatusInvalidParameter {
				t.Fatalf("ntlm bind wrongly rejected by matrix: status=0x%08x", resN.Status)
			}
			if resK.Status == types.StatusRequestOutOfSequence ||
				resK.Status == types.StatusNotSupported ||
				resK.Status == types.StatusInvalidParameter {
				t.Fatalf("kerberos bind wrongly rejected by matrix: status=0x%08x", resK.Status)
			}
		})
	}
}

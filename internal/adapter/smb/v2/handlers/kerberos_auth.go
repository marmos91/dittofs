package handlers

import (
	"fmt"
	"strings"

	"github.com/jcmturner/gofork/encoding/asn1"

	"github.com/marmos91/dittofs/internal/adapter/smb/auth"
	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	kerbauth "github.com/marmos91/dittofs/internal/auth/kerberos"
	"github.com/marmos91/dittofs/internal/logger"
	pkgkerberos "github.com/marmos91/dittofs/pkg/auth/kerberos"
	"github.com/marmos91/dittofs/pkg/identity"
)

// handleKerberosAuth handles Kerberos authentication via SPNEGO.
// Validates the AP-REQ, normalizes the session key to 16 bytes for SMB3 KDF,
// resolves the principal to a DittoFS username, and creates an authenticated session.
// [MS-SMB2] Section 3.3.5.5.3
func (h *Handler) handleKerberosAuth(ctx *SMBHandlerContext, mechToken []byte, parsedToken *auth.ParsedToken) (*HandlerResult, error) {
	// Check that KerberosService is configured (not just the provider).
	// KerberosService encapsulates AP-REQ verification, replay detection,
	// and subkey preference logic shared across NFS and SMB.
	if h.KerberosService == nil {
		logger.Debug("Kerberos auth attempted but no KerberosService configured")
		return NewErrorResult(types.StatusLogonFailure), nil
	}

	// Derive the SMB (CIFS) service principal from the configured NFS principal.
	// The shared Kerberos provider is configured with the NFS SPN (nfs/host@REALM),
	// but SMB clients present tickets for the CIFS SPN (cifs/host@REALM).
	var basePrincipal string
	if h.KerberosService.Provider() != nil {
		basePrincipal = h.KerberosService.Provider().ServicePrincipal()
	}
	smbPrincipal := deriveSMBPrincipal(basePrincipal, h.SMBServicePrincipal)

	// The SPNEGO MechToken is a GSS-API initial context token (RFC 2743
	// Section 3.1) wrapping the Kerberos AP-REQ. KerberosService.Authenticate
	// expects a raw AP-REQ, so we need to strip the GSS-API wrapper first.
	// NFS performs the equivalent step at rpc/gss/framework.go.
	apReqBytes, err := extractAPReqFromGSSToken(mechToken)
	if err != nil {
		logger.Info("Failed to extract AP-REQ from GSS token", "error", err)
		return NewErrorResult(types.StatusLogonFailure), nil
	}

	// Authenticate via shared service (handles AP-REQ parsing, verification,
	// replay detection, and subkey preference).
	authResult, err := h.KerberosService.Authenticate(apReqBytes, smbPrincipal)
	if err != nil {
		logger.Info("Kerberos authentication failed", "error", err)
		return NewErrorResult(types.StatusLogonFailure), nil
	}

	// Normalize session key to exactly 16 bytes for SMB3 KDF.
	// Per MS-SMB2 3.3.5.5.3, the session key is truncated or zero-padded to 16 bytes
	// regardless of the Kerberos encryption type:
	//   - AES-256 (32 bytes) -> truncate to 16
	//   - AES-128 (16 bytes) -> pass through
	//   - DES (8 bytes) -> zero-pad to 16
	sessionKey := normalizeSessionKey(authResult.SessionKey.KeyValue)

	// Resolve principal to DittoFS username via centralized identity resolver
	// (DB-backed mapping + convention fallback), or legacy IdentityConfig.
	username := h.resolveKerberosPrincipal(ctx, authResult.Principal, authResult.Realm)

	logger.Debug("Kerberos authentication succeeded",
		"principal", authResult.Principal,
		"realm", authResult.Realm,
		"username", username,
		"keyType", authResult.SessionKey.KeyType,
		"rawKeyLen", len(authResult.SessionKey.KeyValue),
		"normalizedKeyLen", len(sessionKey))

	// Look up user in control plane.
	// A valid Kerberos ticket from an unknown principal is a hard failure (not guest).
	userStore := h.Registry.GetUserStore()
	if userStore == nil {
		logger.Debug("Kerberos auth: no UserStore configured")
		return NewErrorResult(types.StatusLogonFailure), nil
	}

	user, err := userStore.GetUser(ctx.Context, username)
	if err != nil || user == nil || !user.Enabled {
		logger.Info("Kerberos auth: user lookup failed",
			"username", username, "principal", authResult.Principal,
			"found", user != nil, "error", err)
		return NewErrorResult(types.StatusLogonFailure), nil
	}

	// Create authenticated session and configure signing/encryption.
	// Set ExpiresAt before StoreSession to avoid a data race window where a
	// concurrent reader could observe a zero ExpiresAt on the published
	// session and skip the per-request expiry check in prepareDispatch.
	sessionID := h.GenerateSessionID()
	sess := session.NewSessionWithUser(sessionID, ctx.ClientAddr, user, authResult.Realm)
	sess.ExpiresAt = authResult.APReq.Ticket.DecryptedEncPart.EndTime
	h.SessionManager.StoreSession(sess)
	ctx.SessionID = sessionID
	ctx.IsGuest = false

	// Initialize per-session preauth hash for SMB 3.1.1 key derivation.
	// Per MS-SMB2 3.3.5.5: each session gets its own preauth hash chain
	// seeded from the connection hash and chained with the SESSION_SETUP
	// request bytes. Without this, configureSessionSigningWithKey falls
	// back to the connection-level hash and produces wrong signing/encryption
	// keys, causing the client to reject the signed SESSION_SETUP response.
	// The NTLM path does the equivalent in handleSessionSetup.
	if ctx.ConnCryptoState != nil {
		ctx.ConnCryptoState.InitSessionPreauthHash(sessionID, ctx.RawRequest)
	}

	// Configure session signing with normalized 16-byte key.
	// This goes through the same KDF pipeline as NTLM, producing
	// signing, encryption, and decryption keys for SMB 3.x.
	if cfgResult := h.configureSessionSigningWithKey(sess, sessionKey, ctx); cfgResult != nil {
		return cfgResult, nil
	}

	logger.Debug("Kerberos session created",
		"sessionID", sess.SessionID,
		"username", sess.Username,
		"domain", sess.Domain,
		"isGuest", sess.IsGuest,
		"signingEnabled", sess.ShouldSign(),
		"encryptData", sess.ShouldEncrypt())

	// Build mutual auth AP-REP and wrap it in a GSS-API InitialContextToken
	// (RFC 2743 Section 3.1) for the SPNEGO accept-complete response:
	//
	//   0x60 [len] 0x06 <oid-len> <oid-bytes> 0x02 0x00 <AP-REP>
	//
	// AP-REP encryption uses the ticket session key per RFC 4120 (not the
	// context subkey), which is what clients decrypt with.
	//
	// CRITICAL: the OID inside this wrapper must be the standard RFC 4121
	// Kerberos V5 OID (1.2.840.113554.1.2.2), even when the client advertised
	// the MS legacy OID (1.2.840.48018.1.2.2) in SPNEGO mechTypes. MIT's
	// krb5_gss and Heimdal only recognize the standard OID internally, so
	// echoing the MS OID here causes them to reject the token with
	// GSS_S_DEFECTIVE_TOKEN (issue #335, resolved by #337). Windows SSPI
	// accepts both. The outer SPNEGO supportedMech (responseOID) still
	// mirrors the client's choice for negTokenResp compatibility.
	ticketSessionKey := authResult.APReq.Ticket.DecryptedEncPart.Key
	rawAPRep, err := h.KerberosService.BuildMutualAuth(&authResult.APReq, ticketSessionKey)
	var apRepToken []byte
	if err != nil {
		logger.Debug("Failed to build AP-REP for mutual auth", "error", err)
		// Fall back to accept-complete without AP-REP (still functional).
	} else {
		apRepToken = kerbauth.WrapGSSToken(rawAPRep, kerbauth.KerberosV5OIDBytes, kerbauth.GSSTokenIDAPRep)
	}

	// Match the client's Kerberos OID in the SPNEGO response.
	// Windows clients using the MS OID expect to see it echoed back.
	responseOID := clientKerberosOID(parsedToken)

	// Handle SPNEGO mechListMIC for downgrade protection.
	// If the client sent a mechListMIC, verify it.
	// Then compute a server-side mechListMIC for the response.
	var serverMIC []byte
	if parsedToken != nil && len(parsedToken.MechListBytes) > 0 {
		// Verify client-sent MIC if present
		if len(parsedToken.MechListMIC) > 0 {
			if err := auth.VerifyMechListMIC(authResult.SessionKey, parsedToken.MechListBytes, parsedToken.MechListMIC); err != nil {
				logger.Debug("Client mechListMIC verification failed", "error", err)
				// Per RFC 4178, failed MIC verification should reject the negotiation
				return NewErrorResult(types.StatusLogonFailure), nil
			}
			logger.Debug("Client mechListMIC verified successfully")
		}

		// Compute server mechListMIC using the Kerberos session key
		// (NOT the normalized 16-byte key -- MIC uses the full session key)
		serverMIC, err = auth.ComputeMechListMIC(authResult.SessionKey, parsedToken.MechListBytes)
		if err != nil {
			logger.Debug("Failed to compute server mechListMIC", "error", err)
			// Non-fatal: response without MIC still works
			serverMIC = nil
		}
	}

	// Build SPNEGO response with AP-REP and optional MIC
	spnegoResp, err := auth.BuildAcceptCompleteWithMIC(responseOID, apRepToken, serverMIC)
	if err != nil {
		logger.Debug("Failed to build SPNEGO accept response", "error", err)
		// Fall back to basic accept-complete
		spnegoResp, _ = auth.BuildAcceptComplete(responseOID, nil)
	}

	return h.buildSessionSetupResponse(
		types.StatusSuccess,
		sessionEncryptFlag(sess),
		spnegoResp,
	), nil
}

// smbSessionKeyLen is the fixed session key size for SMB3 key derivation.
// Per MS-SMB2 Section 3.3.5.5.3, session keys are normalized to 16 bytes.
const smbSessionKeyLen = 16

// normalizeSessionKey normalizes a Kerberos session key to exactly 16 bytes
// for use in SMB3 key derivation (KDF). Per MS-SMB2 Section 3.3.5.5.3:
//   - Keys longer than 16 bytes are truncated (e.g., AES-256 -> 16 bytes)
//   - Keys shorter than 16 bytes are zero-padded (e.g., DES 8 bytes -> 16 bytes)
//   - Keys exactly 16 bytes pass through unchanged
func normalizeSessionKey(key []byte) []byte {
	normalized := make([]byte, smbSessionKeyLen)
	copy(normalized, key) // truncates if longer, zero-pads if shorter
	return normalized
}

// deriveSMBPrincipal derives the CIFS service principal from the base principal.
// If override is non-empty, it takes precedence.
// Otherwise, auto-derives from the base: "nfs/host@REALM" -> "cifs/host@REALM".
func deriveSMBPrincipal(basePrincipal, override string) string {
	if override != "" {
		return override
	}
	if strings.HasPrefix(basePrincipal, "nfs/") {
		return "cifs/" + strings.TrimPrefix(basePrincipal, "nfs/")
	}
	return basePrincipal
}

// clientKerberosOID determines which Kerberos OID the client used in SPNEGO
// and returns it for use in the server's accept-complete response.
// Windows clients use the MS OID (1.2.840.48018.1.2.2) and expect it echoed back.
// Standard clients use RFC 4121 OID (1.2.840.113554.1.2.2).
func clientKerberosOID(parsedToken *auth.ParsedToken) asn1.ObjectIdentifier {
	if parsedToken == nil {
		return auth.OIDKerberosV5
	}

	// Prefer MS OID if the client offered it (Windows SSPI compatibility)
	if parsedToken.HasMechanism(auth.OIDMSKerberosV5) {
		return auth.OIDMSKerberosV5
	}

	return auth.OIDKerberosV5
}

// resolveKerberosPrincipal resolves a Kerberos principal to a DittoFS username.
// Uses the centralized identity resolver when available, falling back to the
// legacy IdentityConfig-based resolution for backward compatibility.
func (h *Handler) resolveKerberosPrincipal(ctx *SMBHandlerContext, principal, realm string) string {
	if h.IdentityResolver != nil {
		cred := &identity.Credential{
			Provider:   "kerberos",
			ExternalID: principal + "@" + realm,
			Attributes: map[string]string{"realm": realm},
		}
		resolved, err := h.IdentityResolver.Resolve(ctx.Context, cred)
		if err == nil && resolved.Found {
			return resolved.Username
		}
	}
	return pkgkerberos.ResolvePrincipal(principal, realm, h.IdentityConfig)
}

func extractAPReqFromGSSToken(token []byte) ([]byte, error) {
	if len(token) == 0 {
		return nil, fmt.Errorf("empty token")
	}
	if token[0] != 0x60 {
		return token, nil
	}
	length, lengthBytes, err := parseGSSASN1Length(token[1:])
	if err != nil {
		return nil, fmt.Errorf("parse GSS token length: %w", err)
	}
	if uint64(length) > uint64(len(token)) {
		return nil, fmt.Errorf("GSS token length %d exceeds buffer size %d", length, len(token))
	}
	bodyStart := 1 + lengthBytes
	bodyEnd := bodyStart + int(length)
	if bodyEnd > len(token) {
		return nil, fmt.Errorf("GSS token truncated: expected %d bytes, have %d", bodyEnd, len(token))
	}
	body := token[bodyStart:bodyEnd]
	if len(body) < 4 || body[0] != 0x06 {
		return nil, fmt.Errorf("expected OID tag 0x06 at body start")
	}
	if body[1] >= 0x80 {
		return nil, fmt.Errorf("GSS body OID uses long-form length (0x%02x), not supported", body[1])
	}
	oidLen := int(body[1])
	apReqStart := 2 + oidLen + 2
	if apReqStart > len(body) {
		return nil, fmt.Errorf("GSS body truncated: need %d bytes for OID+tokID, have %d", apReqStart, len(body))
	}
	tokenID := uint16(body[2+oidLen])<<8 | uint16(body[2+oidLen+1])
	if tokenID != kerbauth.GSSTokenIDAPReq {
		return nil, fmt.Errorf("unexpected krb5 token ID: 0x%04x (want 0x%04x for AP-REQ)", tokenID, kerbauth.GSSTokenIDAPReq)
	}
	return body[apReqStart:], nil
}

func parseGSSASN1Length(buf []byte) (uint32, int, error) {
	if len(buf) == 0 {
		return 0, 0, fmt.Errorf("empty length field")
	}
	first := buf[0]
	if first < 0x80 {
		return uint32(first), 1, nil
	}
	n := int(first & 0x7f)
	if n == 0 || n > 4 {
		return 0, 0, fmt.Errorf("unsupported length encoding 0x%02x", first)
	}
	if len(buf) < 1+n {
		return 0, 0, fmt.Errorf("truncated length field")
	}
	var length uint32
	for i := 1; i <= n; i++ {
		length = (length << 8) | uint32(buf[i])
	}
	return length, 1 + n, nil
}

// sessionEncryptFlag returns the session encrypt data flag if the session
// should encrypt, or 0 if encryption is not enabled.
func sessionEncryptFlag(sess interface{ ShouldEncrypt() bool }) uint16 {
	if sess.ShouldEncrypt() {
		return types.SMB2SessionFlagEncryptData
	}
	return 0
}

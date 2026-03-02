package handlers

import (
	"strings"

	"github.com/jcmturner/gofork/encoding/asn1"

	"github.com/marmos91/dittofs/internal/adapter/smb/auth"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
	pkgkerberos "github.com/marmos91/dittofs/pkg/auth/kerberos"
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

	// Authenticate via shared service (handles AP-REQ parsing, verification,
	// replay detection, and subkey preference).
	authResult, err := h.KerberosService.Authenticate(mechToken, smbPrincipal)
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

	// Resolve principal to DittoFS username.
	// Uses configurable mapping (strip-realm default, explicit mapping table).
	username := pkgkerberos.ResolvePrincipal(authResult.Principal, authResult.Realm, h.IdentityConfig)

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
	sessionID := h.GenerateSessionID()
	sess := h.CreateSessionWithUser(sessionID, ctx.ClientAddr, user, authResult.Realm)
	ctx.SessionID = sessionID
	ctx.IsGuest = false

	// Configure session signing with normalized 16-byte key.
	// This goes through the same KDF pipeline as NTLM, producing
	// signing, encryption, and decryption keys for SMB 3.x.
	h.configureSessionSigningWithKey(sess, sessionKey, ctx)

	logger.Debug("Kerberos session created",
		"sessionID", sess.SessionID,
		"username", sess.Username,
		"domain", sess.Domain,
		"isGuest", sess.IsGuest,
		"signingEnabled", sess.ShouldSign(),
		"encryptData", sess.ShouldEncrypt())

	// Build mutual auth AP-REP via shared service for SPNEGO accept-complete response.
	apRepToken, err := h.KerberosService.BuildMutualAuth(&authResult.APReq, authResult.SessionKey)
	if err != nil {
		logger.Debug("Failed to build AP-REP for mutual auth", "error", err)
		// Fall back to accept-complete without AP-REP (still functional).
		apRepToken = nil
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

// sessionEncryptFlag returns the session encrypt data flag if the session
// should encrypt, or 0 if encryption is not enabled.
func sessionEncryptFlag(sess interface{ ShouldEncrypt() bool }) uint16 {
	if sess.ShouldEncrypt() {
		return types.SMB2SessionFlagEncryptData
	}
	return 0
}

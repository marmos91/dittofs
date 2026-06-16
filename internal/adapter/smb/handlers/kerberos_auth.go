package handlers

import (
	"fmt"
	"strings"
	"time"

	"github.com/jcmturner/gofork/encoding/asn1"

	"github.com/marmos91/dittofs/internal/adapter/smb/auth"
	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/internal/adapter/smb/signing"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	kerbauth "github.com/marmos91/dittofs/internal/auth/kerberos"
	"github.com/marmos91/dittofs/internal/logger"
	pkgkerberos "github.com/marmos91/dittofs/pkg/auth/kerberos"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/identity"
)

// handleKerberosAuth handles Kerberos authentication via SPNEGO.
// Validates the AP-REQ, normalizes the session key to 16 bytes for SMB3 KDF,
// resolves the principal to a DittoFS username, and creates an authenticated session.
// [MS-SMB2] Section 3.3.5.5.3
func (h *Handler) handleKerberosAuth(ctx *SMBHandlerContext, mechToken []byte, parsedToken *auth.ParsedToken) (result *HandlerResult, retErr error) {
	// Record the terminal Kerberos auth verdict exactly once. The AP-REQ both
	// negotiates and completes (single-shot), so every return from this function
	// is a final verdict: success is any non-error result; AP-REQ verification
	// failures, unknown principals, and missing config all return LOGON_FAILURE
	// and count as failed krb5 attempts.
	defer func() {
		h.recordAuth("krb5", result != nil && !result.Status.IsError())
	}()

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

	// Pass the FULL Kerberos session key to key derivation. SMB3 key
	// normalization is cipher-aware and happens inside DeriveAllKeys: signing,
	// application, and AES-128 cipher keys use the first 16 bytes, while
	// AES-256 cipher keys use the full key (e.g. a 32-byte AES-256 ticket
	// key). Pre-truncating here would starve the AES-256 derivation and break
	// smb2.session.encryption-aes-256-{ccm,gcm} under Kerberos (#686).
	sessionKey := authResult.SessionKey.KeyValue

	// Resolve principal to DittoFS username via centralized identity resolver
	// (DB-backed mapping + convention fallback), or legacy IdentityConfig.
	username := h.resolveKerberosPrincipal(ctx, authResult.Principal, authResult.Realm)

	logger.Debug("Kerberos authentication succeeded",
		"principal", authResult.Principal,
		"realm", authResult.Realm,
		"username", username,
		"keyType", authResult.SessionKey.KeyType,
		"sessionKeyLen", len(authResult.SessionKey.KeyValue))

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

	// Kerberos ticket end-time becomes the session expiry. prepareDispatch
	// rejects requests on a session past this time with
	// STATUS_NETWORK_SESSION_EXPIRED (MS-SMB2 §3.3.5.2.9), prompting the
	// client to re-authenticate via another SESSION_SETUP on the same
	// session id.
	ticketEndTime := authResult.APReq.Ticket.DecryptedEncPart.EndTime

	// Re-authentication vs. fresh session: a non-binding SESSION_SETUP that
	// targets an existing, non-LoggedOff session id (ctx.SessionID was already
	// validated as origin/bound by handleSessionSetup) re-authenticates that
	// session in place rather than minting a new id. Kerberos is single-shot
	// (the AP-REQ both negotiates and completes), so there is no PendingAuth to
	// carry an IsReauth flag — we detect reauth directly here. Reusing the id is
	// what smbtorture smb2.session.reauth1-5 and expire1/2 require: file opens
	// and tree connects established before the reauth must keep working
	// afterwards, and after the ticket expires the client re-runs SESSION_SETUP
	// on the same session id to recover from STATUS_NETWORK_SESSION_EXPIRED.
	if ctx.SessionID != 0 {
		if sess, ok := h.GetSession(ctx.SessionID); ok && !sess.LoggedOff.Load() {
			return h.reauthKerberosSession(ctx, sess, user, authResult, parsedToken, ticketEndTime)
		}
	}

	// Fresh session. CreateSessionWithUserAndExpiry sets ExpiresAt before
	// StoreSession to avoid a data race window where a concurrent reader
	// could observe a zero ExpiresAt on the published session and skip the
	// per-request expiry check in prepareDispatch (see #341 A1).
	sessionID := h.GenerateSessionID()
	sess := h.CreateSessionWithUserAndExpiry(
		sessionID,
		ctx.ClientAddr,
		user,
		authResult.Realm,
		ticketEndTime,
	)
	sess.OriginConnID = ctx.ConnID
	recordSessionBindIdentity(sess, ctx)
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
		ctx.ConnCryptoState.InitSessionPreauthHash(ctx.SessionID, ctx.RawRequest)
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

	return h.buildKerberosAcceptResponse(sess, authResult, parsedToken)
}

// reauthKerberosSession re-authenticates an existing, non-LoggedOff session in
// place from a fresh Kerberos AP-REQ (smbtorture smb2.session.reauth1-5 and the
// expire1/2 recovery setup). It updates the security context and refreshes the
// ticket lifetime, then returns a signed AP-REP accept-complete response.
//
// Keys are deliberately NOT re-derived. Per MS-SMB2 §3.3.5.5.3 a successful
// re-authentication updates Session.SecurityContext only; the established
// SigningKey / EncryptionKey / DecryptionKey and the frozen
// PreauthIntegrityHash are retained. Samba does the same — its
// smbd_smb2_reauth_generic_return copies the existing application_key_blob over
// the new gensec session key and omits every signing/encryption-key derivation
// call (source3/smbd/smb2_sesssetup.c). Re-deriving from the new ticket session
// key would make the SUCCESS response's signature diverge from what the client
// computes with its unchanged key, and the client rejects the response with
// STATUS_ACCESS_DENIED (the reauth1-4 regression this guards against). The
// per-session preauth hash entry was already deleted after the original setup,
// so the SESSION_SETUP before/after hooks are no-ops on this request and the
// frozen hash survives untouched. Open files and tree connects are preserved
// because the session record itself is reused (reauth5).
func (h *Handler) reauthKerberosSession(
	ctx *SMBHandlerContext,
	sess *session.Session,
	user *models.User,
	authResult *kerbauth.AuthResult,
	parsedToken *auth.ParsedToken,
	ticketEndTime time.Time,
) (*HandlerResult, error) {
	// Refresh identity and lifetime. Updating ExpiresAt to the fresh ticket
	// end-time clears the expired state so prepareDispatch lets subsequent
	// requests through again (expire1/2 recovery).
	sess.User = user
	sess.Username = user.Username
	sess.Domain = authResult.Realm
	sess.IsGuest = false
	sess.IsNull = false
	sess.ExpiresAt = ticketEndTime
	ctx.IsGuest = false

	logger.Info("Kerberos session re-authenticated (identity updated, keys retained)",
		"sessionID", sess.SessionID,
		"username", sess.Username,
		"domain", sess.Domain,
		"signingEnabled", sess.ShouldSign(),
		"encryptData", sess.ShouldEncrypt())

	return h.buildKerberosAcceptResponse(sess, authResult, parsedToken)
}

// buildKerberosAcceptResponse builds the SPNEGO accept-complete SESSION_SETUP
// response (mutual-auth AP-REP + optional mechListMIC) shared by the fresh and
// re-authentication Kerberos paths.
func (h *Handler) buildKerberosAcceptResponse(
	sess *session.Session,
	authResult *kerbauth.AuthResult,
	parsedToken *auth.ParsedToken,
) (*HandlerResult, error) {
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

// completeKerberosBind authenticates a Kerberos AP-REQ presented on a
// SMB2_SESSION_FLAG_BINDING SESSION_SETUP and, on success, registers a new
// signing channel on the existing session. Unlike NTLM bind (a TYPE_1/TYPE_2/
// TYPE_3 challenge handshake), Kerberos bind is single-shot: the AP-REQ arrives
// directly, so this both authenticates and completes the bind in one response —
// mirroring the primary Kerberos session-setup path (handleKerberosAuth) for
// authentication and the NTLM bind completion (completeSessionBind) for channel
// registration. The caller (handleSessionBind) has already validated the bind
// matrix (dialect / signing-algo / cipher) and seeded the per-channel preauth
// hash. smbtorture smb2.session.bind1 / bind2 / bind_invalid_auth.
func (h *Handler) completeKerberosBind(ctx *SMBHandlerContext, sess *session.Session, parsedToken *auth.ParsedToken) (result *HandlerResult, retErr error) {
	// Record the terminal Kerberos bind verdict exactly once. Like
	// handleKerberosAuth this path is single-shot (the AP-REQ both authenticates
	// and completes the bind), so every return is a final verdict — success is
	// any non-error result; LOGON_FAILURE / ACCESS_DENIED count as failed krb5
	// attempts. NTLM bind verdicts are already recorded in completeNTLMAuth via
	// its pending.IsBinding branch.
	defer func() {
		h.recordAuth("krb5", result != nil && !result.Status.IsError())
	}()

	if h.KerberosService == nil {
		logger.Debug("Kerberos bind attempted but no KerberosService configured")
		return NewErrorResult(types.StatusLogonFailure), nil
	}

	var basePrincipal string
	if h.KerberosService.Provider() != nil {
		basePrincipal = h.KerberosService.Provider().ServicePrincipal()
	}
	smbPrincipal := deriveSMBPrincipal(basePrincipal, h.SMBServicePrincipal)

	apReqBytes, err := extractAPReqFromGSSToken(parsedToken.MechToken)
	if err != nil {
		logger.Info("Kerberos bind: failed to extract AP-REQ from GSS token", "error", err)
		return NewErrorResult(types.StatusLogonFailure), nil
	}

	authResult, err := h.KerberosService.Authenticate(apReqBytes, smbPrincipal)
	if err != nil {
		logger.Info("Kerberos bind: authentication failed", "error", err)
		return NewErrorResult(types.StatusLogonFailure), nil
	}
	// Full Kerberos session key; DeriveChannelSigningKey normalizes to 16 bytes
	// for the signing key, and a future AES-256 cipher derivation would use the
	// full key. See handleKerberosAuth for the rationale (#686).
	sessionKey := authResult.SessionKey.KeyValue
	username := h.resolveKerberosPrincipal(ctx, authResult.Principal, authResult.Realm)

	userStore := h.Registry.GetUserStore()
	if userStore == nil {
		logger.Debug("Kerberos bind: no UserStore configured")
		return NewErrorResult(types.StatusLogonFailure), nil
	}
	user, err := userStore.GetUser(ctx.Context, username)
	if err != nil || user == nil || !user.Enabled {
		logger.Info("Kerberos bind: user lookup failed",
			"username", username, "principal", authResult.Principal, "found", user != nil, "error", err)
		return NewErrorResult(types.StatusLogonFailure), nil
	}

	// MS-SMB2 §3.3.5.5.2: the bound channel must authenticate the SAME user as
	// the existing session. A valid ticket for a different principal must be
	// rejected with STATUS_ACCESS_DENIED (smb2.session.bind_invalid_auth).
	if sess.User == nil || user.Username != sess.User.Username {
		sessUser := "<nil>"
		if sess.User != nil {
			sessUser = sess.User.Username
		}
		logger.Info("Kerberos bind: identity mismatch",
			"sessionID", ctx.SessionID, "sessionUser", sessUser, "bindUser", user.Username)
		return NewErrorResult(types.StatusAccessDenied), nil
	}

	// Verify the client's SPNEGO mechListMIC (downgrade protection, RFC 4178)
	// BEFORE registering any channel state. A bad MIC rejects the bind, so it
	// must be checked before AddChannel — otherwise a live signing channel
	// would be left attached to the session on a rejected bind.
	if len(parsedToken.MechListBytes) > 0 && len(parsedToken.MechListMIC) > 0 {
		if vErr := auth.VerifyMechListMIC(authResult.SessionKey, parsedToken.MechListBytes, parsedToken.MechListMIC); vErr != nil {
			logger.Debug("Kerberos bind: client mechListMIC verification failed", "error", vErr)
			return NewErrorResult(types.StatusLogonFailure), nil
		}
	}

	// Per MS-SMB2 §3.3.5.5.2 the channel signing key is derived from THIS
	// bind's session key (not the original session's). Mirrors the NTLM
	// completeSessionBind derivation.
	connDialect := types.Dialect0300
	var signingAlgId uint16
	var signingAlgExplicit bool
	if ctx.ConnCryptoState != nil {
		connDialect = ctx.ConnCryptoState.GetDialect()
		signingAlgId, signingAlgExplicit = ctx.ConnCryptoState.GetSigningAlgorithmId()
	}
	var preauthHash [64]byte
	if connDialect == types.Dialect0311 && ctx.ConnCryptoState != nil {
		preauthHash = ctx.ConnCryptoState.GetSessionPreauthHash(ctx.SessionID)
	}
	channelSigningKey, err := session.DeriveChannelSigningKey(sessionKey, connDialect, preauthHash)
	if err != nil {
		logger.Warn("Kerberos bind: channel key derivation failed", "sessionID", ctx.SessionID, "error", err)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}
	channelSigner := signing.NewSigner(connDialect, signingAlgId, signingAlgExplicit, channelSigningKey)

	channel := &session.Channel{
		ConnID:      ctx.ConnID,
		RemoteAddr:  ctx.ClientAddr,
		Dialect:     connDialect,
		SigningAlgo: signingAlgId,
		SigningKey:  channelSigningKey,
		Signer:      channelSigner,
		PreauthHash: preauthHash,
		Transport:   ctx.ConnTransport,
	}
	if !sess.AddChannel(channel) {
		logger.Info("Kerberos bind rejected: channel cap reached",
			"sessionID", ctx.SessionID, "cap", session.MaxChannelsPerSession)
		return NewErrorResult(types.StatusInsufficientResources), nil
	}
	if ctx.ConnCryptoState != nil {
		ctx.ConnCryptoState.DeleteSessionPreauthHash(ctx.SessionID)
	}

	logger.Info("Kerberos bind: channel registered",
		"sessionID", ctx.SessionID, "connID", ctx.ConnID, "user", user.Username,
		"totalChannels", len(sess.ListChannels()))

	// Build the AP-REP accept-complete response (mutual auth + mechListMIC),
	// mirroring the primary Kerberos path.
	ticketSessionKey := authResult.APReq.Ticket.DecryptedEncPart.Key
	var apRepToken []byte
	if rawAPRep, mErr := h.KerberosService.BuildMutualAuth(&authResult.APReq, ticketSessionKey); mErr == nil {
		apRepToken = kerbauth.WrapGSSToken(rawAPRep, kerbauth.KerberosV5OIDBytes, kerbauth.GSSTokenIDAPRep)
	}
	responseOID := clientKerberosOID(parsedToken)

	// Compute the server-side mechListMIC for the response (the client MIC was
	// already verified above, before any channel state was registered).
	var serverMIC []byte
	if len(parsedToken.MechListBytes) > 0 {
		serverMIC, _ = auth.ComputeMechListMIC(authResult.SessionKey, parsedToken.MechListBytes)
	}

	spnegoResp, err := auth.BuildAcceptCompleteWithMIC(responseOID, apRepToken, serverMIC)
	if err != nil {
		// Keep the AP-REP token on fallback (only the MIC is dropped) so
		// Kerberos mutual authentication still completes.
		spnegoResp, _ = auth.BuildAcceptComplete(responseOID, apRepToken)
	}

	// Binding response matches the existing session's encrypt-data state per
	// §3.3.5.5; the BINDING flag is request-only and not echoed.
	result = h.buildSessionSetupResponse(types.StatusSuccess, sessionEncryptFlag(sess), spnegoResp)
	result.IsBinding = true
	return result, nil
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

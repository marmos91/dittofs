package kerberos

import (
	"fmt"

	// gokrb5 uses gofork's asn1 package, not the Go stdlib, because stdlib's
	// encoding/asn1 has known bugs with Kerberos types (GeneralizedTime
	// fractional-second handling, optional field tagging). Marshalling gokrb5
	// structs with stdlib asn1 produces DER bytes that MIT Kerberos clients
	// reject as malformed. See issue #335.
	"github.com/jcmturner/gofork/encoding/asn1"
	"github.com/jcmturner/gokrb5/v8/asn1tools"
	"github.com/jcmturner/gokrb5/v8/crypto"
	"github.com/jcmturner/gokrb5/v8/messages"
	"github.com/jcmturner/gokrb5/v8/service"
	"github.com/jcmturner/gokrb5/v8/types"

	"github.com/marmos91/dittofs/internal/logger"
	pkgkerberos "github.com/marmos91/dittofs/pkg/auth/kerberos"
)

// Kerberos protocol constants for AP-REP construction (RFC 4120).
const (
	// krbPVNO is the Kerberos protocol version number.
	krbPVNO = 5

	// krbAPRep is the message type for AP-REP (APPLICATION 15).
	krbAPRep = 15

	// appTagEncAPRepPart is the ASN.1 APPLICATION tag for EncAPRepPart.
	appTagEncAPRepPart = 27

	// keyUsageAPRepEncPart is the key usage number for encrypting the
	// EncAPRepPart, per RFC 4120 Section 7.5.1.
	keyUsageAPRepEncPart = 12
)

// AuthResult contains the result of a successful Kerberos AP-REQ verification.
type AuthResult struct {
	// Principal is the client's Kerberos principal name (e.g., "alice").
	Principal string

	// Realm is the client's Kerberos realm (e.g., "EXAMPLE.COM").
	Realm string

	// SessionKey is the session key to use for subsequent operations.
	// This is the authenticator subkey if present, otherwise the ticket session key,
	// per MS-SMB2 3.3.5.5.3 and RFC 4120.
	SessionKey types.EncryptionKey

	// APReq is the parsed AP-REQ message, preserved for AP-REP construction.
	APReq messages.APReq

	// APRepToken contains the raw AP-REP bytes (not GSS-wrapped).
	// Protocol-specific framing is handled by callers.
	APRepToken []byte
}

// KerberosService provides shared Kerberos authentication used by both
// NFS RPCSEC_GSS and SMB SESSION_SETUP.
//
// It handles:
//   - AP-REQ verification via gokrb5 service.VerifyAPREQ
//   - Authenticator subkey preference (per MS-SMB2 3.3.5.5.3 and RFC 4120)
//   - AP-REP construction for mutual authentication
//   - Cross-protocol replay detection via ReplayCache
//
// Thread Safety: All methods are safe for concurrent use.
type KerberosService struct {
	provider    *pkgkerberos.Provider
	replayCache *ReplayCache
}

// NewKerberosService creates a new KerberosService.
// provider may be nil for testing (Authenticate will fail, but BuildMutualAuth works).
func NewKerberosService(provider *pkgkerberos.Provider) *KerberosService {
	return &KerberosService{
		provider:    provider,
		replayCache: NewReplayCache(DefaultReplayCacheTTL),
	}
}

// Provider returns the underlying Kerberos provider.
func (s *KerberosService) Provider() *pkgkerberos.Provider {
	return s.provider
}

// Authenticate verifies an AP-REQ token and returns the authentication result.
//
// The apReqBytes should be the raw AP-REQ (not GSS-wrapped; NFS callers must
// strip the GSS-API wrapper before calling this method).
//
// servicePrincipal is the SPN to validate against (e.g., "nfs/server.example.com"
// for NFS, "cifs/server.example.com" for SMB).
//
// On success, the returned AuthResult contains:
//   - Principal and Realm from the decrypted ticket
//   - SessionKey with subkey preference (authenticator subkey if present)
//   - APReq for use in BuildMutualAuth
//   - APRepToken is empty (call BuildMutualAuth separately if mutual auth is needed)
func (s *KerberosService) Authenticate(apReqBytes []byte, servicePrincipal string) (*AuthResult, error) {
	if s.provider == nil {
		return nil, fmt.Errorf("kerberos provider not configured")
	}

	// Parse the AP-REQ
	var apReq messages.APReq
	if err := apReq.Unmarshal(apReqBytes); err != nil {
		return nil, fmt.Errorf("unmarshal AP-REQ: %w", err)
	}

	// Build gokrb5 service settings from provider
	settings := service.NewSettings(
		s.provider.Keytab(),
		service.MaxClockSkew(s.provider.MaxClockSkew()),
		service.DecodePAC(false),
		service.KeytabPrincipal(servicePrincipal),
	)

	// Verify the AP-REQ
	ok, _, err := service.VerifyAPREQ(&apReq, settings)
	if err != nil {
		return nil, fmt.Errorf("verify AP-REQ: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("AP-REQ verification failed")
	}

	// Extract session key from the decrypted ticket
	sessionKey := apReq.Ticket.DecryptedEncPart.Key

	// Decrypt the authenticator to access ctime/cusec and subkey
	if err := apReq.DecryptAuthenticator(sessionKey); err != nil {
		return nil, fmt.Errorf("decrypt authenticator: %w", err)
	}

	// Check replay cache
	principal := apReq.Authenticator.CName.PrincipalNameString()
	if s.replayCache.Check(principal, apReq.Authenticator.CTime, apReq.Authenticator.Cusec, servicePrincipal) {
		return nil, fmt.Errorf("replay detected for %s", principal)
	}

	// Per RFC 4120 and MS-SMB2 3.3.5.5.3: prefer authenticator subkey over
	// ticket session key for subsequent cryptographic operations.
	contextKey := sessionKey
	if HasSubkey(&apReq) {
		contextKey = apReq.Authenticator.SubKey
		logger.Debug("Using authenticator subkey for session",
			"subkey_etype", contextKey.KeyType,
			"subkey_len", len(contextKey.KeyValue),
		)
	}

	// Client principal is in the decrypted ticket's CName
	clientPrincipal := apReq.Ticket.DecryptedEncPart.CName.PrincipalNameString()
	clientRealm := apReq.Ticket.DecryptedEncPart.CRealm

	logger.Debug("Kerberos authentication successful",
		"principal", clientPrincipal,
		"realm", clientRealm,
		"has_subkey", HasSubkey(&apReq),
	)

	return &AuthResult{
		Principal:  clientPrincipal,
		Realm:      clientRealm,
		SessionKey: contextKey,
		APReq:      apReq,
	}, nil
}

// BuildMutualAuth constructs a raw AP-REP token for mutual authentication.
//
// The returned bytes are raw AP-REP (APPLICATION 15), NOT GSS-wrapped.
// Both protocol adapters wrap in a GSS-API InitialContextToken
// (0x60 + OID + 0x0200 header per RFC 2743 §3.1) before delivery:
//   - NFS: rpc/gss/framework.go adds the wrapper for RPCSEC_GSS replies.
//   - SMB: v2/handlers/kerberos_auth.go adds the wrapper via WrapGSSToken
//     before placing the token inside the SPNEGO accept-complete response.
//     MIT krb5_gss and Heimdal reject raw AP-REPs with GSS_S_DEFECTIVE_TOKEN
//     (see #337). Do not skip the wrap.
//
// Per RFC 4120 Section 5.5.2, the EncAPRepPart contains:
//   - ctime/cusec copied from the authenticator (proves we decrypted the ticket)
//   - subkey (optional): echoed if the client sent a subkey in the authenticator
//
// The EncAPRepPart is encrypted with the session key using key usage 12
// (AP-REP encrypted part, per RFC 4120 Section 7.5.1).
func (s *KerberosService) BuildMutualAuth(apReq *messages.APReq, sessionKey types.EncryptionKey) ([]byte, error) {
	// Build EncAPRepPart with values from the authenticator
	encAPRepPart := messages.EncAPRepPart{
		CTime: apReq.Authenticator.CTime,
		Cusec: apReq.Authenticator.Cusec,
	}

	// If client sent a subkey, include it in the AP-REP.
	// This tells the client we've accepted the subkey and will use it
	// for subsequent operations (MIC computation, wrap/unwrap).
	if HasSubkey(apReq) {
		encAPRepPart.Subkey = apReq.Authenticator.SubKey
		logger.Debug("Including subkey in EncAPRepPart",
			"etype", apReq.Authenticator.SubKey.KeyType,
			"key_len", len(apReq.Authenticator.SubKey.KeyValue),
		)
	}

	// Marshal the EncAPRepPart inner SEQUENCE (without APPLICATION tag)
	encAPRepPartInner, err := asn1.Marshal(encAPRepPart)
	if err != nil {
		return nil, fmt.Errorf("marshal EncAPRepPart inner: %w", err)
	}

	// Add APPLICATION 27 (EncAPRepPart) tag using gokrb5's asn1tools
	encAPRepPartBytes := asn1tools.AddASNAppTag(encAPRepPartInner, appTagEncAPRepPart)

	// Encrypt with session key using key usage 12 (AP-REP encrypted part)
	encryptedData, err := crypto.GetEncryptedData(encAPRepPartBytes, sessionKey, keyUsageAPRepEncPart, 0)
	if err != nil {
		return nil, fmt.Errorf("encrypt EncAPRepPart: %w", err)
	}

	// Build the AP-REP message
	apRep := messages.APRep{
		PVNO:    krbPVNO,
		MsgType: krbAPRep,
		EncPart: encryptedData,
	}

	// Marshal AP-REP inner SEQUENCE (without APPLICATION tag)
	apRepInner, err := asn1.Marshal(apRep)
	if err != nil {
		return nil, fmt.Errorf("marshal AP-REP inner: %w", err)
	}

	// Add APPLICATION 15 (AP-REP) tag. This is the raw AP-REP, NOT GSS-wrapped.
	// Protocol adapters handle their own framing:
	// - NFS adds GSS-API wrapper (0x60 + OID + 0x0200)
	// - SMB passes raw to SPNEGO
	apRepBytes := asn1tools.AddASNAppTag(apRepInner, krbAPRep)

	return apRepBytes, nil
}

// HasSubkey returns true if the AP-REQ authenticator contains a valid subkey.
func HasSubkey(apReq *messages.APReq) bool {
	return apReq.Authenticator.SubKey.KeyType != 0 &&
		len(apReq.Authenticator.SubKey.KeyValue) > 0
}

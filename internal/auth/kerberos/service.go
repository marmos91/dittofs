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
	"github.com/jcmturner/gokrb5/v8/credentials"
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

	// UserSID is the client's Windows Security Identifier as carried in the
	// Kerberos PAC (MS-PAC §2.5 KERB_VALIDATION_INFO): the logon domain SID
	// joined with the user's RID. Empty when the ticket carries no PAC (e.g.
	// an MIT KDC, or a Samba/AD ticket whose PAC could not be decoded).
	UserSID string

	// GroupSIDs are the Windows group Security Identifiers delivered in the
	// Kerberos PAC. Active Directory resolves nested group membership at the
	// DC and stamps the full transitive set into the ticket, so no LDAP
	// group-walk is required at authentication time. Empty when the ticket
	// carries no PAC. These flow into AuthContext.Identity.GroupSIDs for
	// SID-based ACL evaluation.
	//
	// Resolution of these (possibly foreign) SIDs to local UID/GID is the
	// concern of the durable idmap layer (AD-3 / #1235); AD-1 only makes the
	// SIDs flow into the live identity.
	GroupSIDs []string
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
//
// Mutual authentication is not handled here; callers invoke BuildMutualAuth
// separately when an AP-REP token is required.
func (s *KerberosService) Authenticate(apReqBytes []byte, servicePrincipal string) (*AuthResult, error) {
	if s.provider == nil {
		return nil, fmt.Errorf("kerberos provider not configured")
	}

	// Parse the AP-REQ
	var apReq messages.APReq
	if err := apReq.Unmarshal(apReqBytes); err != nil {
		return nil, fmt.Errorf("unmarshal AP-REQ: %w", err)
	}

	// Build gokrb5 service settings from provider.
	//
	// DecodePAC is enabled so AD-issued tickets surface their PAC
	// (MS-PAC) authorization data — specifically the transitive group SIDs the
	// DC stamped into the ticket. gokrb5 verifies the PAC server signature with
	// the same keytab key used for the ticket, so a tampered PAC fails
	// VerifyAPREQ. Tickets without a PAC (e.g. from a plain MIT KDC) verify
	// exactly as before — creds simply carry no AD attributes.
	settings := service.NewSettings(
		s.provider.Keytab(),
		service.MaxClockSkew(s.provider.MaxClockSkew()),
		service.DecodePAC(true),
		service.KeytabPrincipal(servicePrincipal),
	)

	// Verify the AP-REQ. On success creds carries the decoded PAC attributes
	// (when DecodePAC is on and the ticket contained a PAC).
	ok, creds, err := service.VerifyAPREQ(&apReq, settings)
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

	// Extract PAC authorization data (AD group SIDs + user SID) when the ticket
	// carried a verified PAC. creds is nil for callers/tickets without a PAC.
	userSID, groupSIDs := extractPACIdentity(creds)

	logger.Debug("Kerberos authentication successful",
		"principal", clientPrincipal,
		"realm", clientRealm,
		"has_subkey", HasSubkey(&apReq),
		"pac_user_sid", userSID,
		"pac_group_sid_count", len(groupSIDs),
	)

	return &AuthResult{
		Principal:  clientPrincipal,
		Realm:      clientRealm,
		SessionKey: contextKey,
		APReq:      apReq,
		UserSID:    userSID,
		GroupSIDs:  groupSIDs,
	}, nil
}

// extractPACIdentity pulls the Windows user SID and group SIDs out of the
// decoded Kerberos PAC. gokrb5 exposes the decoded KERB_VALIDATION_INFO via
// credentials.ADCredentials (populated by VerifyAPREQ when DecodePAC is on and
// the ticket contained a PAC). It returns empty values when creds is nil or
// the ticket carried no AD attributes — i.e. a non-AD KDC — so the rest of the
// pipeline behaves exactly as it did before PAC decoding was enabled.
//
// The user SID is reconstructed from the logon domain SID + the user's RID per
// MS-PAC §2.5; the group SIDs come straight from
// KERB_VALIDATION_INFO.GetGroupMembershipSIDs (domain groups, extra SIDs, and
// resource groups), already formatted as full SID strings.
func extractPACIdentity(creds *credentials.Credentials) (userSID string, groupSIDs []string) {
	if creds == nil {
		return "", nil
	}
	ad := creds.GetADCredentials()
	if ad.LogonDomainID != "" && ad.UserID != 0 {
		userSID = fmt.Sprintf("%s-%d", ad.LogonDomainID, ad.UserID)
	}
	return userSID, ad.GroupMembershipSIDs
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

package gss

import (
	"encoding/asn1"
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	"github.com/jcmturner/gokrb5/v8/asn1tools"
	"github.com/jcmturner/gokrb5/v8/crypto"
	"github.com/jcmturner/gokrb5/v8/messages"
	"github.com/jcmturner/gokrb5/v8/service"
	"github.com/jcmturner/gokrb5/v8/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/auth/kerberos"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// Verifier Interface (mockable for testing)
// ============================================================================

// VerifiedContext contains the result of a successful GSS token verification.
//
// This is returned by the Verifier interface after validating an AP-REQ.
// It provides the information needed to create a GSSContext.
type VerifiedContext struct {
	// Principal is the client's Kerberos principal name (e.g., "alice").
	Principal string

	// Realm is the client's Kerberos realm (e.g., "EXAMPLE.COM").
	Realm string

	// SessionKey is the session key from the decrypted service ticket.
	// This is the subkey if the authenticator contained one, otherwise
	// the ticket session key.
	SessionKey types.EncryptionKey

	// APRepToken is the AP-REP token for mutual authentication.
	// May be empty if mutual auth is not supported or not requested.
	APRepToken []byte

	// HasAcceptorSubkey indicates whether the AP-REP contains a subkey.
	// When true, MIC tokens must have FLAG_ACCEPTOR_SUBKEY set per RFC 4121.
	HasAcceptorSubkey bool
}

// Verifier abstracts the GSS token verification step.
//
// This interface allows the GSSProcessor to be tested without a real KDC
// by providing a mock implementation. The production implementation uses
// gokrb5's service.VerifyAPREQ.
type Verifier interface {
	// VerifyToken verifies a GSS-API token (containing an AP-REQ).
	//
	// The token is the raw bytes from the RPC call body during RPCSEC_GSS_INIT.
	// For krb5 mechanism, this is an AP-REQ possibly wrapped in a GSS-API
	// initial context token (application tag with KRB5 OID prefix).
	//
	// Parameters:
	//   - gssToken: The raw GSS token from the INIT request
	//
	// Returns:
	//   - *VerifiedContext: Verification result with principal and session key
	//   - error: If verification fails (bad ticket, expired, etc.)
	VerifyToken(gssToken []byte) (*VerifiedContext, error)
}

// ============================================================================
// Production Verifier (uses gokrb5)
// ============================================================================

// Krb5Verifier implements Verifier using gokrb5 AP-REQ verification.
//
// It uses the KerberosProvider's keytab to decrypt and validate the service
// ticket, extract the session key, and verify the authenticator.
type Krb5Verifier struct {
	provider *kerberos.Provider
}

// NewKrb5Verifier creates a new production verifier.
func NewKrb5Verifier(provider *kerberos.Provider) *Krb5Verifier {
	return &Krb5Verifier{provider: provider}
}

// VerifyToken verifies a GSS-API token using gokrb5.
//
// The token may be wrapped in a GSS-API initial context token
// (0x60 application tag with KRB5 OID) or be a raw AP-REQ.
// We try to strip the GSS wrapper first, then fall back to raw AP-REQ.
func (v *Krb5Verifier) VerifyToken(gssToken []byte) (*VerifiedContext, error) {
	// Try to extract the AP-REQ from the GSS-API token.
	// GSS initial context tokens have format:
	//   0x60 [length] OID AP-REQ
	// We need to strip the wrapper to get the raw AP-REQ for gokrb5.
	apReqBytes, err := extractAPReq(gssToken)
	if err != nil {
		return nil, fmt.Errorf("extract AP-REQ from GSS token: %w", err)
	}

	// Parse the AP-REQ
	var apReq messages.APReq
	if err := apReq.Unmarshal(apReqBytes); err != nil {
		return nil, fmt.Errorf("unmarshal AP-REQ: %w", err)
	}

	// Build gokrb5 service settings
	settings := service.NewSettings(
		v.provider.Keytab(),
		service.MaxClockSkew(v.provider.MaxClockSkew()),
		service.DecodePAC(false),
		service.KeytabPrincipal(v.provider.ServicePrincipal()),
	)

	// Verify the AP-REQ
	ok, creds, err := service.VerifyAPREQ(&apReq, settings)
	if err != nil {
		return nil, fmt.Errorf("verify AP-REQ: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("AP-REQ verification failed")
	}

	// Debug: log what VerifyAPREQ returned
	// AP-Options: bit 1 = use-session-key, bit 2 = mutual-required (RFC 4120)
	mutualRequired := false
	if len(apReq.APOptions.Bytes) > 0 {
		// Bit 2 is in the first byte (MSB bit numbering)
		mutualRequired = (apReq.APOptions.Bytes[0] & 0x20) != 0 // 0x20 = bit 2 from MSB
	}
	logger.Debug("AP-REQ verification result",
		"cname", creds.CName().PrincipalNameString(),
		"realm", creds.Domain(),
		"ticket_cname", apReq.Ticket.DecryptedEncPart.CName.PrincipalNameString(),
		"ticket_crealm", apReq.Ticket.DecryptedEncPart.CRealm,
		"ticket_sname", apReq.Ticket.SName.PrincipalNameString(),
		"ticket_srealm", apReq.Ticket.Realm,
		"ap_options_hex", fmt.Sprintf("%x", apReq.APOptions.Bytes),
		"mutual_required", mutualRequired,
	)

	// Extract session key from the decrypted ticket
	sessionKey := apReq.Ticket.DecryptedEncPart.Key

	// Decrypt the authenticator to get ctime/cusec for AP-REP
	if err := apReq.DecryptAuthenticator(sessionKey); err != nil {
		return nil, fmt.Errorf("decrypt authenticator: %w", err)
	}

	logger.Debug("Authenticator decrypted",
		"ctime", apReq.Authenticator.CTime.Format("20060102150405Z"),
		"cusec", apReq.Authenticator.Cusec,
		"cname", apReq.Authenticator.CName.PrincipalNameString(),
		"has_subkey", apReq.Authenticator.SubKey.KeyType != 0,
		"subkey_etype", apReq.Authenticator.SubKey.KeyType,
	)

	// Per RFC 4120, if the client provides a subkey in the authenticator,
	// subsequent protection operations (including MIC for verifier) should
	// use the subkey instead of the session key from the ticket.
	// The subkey becomes the "accepted key" for the GSS context.
	contextKey := sessionKey
	if hasSubkey(apReq) {
		contextKey = apReq.Authenticator.SubKey
		logger.Debug("Using authenticator subkey for context",
			"subkey_etype", contextKey.KeyType,
			"subkey_len", len(contextKey.KeyValue),
			"subkey_first8", fmt.Sprintf("%x", contextKey.KeyValue[:min(8, len(contextKey.KeyValue))]),
			"session_key_first8", fmt.Sprintf("%x", sessionKey.KeyValue[:min(8, len(sessionKey.KeyValue))]),
		)
	}

	// The client principal is in the decrypted ticket's CName, not the authenticator.
	// creds.CName() might return the service principal in some implementations.
	clientPrincipal := apReq.Ticket.DecryptedEncPart.CName.PrincipalNameString()
	clientRealm := apReq.Ticket.DecryptedEncPart.CRealm

	// Build AP-REP for mutual authentication.
	// Per RFC 2203 Section 5.2.3.1: "The gss_token field contains the token
	// returned by the gss_accept_sec_context routine. This token may be empty."
	//
	// CRITICAL: Only send AP-REP when mutual authentication is REQUIRED.
	// Per MIT krb5 source (init_sec_context.c):
	// - If GSS_C_MUTUAL_FLAG is NOT set, gss_init_sec_context() returns GSS_S_COMPLETE
	//   on the first call and the context is immediately established.
	// - The client expects NO AP-REP in this case.
	// - If we send an AP-REP when not expected, the client treats it as an error:
	//   "if (gr.gr_token.length != 0 && maj_stat != GSS_S_CONTINUE_NEEDED) break;"
	//
	// When mutual auth IS required:
	// - Client's first gss_init_sec_context() returns GSS_S_CONTINUE_NEEDED
	// - Client sends AP-REQ, expects AP-REP in response
	// - Client calls gss_init_sec_context() again with AP-REP to complete context
	var apRepToken []byte
	var hasAcceptorSubkey bool

	if mutualRequired {
		apRepToken, err = buildAPRep(apReq, sessionKey)
		if err != nil {
			logger.Debug("Failed to build AP-REP (non-fatal)", "error", err)
			// Continue without mutual auth token - client may fail
		} else {
			logger.Debug("Built AP-REP token (mutual auth required)",
				"len", len(apRepToken),
				"first_bytes", fmt.Sprintf("%x", firstN(apRepToken, 32)),
			)
			// When we send AP-REP with subkey, client will use it as acceptor subkey
			hasAcceptorSubkey = hasSubkey(apReq)
		}
	} else {
		// No mutual auth - don't send AP-REP
		// Client will use initiator subkey (from authenticator) for MIC
		logger.Debug("Mutual auth not required, not sending AP-REP")
	}

	// Build the verified context
	// Use contextKey (which may be the subkey if present) for subsequent operations
	vc := &VerifiedContext{
		Principal:         clientPrincipal,
		Realm:             clientRealm,
		SessionKey:        contextKey, // Use subkey if present, otherwise session key
		APRepToken:        apRepToken,
		HasAcceptorSubkey: hasAcceptorSubkey,
	}

	return vc, nil
}

// extractAPReq strips the GSS-API initial context token wrapper if present.
//
// GSS-API initial context tokens (RFC 2743 Section 3.1) have the format:
//
//	0x60 [length] 0x06 [OID-length] [OID-bytes] [inner-token]
//
// For krb5, the OID is 1.2.840.113554.1.2.2 (9 bytes).
//
// Per RFC 1964 Section 1.1, the inner token for krb5 has a 2-byte token ID:
//
//	0x01 0x00 = AP-REQ (context establishment)
//	0x02 0x00 = AP-REP (mutual authentication reply)
//
// After stripping the GSS wrapper and token ID, we get the raw AP-REQ.
//
// If the token doesn't start with 0x60, it's treated as a raw AP-REQ.
func extractAPReq(token []byte) ([]byte, error) {
	if len(token) < 2 {
		return nil, fmt.Errorf("token too short: %d bytes", len(token))
	}

	// If not a GSS-API wrapped token, assume raw AP-REQ
	if token[0] != 0x60 {
		return token, nil
	}

	// Parse the ASN.1 application tag length
	offset := 1
	length, bytesRead, err := parseASN1Length(token[offset:])
	if err != nil {
		return nil, fmt.Errorf("parse GSS token length: %w", err)
	}
	offset += bytesRead

	// Validate total length
	if offset+int(length) > len(token) {
		return nil, fmt.Errorf("GSS token truncated: expected %d bytes, have %d", offset+int(length), len(token))
	}

	// Expect OID tag (0x06)
	if offset >= len(token) || token[offset] != 0x06 {
		return nil, fmt.Errorf("expected OID tag 0x06 at offset %d, got 0x%02x", offset, token[offset])
	}
	offset++

	// Read OID length
	if offset >= len(token) {
		return nil, fmt.Errorf("truncated OID length")
	}
	oidLen := int(token[offset])
	offset++

	// Skip the OID bytes (we could verify it's the krb5 OID, but we trust the caller)
	offset += oidLen

	if offset > len(token) {
		return nil, fmt.Errorf("truncated after OID")
	}

	// Per RFC 1964 Section 1.1, the inner token starts with a 2-byte token ID.
	// For AP-REQ, this is 0x01 0x00.
	// We need to skip these 2 bytes to get the raw AP-REQ.
	if offset+2 > len(token) {
		return nil, fmt.Errorf("truncated token ID")
	}

	tokenID := (uint16(token[offset]) << 8) | uint16(token[offset+1])
	if tokenID != 0x0100 {
		// Not an AP-REQ token ID. This could be:
		// - 0x0200: AP-REP
		// - 0x0300: Wrap (integrity)
		// - 0x0400: Wrap (privacy)
		// For INIT, we expect AP-REQ.
		return nil, fmt.Errorf("unexpected krb5 token ID: 0x%04x (expected 0x0100 for AP-REQ)", tokenID)
	}
	offset += 2

	// Everything after the token ID is the raw AP-REQ (ASN.1 APPLICATION 14)
	return token[offset:], nil
}

// buildAPRep constructs an AP-REP token for mutual authentication.
//
// Per RFC 4120 Section 5.5.2, the AP-REP contains:
//   - pvno: 5 (Kerberos version)
//   - msg-type: 15 (KRB_AP_REP)
//   - enc-part: EncAPRepPart encrypted with the session key
//
// The EncAPRepPart contains ctime and cusec copied from the authenticator,
// which proves to the client that we successfully decrypted the ticket.
//
// The returned token is wrapped in a GSS-API MechToken with token ID 0x02 0x00
// (AP-REP per RFC 1964).
func buildAPRep(apReq messages.APReq, sessionKey types.EncryptionKey) ([]byte, error) {
	// Build the EncAPRepPart with values from the authenticator
	// Per RFC 4120 Section 5.5.2, EncAPRepPart is APPLICATION 27 SEQUENCE containing:
	//   - ctime [0]: copied from authenticator (proves we decrypted it)
	//   - cusec [1]: copied from authenticator
	//   - subkey [2]: OPTIONAL - if client sent subkey, echo it back
	//   - seq-number [3]: OPTIONAL - sequence number for further exchanges
	//
	// CRITICAL: If the client sent a subkey in the authenticator, we MUST include
	// it in the EncAPRepPart. This tells the client we've accepted the subkey
	// and will use it for subsequent operations (MIC computation, wrap/unwrap).
	// Without this, the client won't know which key to use for gss_verify_mic().
	encAPRepPart := messages.EncAPRepPart{
		CTime: apReq.Authenticator.CTime,
		Cusec: apReq.Authenticator.Cusec,
	}

	// If client sent a subkey, include it in the AP-REP
	if hasSubkey(apReq) {
		encAPRepPart.Subkey = apReq.Authenticator.SubKey
		logger.Debug("Including subkey in EncAPRepPart",
			"etype", apReq.Authenticator.SubKey.KeyType,
			"key_len", len(apReq.Authenticator.SubKey.KeyValue),
		)
	}

	// Marshal the EncAPRepPart inner SEQUENCE first (without APPLICATION tag)
	encAPRepPartInner, err := asn1.Marshal(encAPRepPart)
	if err != nil {
		return nil, fmt.Errorf("marshal EncAPRepPart inner: %w", err)
	}

	// Add APPLICATION 27 tag using gokrb5's asn1tools
	// This produces: APPLICATION 27 { SEQUENCE { ... } }
	encAPRepPartBytes := asn1tools.AddASNAppTag(encAPRepPartInner, 27)

	logger.Debug("EncAPRepPart marshaled",
		"len", len(encAPRepPartBytes),
		"first_bytes", fmt.Sprintf("%x", firstN(encAPRepPartBytes, 32)),
		"ctime", encAPRepPart.CTime.Format("20060102150405Z"),
		"cusec", encAPRepPart.Cusec,
		"has_subkey", hasSubkey(apReq),
		"session_key_etype", sessionKey.KeyType,
	)

	// Encrypt the EncAPRepPart using the session key
	// Key usage 12 is for AP-REP encrypted part (RFC 4120 Section 7.5.1)
	encryptedData, err := crypto.GetEncryptedData(encAPRepPartBytes, sessionKey, 12, 0)
	if err != nil {
		return nil, fmt.Errorf("encrypt EncAPRepPart: %w", err)
	}

	// Build the AP-REP message
	apRep := messages.APRep{
		PVNO:    5,
		MsgType: 15, // KRB_AP_REP
		EncPart: encryptedData,
	}

	// Marshal the AP-REP inner SEQUENCE first (without APPLICATION tag)
	apRepInner, err := asn1.Marshal(apRep)
	if err != nil {
		return nil, fmt.Errorf("marshal AP-REP inner: %w", err)
	}

	// Add APPLICATION 15 tag using gokrb5's asn1tools
	// This produces: APPLICATION 15 { SEQUENCE { ... } }
	apRepBytes := asn1tools.AddASNAppTag(apRepInner, 15)

	logger.Debug("AP-REP marshaled",
		"len", len(apRepBytes),
		"first_bytes", fmt.Sprintf("%x", firstN(apRepBytes, 32)),
	)

	// Wrap in GSS-API MechToken format per RFC 1964:
	// 0x60 [length] OID 0x02 0x00 AP-REP
	return wrapGSSToken(apRepBytes, 0x0200), nil // 0x0200 = AP-REP token ID
}

// wrapGSSToken wraps a Kerberos message in a GSS-API MechToken.
//
// Format: 0x60 [ASN.1 length] [OID tag] [OID length] [OID bytes] [token ID] [inner token]
//
// The OID is 1.2.840.113554.1.2.2 (krb5 mechanism).
// The token ID identifies the inner token type (0x0100 = AP-REQ, 0x0200 = AP-REP, etc.).
func wrapGSSToken(innerToken []byte, tokenID uint16) []byte {
	// KRB5 OID: 1.2.840.113554.1.2.2 = 06 09 2a 86 48 86 f7 12 01 02 02
	krb5OID := []byte{0x06, 0x09, 0x2a, 0x86, 0x48, 0x86, 0xf7, 0x12, 0x01, 0x02, 0x02}

	// Token ID (2 bytes, big-endian)
	tokenIDBytes := []byte{byte(tokenID >> 8), byte(tokenID & 0xFF)}

	// Inner content: OID + token ID + inner token
	innerContent := make([]byte, 0, len(krb5OID)+len(tokenIDBytes)+len(innerToken))
	innerContent = append(innerContent, krb5OID...)
	innerContent = append(innerContent, tokenIDBytes...)
	innerContent = append(innerContent, innerToken...)

	// Encode the length in ASN.1 format
	lengthBytes := encodeASN1Length(len(innerContent))

	// Build final token: 0x60 [length] [content]
	result := make([]byte, 0, 1+len(lengthBytes)+len(innerContent))
	result = append(result, 0x60) // Application tag
	result = append(result, lengthBytes...)
	result = append(result, innerContent...)

	return result
}

// encodeASN1Length encodes a length value in ASN.1 format.
func encodeASN1Length(length int) []byte {
	if length < 128 {
		return []byte{byte(length)}
	}

	// Long form
	var lengthBytes []byte
	for length > 0 {
		lengthBytes = append([]byte{byte(length & 0xFF)}, lengthBytes...)
		length >>= 8
	}
	return append([]byte{byte(0x80 | len(lengthBytes))}, lengthBytes...)
}

// parseASN1Length parses an ASN.1 length field.
// Returns the length value, the number of bytes consumed, and any error.
func parseASN1Length(data []byte) (int, int, error) {
	if len(data) == 0 {
		return 0, 0, fmt.Errorf("empty length field")
	}

	first := data[0]
	if first < 0x80 {
		// Short form: length in a single byte
		return int(first), 1, nil
	}

	// Long form: first byte indicates number of length bytes
	numBytes := int(first & 0x7f)
	if numBytes == 0 || numBytes > 4 {
		return 0, 0, fmt.Errorf("invalid ASN.1 length: %d bytes", numBytes)
	}
	if 1+numBytes > len(data) {
		return 0, 0, fmt.Errorf("truncated ASN.1 length")
	}

	length := 0
	for i := 1; i <= numBytes; i++ {
		length = (length << 8) | int(data[i])
	}
	return length, 1 + numBytes, nil
}

// decodeOpaqueToken extracts the GSS token from an XDR-encoded opaque value.
//
// Per RFC 2203 Section 5.2.1, the INIT call arguments are:
//
//	struct rpc_gss_init_arg { opaque gss_token<>; };
//
// This function decodes the length-prefixed opaque value to get the raw token.
// XDR opaque format: 4-byte big-endian length + data + padding to 4-byte boundary.
func decodeOpaqueToken(data []byte) ([]byte, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("init arg too short: %d bytes", len(data))
	}

	// Read length (big-endian uint32)
	length := binary.BigEndian.Uint32(data[:4])
	if length == 0 {
		return nil, fmt.Errorf("empty GSS token")
	}

	// Validate length
	const maxTokenLen = 65536
	if length > maxTokenLen {
		return nil, fmt.Errorf("GSS token too long: %d bytes", length)
	}

	// Calculate total size with XDR padding
	paddedLen := int(length)
	if length%4 != 0 {
		paddedLen += 4 - int(length%4)
	}

	if 4+paddedLen > len(data) {
		return nil, fmt.Errorf("GSS token truncated: expected %d bytes, have %d", 4+paddedLen, len(data))
	}

	return data[4 : 4+int(length)], nil
}

// ============================================================================
// GSS Process Result
// ============================================================================

// GSSProcessResult contains the result of processing an RPCSEC_GSS call.
//
// For control messages (INIT/CONTINUE_INIT/DESTROY), GSSReply contains the
// encoded reply data and IsControl is true.
//
// For DATA messages, ProcessedData contains the unwrapped procedure arguments
// and Identity contains the resolved Unix identity for NFS permission checks.
type GSSProcessResult struct {
	// ProcessedData contains the unwrapped procedure arguments for DATA requests.
	// nil for control messages (INIT/DESTROY).
	ProcessedData []byte

	// Identity is the resolved Unix identity for DATA requests.
	// nil for control messages.
	Identity *metadata.Identity

	// GSSReply contains the encoded reply for control messages (INIT/DESTROY).
	// nil for DATA requests.
	GSSReply []byte

	// ReplyVerifier contains the GSS verifier for the reply message.
	// For INIT: empty (AUTH_NULL verifier).
	// For DATA: MIC of the sequence number.
	ReplyVerifier []byte

	// IsControl is true for INIT/CONTINUE_INIT/DESTROY, false for DATA.
	IsControl bool

	// SilentDiscard is true when the request should be silently dropped
	// (no reply sent). Per RFC 2203 Section 5.3.3.1, invalid sequence
	// numbers result in silent discard.
	SilentDiscard bool

	// SeqNum is the sequence number from the credential.
	// Needed for building the reply verifier.
	SeqNum uint32

	// Service is the service level from the context.
	// Needed for reply body wrapping (integrity/privacy).
	Service uint32

	// SessionKey is the session key from the GSS context.
	// Needed for computing the reply verifier (MIC of seq_num).
	// Only set for DATA requests.
	SessionKey types.EncryptionKey

	// HasAcceptorSubkey indicates whether the context uses an acceptor subkey.
	// When true, MIC tokens must have FLAG_ACCEPTOR_SUBKEY set.
	// This is true when we include a subkey in the AP-REP EncAPRepPart.
	HasAcceptorSubkey bool

	// Err is set if processing failed.
	Err error

	// AuthStat indicates the RPC auth_stat to return on error.
	// Only meaningful when Err != nil.
	// 0 means default (RPCSEC_GSS_CREDPROBLEM).
	AuthStat uint32
}

// ============================================================================
// GSSProcessor
// ============================================================================

// GSSProcessorOption is a functional option for configuring GSSProcessor.
type GSSProcessorOption func(*GSSProcessor)

// WithMetrics configures Prometheus metrics for the GSS processor.
//
// When set, the processor records context creations, destructions, auth failures,
// data request counts, and operation durations. If nil or not set, no metrics
// are recorded (zero overhead).
func WithMetrics(m *GSSMetrics) GSSProcessorOption {
	return func(p *GSSProcessor) {
		p.metrics = m
	}
}

// GSSProcessor orchestrates RPCSEC_GSS context lifecycle.
//
// It is the central component that intercepts auth flavor 6 (RPCSEC_GSS)
// at the RPC layer. It handles:
//   - RPCSEC_GSS_INIT: Context creation via AP-REQ verification
//   - RPCSEC_GSS_CONTINUE_INIT: Multi-round context establishment
//   - RPCSEC_GSS_DATA: Validation and unwrapping of data requests
//   - RPCSEC_GSS_DESTROY: Context teardown
//
// Thread Safety: All methods are safe for concurrent use.
type GSSProcessor struct {
	contexts *ContextStore
	verifier Verifier
	mapper   kerberos.IdentityMapper
	metrics  *GSSMetrics
	mu       sync.RWMutex
}

// NewGSSProcessor creates a new GSS processor.
//
// Parameters:
//   - verifier: Token verifier (use NewKrb5Verifier for production)
//   - mapper: Identity mapper for principal-to-UID/GID conversion
//   - maxContexts: Maximum concurrent GSS contexts (0 = unlimited)
//   - contextTTL: Time after which idle contexts expire
//   - opts: Optional configuration (e.g., WithMetrics)
//
// Returns:
//   - *GSSProcessor: Initialized processor
func NewGSSProcessor(verifier Verifier, mapper kerberos.IdentityMapper, maxContexts int, contextTTL time.Duration, opts ...GSSProcessorOption) *GSSProcessor {
	p := &GSSProcessor{
		contexts: NewContextStore(maxContexts, contextTTL),
		verifier: verifier,
		mapper:   mapper,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Process is the main entry point for RPCSEC_GSS call processing.
//
// It decodes the RPCSEC_GSS credential from the RPC call, determines the
// GSS procedure type, and routes to the appropriate handler.
//
// Parameters:
//   - credBody: The raw credential body from the RPC call (OpaqueAuth.Body)
//   - verifBody: The raw verifier body from the RPC call (OpaqueAuth.Body)
//   - requestBody: The procedure arguments (call body after cred/verifier)
//
// Returns:
//   - *GSSProcessResult: Result containing either control reply or unwrapped data
func (p *GSSProcessor) Process(credBody []byte, verifBody []byte, requestBody []byte) *GSSProcessResult {
	// Decode the RPCSEC_GSS credential
	cred, err := DecodeGSSCred(credBody)
	if err != nil {
		return &GSSProcessResult{
			Err: fmt.Errorf("decode GSS credential: %w", err),
		}
	}

	// Route based on GSS procedure
	switch cred.GSSProc {
	case RPCGSSInit, RPCGSSContinueInit:
		return p.handleInit(cred, requestBody)
	case RPCGSSData:
		return p.handleData(cred, verifBody, requestBody)
	case RPCGSSDestroy:
		return p.handleDestroy(cred)
	default:
		return &GSSProcessResult{
			Err: fmt.Errorf("unknown RPCSEC_GSS procedure: %d", cred.GSSProc),
		}
	}
}

// handleInit processes RPCSEC_GSS_INIT and RPCSEC_GSS_CONTINUE_INIT.
//
// For INIT:
// 1. The GSS token is in the requestBody (procedure arguments) per RFC 2203 Section 5.2.2
// 2. Extract the GSS token from the XDR-encoded rpc_gss_init_arg struct
// 3. Verify the AP-REQ via the Verifier interface
// 4. Create a new GSSContext with the session key and principal
// 5. CRITICAL: Store the context BEFORE building the reply
// 6. Encode the RPCGSSInitRes with handle, status, and seq_window
//
// For CONTINUE_INIT:
// - Not yet needed for krb5 (single round trip), but handled for completeness
func (p *GSSProcessor) handleInit(cred *RPCGSSCredV1, requestBody []byte) *GSSProcessResult {
	initStart := time.Now()

	p.mu.RLock()
	verifier := p.verifier
	mapper := p.mapper
	p.mu.RUnlock()

	if verifier == nil {
		return &GSSProcessResult{
			IsControl: true,
			Err:       fmt.Errorf("no GSS verifier configured"),
		}
	}

	// Extract the GSS token from the XDR-encoded rpc_gss_init_arg struct.
	// Per RFC 2203 Section 5.2.1, the init args are:
	//   struct rpc_gss_init_arg { opaque gss_token<>; };
	// So we need to decode the length-prefixed opaque value.
	logger.Debug("GSS INIT credential info",
		"gss_proc", cred.GSSProc,
		"seq_num", cred.SeqNum,
		"service", cred.Service,
		"handle_len", len(cred.Handle),
	)
	logger.Debug("GSS INIT raw requestBody",
		"len", len(requestBody),
		"first_bytes", fmt.Sprintf("%x", firstN(requestBody, 32)),
	)
	gssToken, err := decodeOpaqueToken(requestBody)
	if err != nil {
		logger.Debug("GSS INIT failed to decode token", "error", err)
		p.metrics.RecordContextCreation(false)
		p.metrics.RecordAuthFailure("credential_problem")
		p.metrics.RecordInitDuration(time.Since(initStart))
		return &GSSProcessResult{
			IsControl: true,
			Err:       fmt.Errorf("decode GSS init arg: %w", err),
		}
	}
	logger.Debug("GSS INIT decoded token",
		"len", len(gssToken),
		"first_bytes", fmt.Sprintf("%x", firstN(gssToken, 32)),
	)

	// Verify the GSS token (AP-REQ)
	verified, err := verifier.VerifyToken(gssToken)
	if err != nil {
		logger.Debug("GSS INIT verification failed", "error", err)

		p.metrics.RecordContextCreation(false)
		p.metrics.RecordAuthFailure("credential_problem")
		p.metrics.RecordInitDuration(time.Since(initStart))

		// Return an error init response per RFC 2203
		errRes := &RPCGSSInitRes{
			Handle:    nil,
			GSSMajor:  GSSDefectiveCredential,
			GSSMinor:  0,
			SeqWindow: 0,
			GSSToken:  nil,
		}
		errResBytes, encErr := EncodeGSSInitRes(errRes)
		if encErr != nil {
			return &GSSProcessResult{
				IsControl: true,
				Err:       fmt.Errorf("encode GSS error response: %w", encErr),
			}
		}
		return &GSSProcessResult{
			GSSReply:  errResBytes,
			IsControl: true,
			Err:       fmt.Errorf("GSS INIT failed: %w", err),
		}
	}

	// Generate a unique context handle
	handle, err := generateHandle()
	if err != nil {
		return &GSSProcessResult{
			IsControl: true,
			Err:       fmt.Errorf("generate context handle: %w", err),
		}
	}

	// Create the GSS context
	now := time.Now()
	gssCtx := &GSSContext{
		Handle:     handle,
		Principal:  verified.Principal,
		Realm:      verified.Realm,
		SessionKey: verified.SessionKey,
		SeqWindow:  NewSeqWindow(DefaultSeqWindowSize),
		Service:    cred.Service,
		CreatedAt:  now,
		LastUsed:   now,
	}

	// CRITICAL: Store context BEFORE building reply.
	// If the reply reaches the client before the context is stored,
	// the client's first DATA request will fail with "context not found".
	p.contexts.Store(gssCtx)

	// Map principal to Unix identity (for logging; identity is resolved on each DATA call)
	if mapper != nil {
		identity, mapErr := mapper.MapPrincipal(verified.Principal, verified.Realm)
		if mapErr != nil {
			logger.Debug("GSS identity mapping failed (non-fatal during INIT)",
				"principal", verified.Principal,
				"realm", verified.Realm,
				"error", mapErr,
			)
		} else if identity != nil && identity.UID != nil {
			logger.Debug("GSS context established",
				"principal", verified.Principal,
				"realm", verified.Realm,
				"uid", *identity.UID,
			)
		}
	}

	// Build the INIT response
	initRes := &RPCGSSInitRes{
		Handle:    handle,
		GSSMajor:  GSSComplete,
		GSSMinor:  0,
		SeqWindow: DefaultSeqWindowSize,
		GSSToken:  verified.APRepToken, // May be empty if mutual auth not supported
	}

	logger.Debug("GSS INIT response fields",
		"handle_len", len(handle),
		"gss_major", initRes.GSSMajor,
		"gss_minor", initRes.GSSMinor,
		"seq_window", initRes.SeqWindow,
		"gss_token_len", len(initRes.GSSToken),
	)

	resBytes, err := EncodeGSSInitRes(initRes)
	if err != nil {
		return &GSSProcessResult{
			IsControl: true,
			Err:       fmt.Errorf("encode GSS init response: %w", err),
		}
	}

	// Log detailed response structure for debugging
	logger.Debug("GSS INIT response encoded (rpc_gss_init_res)",
		"total_len", len(resBytes),
		"handle_len", len(handle),
		"gss_major", initRes.GSSMajor,
		"gss_minor", initRes.GSSMinor,
		"seq_window", initRes.SeqWindow,
		"gss_token_len", len(initRes.GSSToken),
		"hex_dump", fmt.Sprintf("%x", resBytes),
	)

	p.metrics.RecordContextCreation(true)
	p.metrics.RecordInitDuration(time.Since(initStart))

	return &GSSProcessResult{
		GSSReply:          resBytes,
		IsControl:         true,
		SeqNum:            cred.SeqNum,
		Service:           cred.Service,
		SessionKey:        verified.SessionKey,        // Needed for reply verifier MIC
		HasAcceptorSubkey: verified.HasAcceptorSubkey, // Needed for MIC flags
	}
}

// DefaultSeqWindowSize is the sequence window size advertised to clients.
// Exported so that the NFS connection handler can reference the same value
// when computing the INIT reply verifier.
const DefaultSeqWindowSize = 128

// hasSubkey returns true if the authenticator contains a valid subkey.
// This check is used during AP-REQ verification, AP-REP construction,
// and acceptor subkey detection.
func hasSubkey(apReq messages.APReq) bool {
	return apReq.Authenticator.SubKey.KeyType != 0 &&
		len(apReq.Authenticator.SubKey.KeyValue) > 0
}

// firstN returns the first n bytes of a slice, or the whole slice if shorter.
func firstN(b []byte, n int) []byte {
	if len(b) < n {
		return b
	}
	return b[:n]
}

// lastN returns the last n bytes of a slice, or the whole slice if shorter.
func lastN(b []byte, n int) []byte {
	if len(b) < n {
		return b
	}
	return b[len(b)-n:]
}

// handleData processes RPCSEC_GSS_DATA calls.
//
// DATA handling:
// 1. Look up the context by handle (RPCSEC_GSS_CREDPROBLEM if not found)
// 2. Validate the sequence number (silent discard if invalid per RFC 2203 Section 5.3.3.1)
// 3. Check for MAXSEQ exceeded (context must be destroyed per RFC 2203)
// 4. For svc_none (krb5 auth-only): requestBody IS the procedure arguments
// 5. For svc_integrity/privacy: not yet implemented (Plan 04)
// 6. Map principal to Unix identity via IdentityMapper
// 7. Return unwrapped procedure arguments and identity
func (p *GSSProcessor) handleData(cred *RPCGSSCredV1, verifBody []byte, requestBody []byte) *GSSProcessResult {
	dataStart := time.Now()

	// 1. Look up context by handle
	ctx, found := p.contexts.Lookup(cred.Handle)
	if !found {
		logger.Debug("GSS DATA: context not found for handle",
			"cred_service", cred.Service,
			"handle_len", len(cred.Handle),
		)
		p.metrics.RecordAuthFailure("context_problem")
		return &GSSProcessResult{
			Err:      fmt.Errorf("RPCSEC_GSS_CREDPROBLEM: context not found"),
			AuthStat: AuthStatCredProblem,
		}
	}

	// Log service level comparison for debugging krb5i issues
	if cred.Service != ctx.Service {
		logger.Warn("GSS DATA: service level mismatch",
			"cred_service", cred.Service,
			"ctx_service", ctx.Service,
			"principal", ctx.Principal,
		)
	}
	logger.Debug("GSS DATA: credential and context info",
		"cred_service", cred.Service,
		"ctx_service", ctx.Service,
		"seq_num", cred.SeqNum,
		"principal", ctx.Principal,
	)

	// 2. Check for MAXSEQ exceeded -- context must be destroyed per RFC 2203
	if cred.SeqNum >= MAXSEQ {
		logger.Debug("GSS DATA: sequence number exceeds MAXSEQ, destroying context",
			"seq_num", cred.SeqNum,
			"principal", ctx.Principal,
		)
		p.contexts.Delete(cred.Handle)
		p.metrics.RecordAuthFailure("context_problem")
		return &GSSProcessResult{
			Err:      fmt.Errorf("RPCSEC_GSS_CTXPROBLEM: sequence number exceeds MAXSEQ"),
			AuthStat: AuthStatCtxProblem,
		}
	}

	// 3. Validate sequence number via sliding window
	if !ctx.SeqWindow.Accept(cred.SeqNum) {
		// Per RFC 2203 Section 5.3.3.1: silent discard for sequence violations
		logger.Debug("GSS DATA: sequence number rejected (duplicate or out of window)",
			"seq_num", cred.SeqNum,
			"principal", ctx.Principal,
		)
		p.metrics.RecordAuthFailure("sequence_violation")
		return &GSSProcessResult{
			SilentDiscard: true,
		}
	}

	// 4. Process based on service level from CREDENTIAL (per-call), not context
	//
	// Per RFC 2203 Section 5.3.3.4:
	//   "The service field is set to rpc_gss_svc_none, rpc_gss_svc_integrity,
	//    or rpc_gss_svc_privacy to indicate the service used for the call data."
	//
	// The service level is determined PER-CALL via the credential, not locked
	// at context establishment. This allows clients to use different protection
	// levels for different operations (e.g., MOUNT with auth-only, NFS with integrity).
	//
	// SECURITY NOTE: We use the credential's service level for unwrapping because
	// that's what the client used to wrap the data. The session key from the context
	// is still used for cryptographic operations.
	var processedData []byte
	switch cred.Service {
	case RPCGSSSvcNone:
		// krb5 (auth-only): requestBody IS the procedure arguments, no unwrapping
		processedData = requestBody

	case RPCGSSSvcIntegrity:
		// krb5i: Unwrap integrity-protected data
		args, bodySeqNum, err := UnwrapIntegrity(ctx.SessionKey, cred.SeqNum, requestBody)
		if err != nil {
			logger.Debug("GSS DATA: integrity unwrap failed",
				"principal", ctx.Principal,
				"cred_service", cred.Service,
				"error", err,
			)
			p.metrics.RecordAuthFailure("integrity_failure")
			return &GSSProcessResult{
				Err: fmt.Errorf("integrity unwrap failed: %w", err),
			}
		}
		_ = bodySeqNum // Already validated by UnwrapIntegrity (dual validation)
		processedData = args

	case RPCGSSSvcPrivacy:
		// krb5p: Unwrap privacy-protected data
		args, bodySeqNum, err := UnwrapPrivacy(ctx.SessionKey, cred.SeqNum, requestBody)
		if err != nil {
			logger.Debug("GSS DATA: privacy unwrap failed",
				"principal", ctx.Principal,
				"cred_service", cred.Service,
				"error", err,
			)
			p.metrics.RecordAuthFailure("privacy_failure")
			return &GSSProcessResult{
				Err: fmt.Errorf("privacy unwrap failed: %w", err),
			}
		}
		_ = bodySeqNum // Already validated by UnwrapPrivacy (dual validation)
		processedData = args

	default:
		return &GSSProcessResult{
			Err: fmt.Errorf("unknown RPCSEC_GSS service level: %d", cred.Service),
		}
	}

	// 5. Map principal to Unix identity
	p.mu.RLock()
	mapper := p.mapper
	p.mu.RUnlock()

	var identity *metadata.Identity
	if mapper != nil {
		id, err := mapper.MapPrincipal(ctx.Principal, ctx.Realm)
		if err != nil {
			logger.Debug("GSS DATA: identity mapping failed",
				"principal", ctx.Principal,
				"realm", ctx.Realm,
				"error", err,
			)
			return &GSSProcessResult{
				Err: fmt.Errorf("identity mapping failed for %s@%s: %w", ctx.Principal, ctx.Realm, err),
			}
		}
		identity = id
	}

	logger.Debug("GSS DATA: request authenticated",
		"principal", ctx.Principal,
		"realm", ctx.Realm,
		"seq_num", cred.SeqNum,
		"cred_service", cred.Service,
	)

	p.metrics.RecordDataRequest(serviceLevelName(cred.Service), time.Since(dataStart))

	return &GSSProcessResult{
		ProcessedData: processedData,
		Identity:      identity,
		IsControl:     false,
		SeqNum:        cred.SeqNum,
		Service:       cred.Service, // Use credential's service level (per-call)
		SessionKey:    ctx.SessionKey,
	}
}

// handleDestroy processes RPCSEC_GSS_DESTROY.
//
// The client calls DESTROY to tear down the security context.
// Per RFC 2203, the server should reply even if the context is not found.
//
// Steps:
// 1. Look up context by handle
// 2. Delete context from store (if found)
// 3. Return empty reply with IsControl=true
func (p *GSSProcessor) handleDestroy(cred *RPCGSSCredV1) *GSSProcessResult {
	destroyStart := time.Now()

	// Lookup context (may not exist if already expired)
	_, found := p.contexts.Lookup(cred.Handle)
	if !found {
		logger.Debug("GSS DESTROY for unknown context (may have expired)")
	}

	// Delete the context
	p.contexts.Delete(cred.Handle)

	// Build an empty INIT response structure to serve as the DESTROY reply.
	// Per RFC 2203, the DESTROY reply uses the same format as INIT response
	// with an empty token.
	destroyRes := &RPCGSSInitRes{
		Handle:    cred.Handle,
		GSSMajor:  GSSComplete,
		GSSMinor:  0,
		SeqWindow: 0,
		GSSToken:  nil,
	}

	resBytes, err := EncodeGSSInitRes(destroyRes)
	if err != nil {
		return &GSSProcessResult{
			IsControl: true,
			Err:       fmt.Errorf("encode GSS destroy response: %w", err),
		}
	}

	if found {
		p.metrics.RecordContextDestruction()
	}
	p.metrics.RecordDestroyDuration(time.Since(destroyStart))

	return &GSSProcessResult{
		GSSReply:  resBytes,
		IsControl: true,
		SeqNum:    cred.SeqNum,
		Service:   cred.Service,
	}
}

// Stop shuts down the GSS processor and releases resources.
//
// This stops the context store's background cleanup goroutine.
// Must be called during server shutdown.
func (p *GSSProcessor) Stop() {
	p.contexts.Stop()
}

// ContextCount returns the number of active GSS contexts.
// Useful for metrics and monitoring.
func (p *GSSProcessor) ContextCount() int {
	return p.contexts.Count()
}

// SetVerifier replaces the verifier (supports hot-swap for keytab rotation).
func (p *GSSProcessor) SetVerifier(v Verifier) {
	p.mu.Lock()
	p.verifier = v
	p.mu.Unlock()
}

// SetMapper replaces the identity mapper.
func (p *GSSProcessor) SetMapper(m kerberos.IdentityMapper) {
	p.mu.Lock()
	p.mapper = m
	p.mu.Unlock()
}

package gss

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	"github.com/jcmturner/gokrb5/v8/types"

	kerbauth "github.com/marmos91/dittofs/internal/auth/kerberos"
	"github.com/marmos91/dittofs/internal/logger"
	nfsidentity "github.com/marmos91/dittofs/pkg/adapter/nfs/identity"
	pkgidentity "github.com/marmos91/dittofs/pkg/identity"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// apOptionsMutualRequired is the bit mask for mutual authentication in AP-Options.
// Per RFC 4120, AP-Options bit 2 is MUTUAL-REQUIRED. In ASN.1 BIT STRING encoding
// (MSB first), bit 2 maps to 0x20 in the first byte.
const apOptionsMutualRequired byte = 0x20

// VerifiedContext contains the result of a successful GSS token verification.
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

// Verifier abstracts the GSS token verification step, allowing the
// GSSProcessor to be tested without a real KDC.
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

// Krb5Verifier implements Verifier using the shared KerberosService for AP-REQ
// verification and AP-REP construction. NFS-specific concerns (GSS-API token
// wrapping/unwrapping) remain in this package.
type Krb5Verifier struct {
	kerbService *kerbauth.KerberosService
}

// NewKrb5Verifier creates a new production verifier that delegates to the
// shared KerberosService for AP-REQ verification and AP-REP construction.
func NewKrb5Verifier(kerbService *kerbauth.KerberosService) *Krb5Verifier {
	return &Krb5Verifier{kerbService: kerbService}
}

// VerifyToken verifies a GSS-API token using the shared KerberosService.
//
// The token may be wrapped in a GSS-API initial context token
// (0x60 application tag with KRB5 OID) or be a raw AP-REQ.
// We strip the NFS-specific GSS wrapper first, then delegate to KerberosService.
func (v *Krb5Verifier) VerifyToken(gssToken []byte) (*VerifiedContext, error) {
	// NFS-specific: extract raw AP-REQ from GSS-API token wrapper.
	// GSS initial context tokens have format: 0x60 [length] OID AP-REQ
	apReqBytes, err := extractAPReq(gssToken)
	if err != nil {
		return nil, fmt.Errorf("extract AP-REQ from GSS token: %w", err)
	}

	// Delegate AP-REQ verification to shared KerberosService.
	// This handles: unmarshal, VerifyAPREQ, authenticator decryption,
	// replay detection, and subkey preference.
	if v.kerbService == nil || v.kerbService.Provider() == nil {
		return nil, fmt.Errorf("kerberos service not configured")
	}
	provider := v.kerbService.Provider()
	authResult, err := v.kerbService.Authenticate(apReqBytes, provider.ServicePrincipal())
	if err != nil {
		return nil, fmt.Errorf("kerberos authenticate: %w", err)
	}

	// Check if mutual authentication is required (AP-Options bit 2)
	mutualRequired := len(authResult.APReq.APOptions.Bytes) > 0 &&
		(authResult.APReq.APOptions.Bytes[0]&apOptionsMutualRequired) != 0

	logger.Debug("AP-REQ verified via shared KerberosService",
		"principal", authResult.Principal,
		"realm", authResult.Realm,
		"mutual_required", mutualRequired,
		"has_subkey", kerbauth.HasSubkey(&authResult.APReq),
	)

	// Build AP-REP for mutual authentication if required.
	// The shared service returns raw AP-REP; we add the NFS GSS-API wrapper.
	var apRepToken []byte
	var hasAcceptorSubkey bool

	if mutualRequired {
		// Get the ticket session key for AP-REP encryption (not the context key which
		// may be the subkey). AP-REP is encrypted with the ticket session key per RFC 4120.
		ticketSessionKey := authResult.APReq.Ticket.DecryptedEncPart.Key

		rawAPRep, buildErr := v.kerbService.BuildMutualAuth(&authResult.APReq, ticketSessionKey)
		if buildErr != nil {
			logger.Debug("Failed to build AP-REP (non-fatal)", "error", buildErr)
		} else {
			// NFS-specific: wrap raw AP-REP in GSS-API token (0x60 + OID + 0x0200)
			apRepToken = kerbauth.WrapGSSToken(rawAPRep, kerbauth.KerberosV5OIDBytes, kerbauth.GSSTokenIDAPRep)
			logger.Debug("Built GSS-wrapped AP-REP token (mutual auth required)",
				"raw_len", len(rawAPRep),
				"wrapped_len", len(apRepToken),
			)
			hasAcceptorSubkey = kerbauth.HasSubkey(&authResult.APReq)
		}
	} else {
		logger.Debug("Mutual auth not required, not sending AP-REP")
	}

	return &VerifiedContext{
		Principal:         authResult.Principal,
		Realm:             authResult.Realm,
		SessionKey:        authResult.SessionKey,
		APRepToken:        apRepToken,
		HasAcceptorSubkey: hasAcceptorSubkey,
	}, nil
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
	if tokenID != kerbauth.GSSTokenIDAPReq {
		return nil, fmt.Errorf("unexpected krb5 token ID: 0x%04x (expected 0x%04x for AP-REQ)", tokenID, kerbauth.GSSTokenIDAPReq)
	}
	offset += 2

	// Everything after the token ID is the raw AP-REQ (ASN.1 APPLICATION 14)
	return token[offset:], nil
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

// GSSProcessorOption is a functional option for configuring GSSProcessor.
type GSSProcessorOption func(*GSSProcessor)

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
	mapper   nfsidentity.IdentityMapper // legacy mapper (used if resolver is nil)
	resolver *pkgidentity.Resolver      // centralized identity resolver
	mu       sync.RWMutex
}

// NewGSSProcessor creates a new GSS processor.
// Use NewKrb5Verifier for the production verifier.
// maxContexts of 0 means unlimited; contextTTL controls idle context expiry.
func NewGSSProcessor(verifier Verifier, mapper nfsidentity.IdentityMapper, maxContexts int, contextTTL time.Duration, opts ...GSSProcessorOption) *GSSProcessor {
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
// It decodes the credential, determines the GSS procedure type, and routes
// to the appropriate handler (INIT, DATA, or DESTROY).
func (p *GSSProcessor) Process(ctx context.Context, credBody []byte, verifBody []byte, requestBody []byte) *GSSProcessResult {
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
		return p.handleData(ctx, cred, verifBody, requestBody)
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
	p.mu.RLock()
	verifier := p.verifier
	p.mu.RUnlock()

	if verifier == nil {
		return &GSSProcessResult{
			IsControl: true,
			Err:       fmt.Errorf("no GSS verifier configured"),
		}
	}

	// Extract the GSS token from the XDR-encoded rpc_gss_init_arg struct
	logger.Debug("GSS INIT request",
		"gss_proc", cred.GSSProc,
		"seq_num", cred.SeqNum,
		"service", cred.Service,
		"body_len", len(requestBody),
	)
	gssToken, err := decodeOpaqueToken(requestBody)
	if err != nil {
		logger.Debug("GSS INIT failed to decode token", "error", err)
		return &GSSProcessResult{
			IsControl: true,
			Err:       fmt.Errorf("decode GSS init arg: %w", err),
		}
	}
	// Verify the GSS token (AP-REQ)
	verified, err := verifier.VerifyToken(gssToken)
	if err != nil {
		logger.Debug("GSS INIT verification failed", "error", err)

		// Return an error init response per RFC 2203
		errRes := &RPCGSSInitRes{
			GSSMajor: GSSDefectiveCredential,
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
			Err:       fmt.Errorf("verify GSS INIT token: %w", err),
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

	logger.Debug("GSS context established",
		"principal", verified.Principal,
		"realm", verified.Realm,
	)

	// Build the INIT response
	initRes := &RPCGSSInitRes{
		Handle:    handle,
		GSSMajor:  GSSComplete,
		SeqWindow: DefaultSeqWindowSize,
		GSSToken:  verified.APRepToken,
	}

	resBytes, err := EncodeGSSInitRes(initRes)
	if err != nil {
		return &GSSProcessResult{
			IsControl: true,
			Err:       fmt.Errorf("encode GSS init response: %w", err),
		}
	}

	logger.Debug("GSS INIT response encoded",
		"total_len", len(resBytes),
		"handle_len", len(handle),
		"gss_major", initRes.GSSMajor,
		"seq_window", initRes.SeqWindow,
		"gss_token_len", len(initRes.GSSToken),
	)

	return &GSSProcessResult{
		GSSReply:          resBytes,
		IsControl:         true,
		SeqNum:            cred.SeqNum,
		Service:           cred.Service,
		SessionKey:        verified.SessionKey,
		HasAcceptorSubkey: verified.HasAcceptorSubkey,
	}
}

// DefaultSeqWindowSize is the sequence window size advertised to clients.
// Exported so that the NFS connection handler can reference the same value
// when computing the INIT reply verifier.
const DefaultSeqWindowSize = 128

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
// 4. Unwrap based on service level (svc_none / svc_integrity / svc_privacy)
// 5. Map principal to Unix identity via IdentityMapper
// 6. Return unwrapped procedure arguments and identity
func (p *GSSProcessor) handleData(ctx context.Context, cred *RPCGSSCredV1, verifBody []byte, requestBody []byte) *GSSProcessResult {
	// 1. Look up context by handle
	gssCtx, found := p.contexts.Lookup(cred.Handle)
	if !found {
		logger.Debug("GSS DATA: context not found for handle",
			"cred_service", cred.Service,
			"handle_len", len(cred.Handle),
		)
		return &GSSProcessResult{
			Err:      fmt.Errorf("RPCSEC_GSS_CREDPROBLEM: context not found"),
			AuthStat: AuthStatCredProblem,
		}
	}

	// Log service level comparison for debugging krb5i issues
	if cred.Service != gssCtx.Service {
		logger.Warn("GSS DATA: service level mismatch",
			"cred_service", cred.Service,
			"ctx_service", gssCtx.Service,
			"principal", gssCtx.Principal,
		)
	}
	logger.Debug("GSS DATA: credential and context info",
		"cred_service", cred.Service,
		"ctx_service", gssCtx.Service,
		"seq_num", cred.SeqNum,
		"principal", gssCtx.Principal,
	)

	// 2. Check for MAXSEQ exceeded -- context must be destroyed per RFC 2203
	if cred.SeqNum >= MAXSEQ {
		logger.Debug("GSS DATA: sequence number exceeds MAXSEQ, destroying context",
			"seq_num", cred.SeqNum,
			"principal", gssCtx.Principal,
		)
		p.contexts.Delete(cred.Handle)
		return &GSSProcessResult{
			Err:      fmt.Errorf("RPCSEC_GSS_CTXPROBLEM: sequence number exceeds MAXSEQ"),
			AuthStat: AuthStatCtxProblem,
		}
	}

	// 3. Validate sequence number via sliding window
	if !gssCtx.SeqWindow.Accept(cred.SeqNum) {
		// Per RFC 2203 Section 5.3.3.1: silent discard for sequence violations
		logger.Debug("GSS DATA: sequence number rejected (duplicate or out of window)",
			"seq_num", cred.SeqNum,
			"principal", gssCtx.Principal,
		)
		return &GSSProcessResult{
			SilentDiscard: true,
		}
	}

	// 4. Unwrap based on credential's service level (per-call, per RFC 2203 Section 5.3.3.4).
	// The session key from the context is used for cryptographic operations.
	var processedData []byte
	switch cred.Service {
	case RPCGSSSvcNone:
		processedData = requestBody

	case RPCGSSSvcIntegrity:
		args, _, err := UnwrapIntegrity(gssCtx.SessionKey, cred.SeqNum, requestBody)
		if err != nil {
			logger.Debug("GSS DATA: integrity unwrap failed",
				"principal", gssCtx.Principal,
				"cred_service", cred.Service,
				"error", err,
			)
			return &GSSProcessResult{
				Err: fmt.Errorf("unwrap integrity: %w", err),
			}
		}
		processedData = args

	case RPCGSSSvcPrivacy:
		args, _, err := UnwrapPrivacy(gssCtx.SessionKey, cred.SeqNum, requestBody)
		if err != nil {
			logger.Debug("GSS DATA: privacy unwrap failed",
				"principal", gssCtx.Principal,
				"cred_service", cred.Service,
				"error", err,
			)
			return &GSSProcessResult{
				Err: fmt.Errorf("unwrap privacy: %w", err),
			}
		}
		processedData = args

	default:
		return &GSSProcessResult{
			Err: fmt.Errorf("unknown RPCSEC_GSS service level: %d", cred.Service),
		}
	}

	// 5. Map principal to Unix identity via centralized resolver or legacy mapper.
	ident, identErr := p.resolveIdentity(ctx, gssCtx.Principal, gssCtx.Realm)
	if identErr != nil {
		return &GSSProcessResult{Err: identErr}
	}

	logger.Debug("GSS DATA: request authenticated",
		"principal", gssCtx.Principal,
		"realm", gssCtx.Realm,
		"seq_num", cred.SeqNum,
		"cred_service", cred.Service,
	)

	return &GSSProcessResult{
		ProcessedData: processedData,
		Identity:      ident,
		IsControl:     false,
		SeqNum:        cred.SeqNum,
		Service:       cred.Service,
		SessionKey:    gssCtx.SessionKey,
	}
}

// resolveIdentity maps a Kerberos principal to a Unix identity using the
// centralized resolver (DB-backed) with fallback to the legacy static mapper.
// Returns a nobody identity when the principal is not found.
func (p *GSSProcessor) resolveIdentity(ctx context.Context, principal, realm string) (*metadata.Identity, error) {
	p.mu.RLock()
	resolver := p.resolver
	legacyMapper := p.mapper
	p.mu.RUnlock()

	principalKey := principal + "@" + realm

	// Try centralized resolver first (DB-backed mapping + convention fallback).
	if resolver != nil {
		cred := &pkgidentity.Credential{
			Provider:   "kerberos",
			ExternalID: principalKey,
			Attributes: map[string]string{"realm": realm},
		}
		resolved, err := resolver.Resolve(ctx, cred)
		if err != nil {
			return nil, fmt.Errorf("identity resolver unavailable for %s@%s: %w", principal, realm, err)
		}
		if resolved.Found {
			return &metadata.Identity{
				UID:      &resolved.UID,
				GID:      &resolved.GID,
				GIDs:     resolved.GIDs,
				Username: resolved.Username,
				Domain:   resolved.Domain,
			}, nil
		} else {
			nobody := pkgidentity.NobodyIdentity()
			return &metadata.Identity{
				UID:      &nobody.UID,
				GID:      &nobody.GID,
				Username: nobody.Username,
			}, nil
		}
	}

	// Legacy static mapper fallback.
	if legacyMapper != nil {
		resolved, err := legacyMapper.Resolve(ctx, principalKey)
		if err != nil {
			return nil, fmt.Errorf("map identity for %s: %w", principalKey, err)
		}
		if resolved == nil {
			return nil, fmt.Errorf("identity mapping returned nil for %s", principalKey)
		}
		if !resolved.Found {
			resolved = nfsidentity.NobodyIdentity()
		}
		return &metadata.Identity{
			UID:      &resolved.UID,
			GID:      &resolved.GID,
			GIDs:     resolved.GIDs,
			Username: resolved.Username,
			Domain:   resolved.Domain,
		}, nil
	}

	return nil, fmt.Errorf("no identity mapper configured for %s", principalKey)
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
	// Lookup context (may not exist if already expired)
	_, found := p.contexts.Lookup(cred.Handle)
	if !found {
		logger.Debug("GSS DESTROY for unknown context (may have expired)")
	}

	// Delete the context
	p.contexts.Delete(cred.Handle)

	// Per RFC 2203, DESTROY reply uses the same format as INIT response
	destroyRes := &RPCGSSInitRes{
		Handle:   cred.Handle,
		GSSMajor: GSSComplete,
	}

	resBytes, err := EncodeGSSInitRes(destroyRes)
	if err != nil {
		return &GSSProcessResult{
			IsControl: true,
			Err:       fmt.Errorf("encode GSS destroy response: %w", err),
		}
	}

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

// SetMapper replaces the legacy identity mapper.
func (p *GSSProcessor) SetMapper(m nfsidentity.IdentityMapper) {
	p.mu.Lock()
	p.mapper = m
	p.mu.Unlock()
}

// SetResolver sets the centralized identity resolver, which takes precedence
// over the legacy mapper when both are set.
func (p *GSSProcessor) SetResolver(r *pkgidentity.Resolver) {
	p.mu.Lock()
	p.resolver = r
	p.mu.Unlock()
}

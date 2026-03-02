// Package auth provides authentication for SMB protocol adapters.
//
// This file provides SPNEGO (Simple and Protected GSSAPI Negotiation Mechanism)
// parsing and building. It wraps github.com/jcmturner/gokrb5/v8/spnego to
// provide a clean interface for extracting mechanism tokens and building
// server responses.
//
// SPNEGO is defined in RFC 4178 and is used by:
//   - SMB: Wraps NTLM or Kerberos tokens in SESSION_SETUP
//   - NFSv4: Used with RPCSEC_GSS for Kerberos authentication
package auth

import (
	"errors"
	"fmt"

	"github.com/jcmturner/gofork/encoding/asn1"
	"github.com/jcmturner/gokrb5/v8/gssapi"
	"github.com/jcmturner/gokrb5/v8/spnego"
	"github.com/jcmturner/gokrb5/v8/types"
)

// Well-known mechanism OIDs used in SPNEGO negotiation.
// These identify the authentication mechanism being used.
var (
	// OIDMSKerberosV5 is Microsoft's Kerberos 5 OID (1.2.840.48018.1.2.2).
	// Used by Windows clients for Kerberos authentication.
	OIDMSKerberosV5 = asn1.ObjectIdentifier{1, 2, 840, 48018, 1, 2, 2}

	// OIDKerberosV5 is the standard Kerberos 5 OID (1.2.840.113554.1.2.2).
	// Defined in RFC 4121.
	OIDKerberosV5 = asn1.ObjectIdentifier{1, 2, 840, 113554, 1, 2, 2}

	// OIDNTLMSSP is the NTLM Security Support Provider OID (1.3.6.1.4.1.311.2.2.10).
	// Used for NTLM authentication over SPNEGO.
	OIDNTLMSSP = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 311, 2, 2, 10}

	// OIDSPNEGO is the SPNEGO mechanism OID (1.3.6.1.5.5.2).
	// Identifies the outer GSSAPI wrapper.
	OIDSPNEGO = asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 2}
)

// NegState represents the state of SPNEGO negotiation.
// [RFC 4178] Section 4.2.2
type NegState int

const (
	// NegStateAcceptCompleted indicates successful authentication.
	NegStateAcceptCompleted NegState = 0

	// NegStateAcceptIncomplete indicates more tokens are needed.
	NegStateAcceptIncomplete NegState = 1

	// NegStateReject indicates authentication was rejected.
	NegStateReject NegState = 2

	// NegStateRequestMIC indicates a MIC is required.
	NegStateRequestMIC NegState = 3
)

// Error types for SPNEGO parsing.
var (
	ErrInvalidToken    = errors.New("spnego: invalid token format")
	ErrUnsupportedMech = errors.New("spnego: unsupported mechanism")
	ErrNoMechToken     = errors.New("spnego: no mechanism token present")
	ErrUnexpectedToken = errors.New("spnego: unexpected token type")
)

// TokenType indicates whether a token is an init or response token.
type TokenType int

const (
	// TokenTypeInit is a NegTokenInit (client's first message).
	TokenTypeInit TokenType = iota

	// TokenTypeResp is a NegTokenResp (server response or subsequent client message).
	TokenTypeResp
)

// ParsedToken contains the result of parsing a SPNEGO token.
type ParsedToken struct {
	// Type indicates whether this is an init or response token.
	Type TokenType

	// MechTypes lists the mechanisms offered (only for TokenTypeInit).
	MechTypes []asn1.ObjectIdentifier

	// MechToken is the inner mechanism token (e.g., NTLM message).
	MechToken []byte

	// NegState is the negotiation state (only for TokenTypeResp).
	NegState NegState

	// SupportedMech is the selected mechanism (only for TokenTypeResp).
	SupportedMech asn1.ObjectIdentifier

	// MechListBytes is the DER-encoded SEQUENCE OF OIDs from the NegTokenInit.
	// This is the raw bytes used for mechListMIC computation per RFC 4178.
	// Only populated for TokenTypeInit.
	MechListBytes []byte

	// MechListMIC is the mechanism list MIC from the token.
	// Present in NegTokenInit (client-sent MIC) or NegTokenResp (server-sent MIC).
	MechListMIC []byte
}

// Parse parses a SPNEGO token and extracts its contents.
//
// The input can be either:
//   - A GSSAPI-wrapped token (starts with 0x60)
//   - A raw NegTokenInit (starts with 0xa0)
//   - A raw NegTokenResp (starts with 0xa1)
//
// Returns a ParsedToken containing the mechanism token and metadata.
func Parse(data []byte) (*ParsedToken, error) {
	if len(data) < 2 {
		return nil, ErrInvalidToken
	}

	isInit, token, err := spnego.UnmarshalNegToken(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}

	if isInit {
		initToken, ok := token.(spnego.NegTokenInit)
		if !ok {
			return nil, ErrInvalidToken
		}

		// Capture the DER-encoded mechTypes for MIC computation.
		// Per RFC 4178, the MIC is computed over the DER SEQUENCE OF OIDs.
		mechListBytes := marshalMechTypes(initToken.MechTypes)

		return &ParsedToken{
			Type:          TokenTypeInit,
			MechTypes:     initToken.MechTypes,
			MechToken:     initToken.MechTokenBytes,
			MechListBytes: mechListBytes,
			MechListMIC:   initToken.MechListMIC,
		}, nil
	}

	respToken, ok := token.(spnego.NegTokenResp)
	if !ok {
		return nil, ErrInvalidToken
	}

	return &ParsedToken{
		Type:          TokenTypeResp,
		MechToken:     respToken.ResponseToken,
		NegState:      NegState(respToken.NegState),
		SupportedMech: respToken.SupportedMech,
		MechListMIC:   respToken.MechListMIC,
	}, nil
}

// HasMechanism checks if the parsed token offers a specific mechanism.
// Only valid for TokenTypeInit tokens.
func (p *ParsedToken) HasMechanism(oid asn1.ObjectIdentifier) bool {
	for _, mech := range p.MechTypes {
		if mech.Equal(oid) {
			return true
		}
	}
	return false
}

// HasNTLM returns true if the token offers NTLM authentication.
func (p *ParsedToken) HasNTLM() bool {
	return p.HasMechanism(OIDNTLMSSP)
}

// HasKerberos returns true if the token offers Kerberos authentication.
func (p *ParsedToken) HasKerberos() bool {
	return p.HasMechanism(OIDKerberosV5) || p.HasMechanism(OIDMSKerberosV5)
}

// BuildResponse creates a SPNEGO NegTokenResp for server responses.
//
// Parameters:
//   - state: The negotiation state (accept, reject, incomplete)
//   - mech: The selected mechanism OID (can be nil if rejecting)
//   - responseToken: The mechanism-specific response (e.g., NTLM challenge)
//
// Returns the ASN.1 DER-encoded NegTokenResp.
func BuildResponse(state NegState, mech asn1.ObjectIdentifier, responseToken []byte) ([]byte, error) {
	resp := spnego.NegTokenResp{
		NegState:      asn1.Enumerated(state),
		SupportedMech: mech,
		ResponseToken: responseToken,
	}

	return resp.Marshal()
}

// BuildAcceptIncomplete creates a NegTokenResp indicating more tokens are needed.
// Used when sending the NTLM challenge (Type 2) message.
func BuildAcceptIncomplete(mech asn1.ObjectIdentifier, responseToken []byte) ([]byte, error) {
	return BuildResponse(NegStateAcceptIncomplete, mech, responseToken)
}

// BuildAcceptComplete creates a NegTokenResp indicating successful authentication.
// Used after validating the NTLM authenticate (Type 3) message.
func BuildAcceptComplete(mech asn1.ObjectIdentifier, responseToken []byte) ([]byte, error) {
	return BuildResponse(NegStateAcceptCompleted, mech, responseToken)
}

// BuildAcceptCompleteWithMIC creates a NegTokenResp indicating successful authentication,
// including the mechListMIC field for SPNEGO downgrade protection per RFC 4178.
//
// Parameters:
//   - mech: The selected mechanism OID (e.g., OIDKerberosV5)
//   - mechToken: The mechanism-specific response token (e.g., AP-REP)
//   - mechListMIC: The computed MIC over the mechList (may be nil)
//
// Returns the ASN.1 DER-encoded NegTokenResp with MIC field.
func BuildAcceptCompleteWithMIC(mech asn1.ObjectIdentifier, mechToken []byte, mechListMIC []byte) ([]byte, error) {
	resp := spnego.NegTokenResp{
		NegState:      asn1.Enumerated(NegStateAcceptCompleted),
		SupportedMech: mech,
		ResponseToken: mechToken,
		MechListMIC:   mechListMIC,
	}

	return resp.Marshal()
}

// BuildReject creates a NegTokenResp indicating authentication failure.
func BuildReject() ([]byte, error) {
	return BuildResponse(NegStateReject, nil, nil)
}

// KeyUsageAcceptorSign is the Kerberos key usage for acceptor (server) MIC tokens.
// RFC 4121 Section 2: KG-USAGE-ACCEPTOR-SIGN = 23.
const KeyUsageAcceptorSign uint32 = 23

// KeyUsageInitiatorSign is the Kerberos key usage for initiator (client) MIC tokens.
// RFC 4121 Section 2: KG-USAGE-INITIATOR-SIGN = 25.
const KeyUsageInitiatorSign uint32 = 25

// ComputeMechListMIC computes a GSS-API MIC over the SPNEGO mechList for downgrade protection.
// Per RFC 4178, the MIC protects the DER-encoded SEQUENCE OF OIDs from the original NegTokenInit.
// Uses Kerberos GSS-API MIC token format with key usage 23 (acceptor sign) per RFC 4121,
// since the server is the acceptor computing this MIC.
func ComputeMechListMIC(sessionKey types.EncryptionKey, mechListBytes []byte) ([]byte, error) {
	micToken := gssapi.MICToken{
		Flags:     gssapi.MICTokenFlagSentByAcceptor,
		SndSeqNum: 0, // Sequence number 0 for SPNEGO MIC
		Payload:   mechListBytes,
	}

	if err := micToken.SetChecksum(sessionKey, KeyUsageAcceptorSign); err != nil {
		return nil, fmt.Errorf("compute mechListMIC: %w", err)
	}

	micBytes, err := micToken.Marshal()
	if err != nil {
		return nil, fmt.Errorf("marshal mechListMIC: %w", err)
	}

	return micBytes, nil
}

// VerifyMechListMIC verifies a client-sent SPNEGO mechListMIC.
// The client is the initiator, so the MIC uses key usage 25 (initiator sign) per RFC 4121.
// Returns nil if the MIC is valid, error if verification fails.
func VerifyMechListMIC(sessionKey types.EncryptionKey, mechListBytes []byte, micBytes []byte) error {
	var micToken gssapi.MICToken
	if err := micToken.Unmarshal(micBytes, false); err != nil {
		return fmt.Errorf("unmarshal mechListMIC: %w", err)
	}

	// Set the payload for verification (the MIC was computed over the mechList bytes)
	micToken.Payload = mechListBytes

	ok, err := micToken.Verify(sessionKey, KeyUsageInitiatorSign)
	if err != nil {
		return fmt.Errorf("verify mechListMIC: %w", err)
	}
	if !ok {
		return fmt.Errorf("mechListMIC verification failed")
	}

	return nil
}

// marshalMechTypes DER-encodes a list of OIDs into a SEQUENCE OF ObjectIdentifier.
// This produces the raw bytes needed for mechListMIC computation per RFC 4178.
func marshalMechTypes(mechTypes []asn1.ObjectIdentifier) []byte {
	if len(mechTypes) == 0 {
		return nil
	}

	// Marshal as ASN.1 SEQUENCE OF OID
	data, err := asn1.Marshal(mechTypes)
	if err != nil {
		return nil
	}

	return data
}

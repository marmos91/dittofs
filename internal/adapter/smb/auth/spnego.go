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
	"github.com/jcmturner/gokrb5/v8/spnego"
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

		return &ParsedToken{
			Type:      TokenTypeInit,
			MechTypes: initToken.MechTypes,
			MechToken: initToken.MechTokenBytes,
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

// BuildReject creates a NegTokenResp indicating authentication failure.
func BuildReject() ([]byte, error) {
	return BuildResponse(NegStateReject, nil, nil)
}

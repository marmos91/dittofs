// Package gss implements RPCSEC_GSS reply verifier computation.
//
// Per RFC 2203 Section 5.3.3.2, the reply verifier for RPCSEC_GSS DATA requests
// is a MIC (Message Integrity Code) computed over the XDR-encoded sequence number.
// This proves to the client that the server holds the session key.
//
// For krb5 mechanism, the MIC is computed using RFC 4121 GSS-API MICToken
// with KeyUsageAcceptorSign (25).
package gss

import (
	"encoding/binary"
	"fmt"

	"github.com/jcmturner/gokrb5/v8/gssapi"
	"github.com/jcmturner/gokrb5/v8/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/rpc"
)

// ComputeReplyVerifier computes the RPCSEC_GSS reply verifier for DATA requests.
//
// Per RFC 2203 Section 5.3.3.2, the reply verifier is the MIC of the
// XDR-encoded sequence number. The MIC is computed using the GSS-API
// MICToken (RFC 4121) with the session key from the security context.
//
// Parameters:
//   - sessionKey: The session key from the GSS context (decrypted service ticket)
//   - seqNum: The sequence number from the RPCSEC_GSS credential
//
// Returns:
//   - []byte: The MIC token bytes (reply verifier body)
//   - error: If MIC computation fails (e.g., unsupported encryption type)
func ComputeReplyVerifier(sessionKey types.EncryptionKey, seqNum uint32) ([]byte, error) {
	// XDR-encode the sequence number as uint32 (4 bytes big-endian)
	seqBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(seqBytes, seqNum)

	// Create a GSS-API MIC token per RFC 4121
	micToken := gssapi.MICToken{
		Flags:     gssapi.MICTokenFlagSentByAcceptor, // Server is the acceptor
		SndSeqNum: uint64(seqNum),
		Payload:   seqBytes,
	}

	// Compute the checksum using the session key with acceptor sign key usage.
	// RFC 4121 defines key usage 25 for acceptor (server) MIC tokens.
	if err := micToken.SetChecksum(sessionKey, KeyUsageAcceptorSign); err != nil {
		return nil, fmt.Errorf("compute MIC for reply verifier: %w", err)
	}

	// Marshal the MIC token to wire format
	micBytes, err := micToken.Marshal()
	if err != nil {
		return nil, fmt.Errorf("marshal MIC token: %w", err)
	}

	return micBytes, nil
}

// WrapReplyVerifier wraps MIC bytes into an OpaqueAuth suitable for an RPC reply verifier.
//
// The reply verifier for RPCSEC_GSS uses auth flavor 6 (RPCSEC_GSS) and
// the MIC token as the body. This is different from the typical AUTH_NULL
// verifier used in non-GSS replies.
//
// Parameters:
//   - mic: The MIC token bytes from ComputeReplyVerifier
//
// Returns:
//   - rpc.OpaqueAuth: Verifier with Flavor=6 (RPCSEC_GSS) and Body=mic
func WrapReplyVerifier(mic []byte) rpc.OpaqueAuth {
	return rpc.OpaqueAuth{
		Flavor: rpc.AuthRPCSECGSS,
		Body:   mic,
	}
}

// ComputeInitVerifier computes the RPCSEC_GSS reply verifier for INIT responses.
//
// Per RFC 2203 Section 5.3.3.2, when the server returns GSS_S_COMPLETE, the reply
// verifier contains the checksum (MIC) of the sequence window size. This proves
// to the client that the server has established the context successfully.
//
// Parameters:
//   - sessionKey: The session key from the GSS context (may be subkey from authenticator)
//   - seqWindow: The sequence window size from rpc_gss_init_res
//   - hasAcceptorSubkey: Whether the context uses an acceptor subkey (from AP-REP)
//
// Returns:
//   - []byte: The MIC token bytes (reply verifier body)
//   - error: If MIC computation fails (e.g., unsupported encryption type)
func ComputeInitVerifier(sessionKey types.EncryptionKey, seqWindow uint32, hasAcceptorSubkey bool) ([]byte, error) {
	// XDR-encode the sequence window as uint32 (4 bytes big-endian)
	// This matches the client's verification: seq = htonl(gr.gr_win)
	winBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(winBytes, seqWindow)

	// Build flags for the MIC token.
	// Per RFC 4121 Section 4.2.2:
	// - Bit 0 (SentByAcceptor): Set because server is the acceptor
	// - Bit 2 (AcceptorSubkey): Set when acceptor subkey is used for the MIC
	//
	// CRITICAL: When we include a subkey in EncAPRepPart, the client sets
	// ctx->have_acceptor_subkey = 1 and expects FLAG_ACCEPTOR_SUBKEY in MIC tokens.
	// Without this flag, gss_verify_mic() may fail or use wrong key.
	var flags byte = gssapi.MICTokenFlagSentByAcceptor
	if hasAcceptorSubkey {
		flags |= gssapi.MICTokenFlagAcceptorSubkey
	}

	// Create a GSS-API MIC token per RFC 4121
	// For INIT response, we use sequence number 0 since context is being established
	micToken := gssapi.MICToken{
		Flags:     flags,
		SndSeqNum: 0, // Initial sequence
		Payload:   winBytes,
	}

	// Compute the checksum using the session key with acceptor sign key usage.
	// RFC 4121 defines key usage 25 for acceptor (server) MIC tokens.
	if err := micToken.SetChecksum(sessionKey, KeyUsageAcceptorSign); err != nil {
		return nil, fmt.Errorf("compute MIC for INIT verifier: %w", err)
	}

	// Marshal the MIC token to wire format
	micBytes, err := micToken.Marshal()
	if err != nil {
		return nil, fmt.Errorf("marshal MIC token: %w", err)
	}

	return micBytes, nil
}

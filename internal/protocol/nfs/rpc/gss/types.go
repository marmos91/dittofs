// Package gss implements RPCSEC_GSS wire protocol types per RFC 2203 and RFC 5403.
//
// This package provides the XDR encoding/decoding for RPCSEC_GSS credentials,
// initialization responses, and associated constants. These types form the
// foundation for Kerberos (RPCSEC_GSS with krb5 mechanism) authentication
// in NFS.
//
// Key types:
//   - RPCGSSCredV1: The RPCSEC_GSS credential sent in RPC call headers
//   - RPCGSSInitRes: The server's response during context establishment
//   - SeqWindow: Sliding window for replay detection (in sequence.go)
//
// References:
//   - RFC 2203: RPCSEC_GSS Protocol Specification
//   - RFC 2743: Generic Security Service Application Program Interface Version 2
//   - RFC 4121: The Kerberos Version 5 GSS-API Mechanism (krb5 key usage)
package gss

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// ============================================================================
// RPCSEC_GSS Constants
// ============================================================================

// Authentication flavor for RPCSEC_GSS.
// RFC 2203 Section 1: RPCSEC_GSS uses auth flavor 6.
const AuthRPCSECGSS uint32 = 6

// RPCSEC_GSS version. Only version 1 is defined.
const RPCGSSVers1 uint32 = 1

// RPCSEC_GSS procedure values (gss_proc field in credential).
// These indicate the purpose of the RPC call within the GSS context lifecycle.
const (
	// RPCGSSData indicates a normal data exchange call.
	// The call body is integrity-protected or privacy-protected
	// depending on the service level.
	RPCGSSData uint32 = 0

	// RPCGSSInit indicates a context establishment call.
	// The client sends the initial GSS token to begin authentication.
	RPCGSSInit uint32 = 1

	// RPCGSSContinueInit indicates a continuation of context establishment.
	// Used when the GSS mechanism requires multiple round trips.
	RPCGSSContinueInit uint32 = 2

	// RPCGSSDestroy indicates a context destruction call.
	// The client requests teardown of the security context.
	RPCGSSDestroy uint32 = 3
)

// RPCSEC_GSS service levels.
// These determine how the RPC call body is protected.
const (
	// RPCGSSSvcNone provides authentication only (no integrity or privacy).
	// The call body is sent in cleartext.
	RPCGSSSvcNone uint32 = 1

	// RPCGSSSvcIntegrity provides authentication and integrity protection.
	// The call body includes a MIC (Message Integrity Code) computed
	// over the sequence number and call arguments.
	RPCGSSSvcIntegrity uint32 = 2

	// RPCGSSSvcPrivacy provides authentication, integrity, and privacy.
	// The call body is encrypted and integrity-protected.
	RPCGSSSvcPrivacy uint32 = 3
)

// MAXSEQ is the maximum allowed sequence number per RFC 2203 Section 5.3.3.1.
// Sequence numbers must not exceed this value to prevent wraparound issues.
const MAXSEQ uint32 = 0x80000000

// ============================================================================
// GSS Major Status Codes (RFC 2743 Section 1.2.1.1)
// ============================================================================

const (
	// GSSComplete indicates the GSS operation completed successfully.
	GSSComplete uint32 = 0

	// GSSContinueNeeded indicates more tokens are needed to complete
	// context establishment. The client should send the response token
	// back to the server.
	GSSContinueNeeded uint32 = 1

	// GSSDefectiveCredential indicates the supplied credential was defective.
	GSSDefectiveCredential uint32 = 2
)

// ============================================================================
// Kerberos 5 OID and Pseudo-Flavors
// ============================================================================

// KRB5OID is the Kerberos 5 mechanism OID: 1.2.840.113554.1.2.2
// This identifies the krb5 GSS-API mechanism per RFC 4121.
var KRB5OID = []int{1, 2, 840, 113554, 1, 2, 2}

// Pseudo-flavor constants for NFS Kerberos authentication.
// These are used in SECINFO responses to advertise supported security flavors.
// Values defined by IANA and used by Linux NFS implementation.
const (
	// PseudoFlavorKrb5 is RPCSEC_GSS with krb5 mechanism, service=none.
	PseudoFlavorKrb5 uint32 = 390003

	// PseudoFlavorKrb5i is RPCSEC_GSS with krb5 mechanism, service=integrity.
	PseudoFlavorKrb5i uint32 = 390004

	// PseudoFlavorKrb5p is RPCSEC_GSS with krb5 mechanism, service=privacy.
	PseudoFlavorKrb5p uint32 = 390005
)

// ============================================================================
// RFC 4121 Key Usage Constants
// ============================================================================

// Key usage values for RFC 4121 krb5 GSS-API mechanism.
// Per RFC 4121 Section 2:
//   - KG-USAGE-ACCEPTOR-SEAL  = 22
//   - KG-USAGE-ACCEPTOR-SIGN  = 23
//   - KG-USAGE-INITIATOR-SEAL = 24
//   - KG-USAGE-INITIATOR-SIGN = 25
//
// These are used with krb5 encryption/decryption for MIC and Wrap tokens.
const (
	// KeyUsageAcceptorSeal is the key usage for acceptor (server) Wrap tokens.
	KeyUsageAcceptorSeal uint32 = 22

	// KeyUsageAcceptorSign is the key usage for acceptor (server) MIC tokens.
	KeyUsageAcceptorSign uint32 = 23

	// KeyUsageInitiatorSeal is the key usage for initiator (client) Wrap tokens.
	KeyUsageInitiatorSeal uint32 = 24

	// KeyUsageInitiatorSign is the key usage for initiator (client) MIC tokens.
	KeyUsageInitiatorSign uint32 = 25
)

// ============================================================================
// RPCSEC_GSS Credential (rpc_gss_cred_t)
// ============================================================================

// RPCGSSCredV1 represents the RPCSEC_GSS credential body (version 1).
//
// This is carried in the OpaqueAuth.Body field of RPC call messages
// when the auth flavor is RPCSEC_GSS (6).
//
// Wire format (XDR, after version field):
//
//	gss_proc:  uint32 (DATA=0, INIT=1, CONTINUE_INIT=2, DESTROY=3)
//	seq_num:   uint32 (monotonically increasing per context)
//	service:   uint32 (NONE=1, INTEGRITY=2, PRIVACY=3)
//	handle:    opaque<> (context handle, empty during INIT)
//
// Reference: RFC 2203 Section 5.3.1
type RPCGSSCredV1 struct {
	// GSSProc indicates what kind of RPCSEC_GSS call this is.
	GSSProc uint32

	// SeqNum is the sequence number for this call within the GSS context.
	// Must be monotonically increasing and within the sequence window.
	SeqNum uint32

	// Service indicates the level of protection for the call body.
	Service uint32

	// Handle is the GSS context handle.
	// Empty (zero-length) during INIT; server-assigned after context setup.
	Handle []byte
}

// DecodeGSSCred decodes an RPCSEC_GSS credential from the opaque auth body.
//
// The body must start with the version field (uint32), which must be 1.
// After the version, the credential fields are decoded in order:
// gss_proc, seq_num, service, handle (as variable-length opaque).
//
// Parameters:
//   - body: Raw XDR-encoded credential body from OpaqueAuth.Body
//
// Returns:
//   - *RPCGSSCredV1: Decoded credential
//   - error: If version is not 1 or if XDR decoding fails
func DecodeGSSCred(body []byte) (*RPCGSSCredV1, error) {
	if len(body) < 20 { // minimum: version(4) + gss_proc(4) + seq_num(4) + service(4) + handle_len(4)
		return nil, fmt.Errorf("gss credential body too short: %d bytes", len(body))
	}

	reader := bytes.NewReader(body)

	// Read version (must be 1)
	var version uint32
	if err := binary.Read(reader, binary.BigEndian, &version); err != nil {
		return nil, fmt.Errorf("read gss version: %w", err)
	}
	if version != RPCGSSVers1 {
		return nil, fmt.Errorf("unsupported RPCSEC_GSS version: %d (expected %d)", version, RPCGSSVers1)
	}

	cred := &RPCGSSCredV1{}

	// Read gss_proc
	if err := binary.Read(reader, binary.BigEndian, &cred.GSSProc); err != nil {
		return nil, fmt.Errorf("read gss_proc: %w", err)
	}

	// Read seq_num
	if err := binary.Read(reader, binary.BigEndian, &cred.SeqNum); err != nil {
		return nil, fmt.Errorf("read seq_num: %w", err)
	}

	// Read service
	if err := binary.Read(reader, binary.BigEndian, &cred.Service); err != nil {
		return nil, fmt.Errorf("read service: %w", err)
	}

	// Read handle as variable-length opaque (length-prefixed)
	var handleLen uint32
	if err := binary.Read(reader, binary.BigEndian, &handleLen); err != nil {
		return nil, fmt.Errorf("read handle length: %w", err)
	}

	// Validate handle length
	const maxHandleLen = 65536
	if handleLen > maxHandleLen {
		return nil, fmt.Errorf("handle length %d exceeds maximum %d", handleLen, maxHandleLen)
	}

	if handleLen > 0 {
		cred.Handle = make([]byte, handleLen)
		if _, err := reader.Read(cred.Handle); err != nil {
			return nil, fmt.Errorf("read handle data: %w", err)
		}
		// Skip XDR padding
		padding := (4 - (handleLen % 4)) % 4
		for range int(padding) {
			if _, err := reader.ReadByte(); err != nil {
				return nil, fmt.Errorf("skip handle padding: %w", err)
			}
		}
	}

	return cred, nil
}

// EncodeGSSCred encodes an RPCSEC_GSS credential to XDR format.
//
// The encoded format includes the version field (always 1) followed by
// gss_proc, seq_num, service, and handle (as variable-length opaque).
//
// Parameters:
//   - cred: Credential to encode
//
// Returns:
//   - []byte: XDR-encoded credential body
//   - error: Encoding error
func EncodeGSSCred(cred *RPCGSSCredV1) ([]byte, error) {
	buf := &bytes.Buffer{}

	// Write version
	if err := binary.Write(buf, binary.BigEndian, RPCGSSVers1); err != nil {
		return nil, fmt.Errorf("write version: %w", err)
	}

	// Write gss_proc
	if err := binary.Write(buf, binary.BigEndian, cred.GSSProc); err != nil {
		return nil, fmt.Errorf("write gss_proc: %w", err)
	}

	// Write seq_num
	if err := binary.Write(buf, binary.BigEndian, cred.SeqNum); err != nil {
		return nil, fmt.Errorf("write seq_num: %w", err)
	}

	// Write service
	if err := binary.Write(buf, binary.BigEndian, cred.Service); err != nil {
		return nil, fmt.Errorf("write service: %w", err)
	}

	// Write handle as variable-length opaque
	handleLen := uint32(len(cred.Handle))
	if err := binary.Write(buf, binary.BigEndian, handleLen); err != nil {
		return nil, fmt.Errorf("write handle length: %w", err)
	}

	if handleLen > 0 {
		if _, err := buf.Write(cred.Handle); err != nil {
			return nil, fmt.Errorf("write handle data: %w", err)
		}
		// Write XDR padding
		padding := (4 - (handleLen % 4)) % 4
		for range int(padding) {
			if err := buf.WriteByte(0); err != nil {
				return nil, fmt.Errorf("write handle padding: %w", err)
			}
		}
	}

	return buf.Bytes(), nil
}

// ============================================================================
// RPCSEC_GSS Init Response (rpc_gss_init_res)
// ============================================================================

// RPCGSSInitRes represents the RPCSEC_GSS context establishment response.
//
// This is sent by the server in reply to INIT and CONTINUE_INIT calls.
// It contains the server's GSS context handle and the output token
// that the client should process.
//
// Wire format (XDR):
//
//	handle:     opaque<> (server-assigned context handle)
//	gss_major:  uint32 (GSS major status code)
//	gss_minor:  uint32 (GSS minor status code, mechanism-specific)
//	seq_window: uint32 (size of the sequence number window)
//	gss_token:  opaque<> (output token for the client)
//
// Reference: RFC 2203 Section 5.2.3.1
type RPCGSSInitRes struct {
	// Handle is the server-assigned context handle.
	// The client must include this in subsequent RPCSEC_GSS credentials.
	Handle []byte

	// GSSMajor is the GSS-API major status code.
	// GSSComplete (0) means context is established.
	// GSSContinueNeeded (1) means more round trips are needed.
	GSSMajor uint32

	// GSSMinor is the GSS-API minor status code.
	// Mechanism-specific (e.g., Kerberos error codes).
	GSSMinor uint32

	// SeqWindow is the size of the sequence number window.
	// The client must not reuse sequence numbers within this window.
	SeqWindow uint32

	// GSSToken is the output token to send back to the client.
	// For Kerberos, this is the AP-REP or continued negotiation token.
	GSSToken []byte
}

// EncodeGSSInitRes encodes an RPCSEC_GSS init response to XDR format.
//
// This is used when building the server's reply to INIT/CONTINUE_INIT calls.
//
// Parameters:
//   - res: Init response to encode
//
// Returns:
//   - []byte: XDR-encoded init response
//   - error: Encoding error
func EncodeGSSInitRes(res *RPCGSSInitRes) ([]byte, error) {
	buf := &bytes.Buffer{}

	// Write handle as variable-length opaque
	if err := writeOpaque(buf, res.Handle); err != nil {
		return nil, fmt.Errorf("write handle: %w", err)
	}

	// Write gss_major
	if err := binary.Write(buf, binary.BigEndian, res.GSSMajor); err != nil {
		return nil, fmt.Errorf("write gss_major: %w", err)
	}

	// Write gss_minor
	if err := binary.Write(buf, binary.BigEndian, res.GSSMinor); err != nil {
		return nil, fmt.Errorf("write gss_minor: %w", err)
	}

	// Write seq_window
	if err := binary.Write(buf, binary.BigEndian, res.SeqWindow); err != nil {
		return nil, fmt.Errorf("write seq_window: %w", err)
	}

	// Write gss_token as variable-length opaque
	if err := writeOpaque(buf, res.GSSToken); err != nil {
		return nil, fmt.Errorf("write gss_token: %w", err)
	}

	return buf.Bytes(), nil
}

// writeOpaque writes a variable-length opaque value in XDR format.
func writeOpaque(buf *bytes.Buffer, data []byte) error {
	length := uint32(len(data))
	if err := binary.Write(buf, binary.BigEndian, length); err != nil {
		return err
	}

	if length > 0 {
		if _, err := buf.Write(data); err != nil {
			return err
		}
		// Write XDR padding
		padding := (4 - (length % 4)) % 4
		for range int(padding) {
			if err := buf.WriteByte(0); err != nil {
				return err
			}
		}
	}

	return nil
}

// Package gss provides RPCSEC_GSS krb5i (integrity) wrapping and unwrapping.
//
// Per RFC 2203 Section 5.3.3.4.2, when the security service is rpc_gss_svc_integrity,
// the call body is replaced with rpc_gss_integ_data:
//
//	struct rpc_gss_integ_data {
//	    opaque  databody_integ<>;  // XDR(seq_num + args)
//	    opaque  checksum<>;        // MIC over databody_integ
//	};
//
// The MIC is computed using RFC 4121 GSS-API MICToken.
// For client->server: KeyUsageInitiatorSign (23)
// For server->client: KeyUsageAcceptorSign (25)
package gss

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/jcmturner/gokrb5/v8/gssapi"
	"github.com/jcmturner/gokrb5/v8/types"
)

// UnwrapIntegrity decodes and verifies an rpc_gss_integ_data request body.
//
// Per RFC 2203 Section 5.3.3.4.2:
// 1. Decode rpc_gss_integ_data: databody_integ (XDR opaque) + checksum (XDR opaque)
// 2. Verify MIC over databody_integ using the session key with KeyUsageInitiatorSign (23)
// 3. Extract seq_num from the first 4 bytes of databody_integ
// 4. Validate seq_num matches the credential's seq_num (dual validation)
// 5. Return remaining bytes as procedure arguments
//
// Parameters:
//   - sessionKey: The session key from the GSS context
//   - credSeqNum: The sequence number from the RPCSEC_GSS credential (for dual validation)
//   - requestBody: The raw rpc_gss_integ_data bytes
//
// Returns:
//   - procedureArgs: The unwrapped procedure arguments
//   - seqNum: The sequence number extracted from the body
//   - error: If MIC verification fails or seq_num mismatch
func UnwrapIntegrity(sessionKey types.EncryptionKey, credSeqNum uint32, requestBody []byte) ([]byte, uint32, error) {
	reader := bytes.NewReader(requestBody)

	// 1. Decode databody_integ (XDR opaque: length + data + padding)
	databodyInteg, err := readXDROpaque(reader)
	if err != nil {
		return nil, 0, fmt.Errorf("decode databody_integ: %w", err)
	}

	// 2. Decode checksum (XDR opaque: the MIC token bytes)
	checksumBytes, err := readXDROpaque(reader)
	if err != nil {
		return nil, 0, fmt.Errorf("decode checksum: %w", err)
	}

	// 3. Verify MIC: unmarshal the checksum as a MIC token, set payload = databody_integ
	var micToken gssapi.MICToken
	if err := micToken.Unmarshal(checksumBytes, false /* from initiator, not acceptor */); err != nil {
		return nil, 0, fmt.Errorf("unmarshal MIC token: %w", err)
	}

	// Set the payload for verification
	micToken.Payload = databodyInteg

	// Verify the checksum using initiator sign key usage (23)
	ok, err := micToken.Verify(sessionKey, KeyUsageInitiatorSign)
	if err != nil {
		return nil, 0, fmt.Errorf("verify MIC: %w", err)
	}
	if !ok {
		return nil, 0, fmt.Errorf("MIC verification failed")
	}

	// 4. Extract seq_num from databody_integ (first 4 bytes, big-endian uint32)
	if len(databodyInteg) < 4 {
		return nil, 0, fmt.Errorf("databody_integ too short for seq_num: %d bytes", len(databodyInteg))
	}
	bodySeqNum := binary.BigEndian.Uint32(databodyInteg[0:4])

	// 5. Dual validation: body seq_num must match credential seq_num
	if bodySeqNum != credSeqNum {
		return nil, 0, fmt.Errorf("seq_num mismatch: credential=%d, body=%d", credSeqNum, bodySeqNum)
	}

	// 6. Remaining bytes are the procedure arguments
	procedureArgs := databodyInteg[4:]

	return procedureArgs, bodySeqNum, nil
}

// WrapIntegrity wraps reply data as rpc_gss_integ_data for the client.
//
// Per RFC 2203 Section 5.3.3.4.2:
// 1. Build databody_integ: XDR(seq_num) + replyBody
// 2. Compute MIC over databody_integ using KeyUsageAcceptorSign (25)
// 3. Encode rpc_gss_integ_data: XDR opaque(databody_integ) + XDR opaque(mic_token_bytes)
//
// Parameters:
//   - sessionKey: The session key from the GSS context
//   - seqNum: The sequence number for this reply
//   - replyBody: The XDR-encoded procedure results
//
// Returns:
//   - []byte: The encoded rpc_gss_integ_data
//   - error: If MIC computation fails
func WrapIntegrity(sessionKey types.EncryptionKey, seqNum uint32, replyBody []byte) ([]byte, error) {
	// 1. Build databody_integ: XDR(seq_num) + replyBody
	databodyInteg := make([]byte, 4+len(replyBody))
	binary.BigEndian.PutUint32(databodyInteg[0:4], seqNum)
	copy(databodyInteg[4:], replyBody)

	// 2. Compute MIC over databody_integ using acceptor sign key usage (25)
	micToken := gssapi.MICToken{
		Flags:     gssapi.MICTokenFlagSentByAcceptor,
		SndSeqNum: uint64(seqNum),
		Payload:   databodyInteg,
	}

	if err := micToken.SetChecksum(sessionKey, KeyUsageAcceptorSign); err != nil {
		return nil, fmt.Errorf("compute integrity MIC: %w", err)
	}

	micBytes, err := micToken.Marshal()
	if err != nil {
		return nil, fmt.Errorf("marshal integrity MIC: %w", err)
	}

	// 3. Encode rpc_gss_integ_data: XDR opaque(databody_integ) + XDR opaque(mic_bytes)
	var buf bytes.Buffer
	if err := writeOpaque(&buf, databodyInteg); err != nil {
		return nil, fmt.Errorf("encode databody_integ: %w", err)
	}
	if err := writeOpaque(&buf, micBytes); err != nil {
		return nil, fmt.Errorf("encode checksum: %w", err)
	}

	return buf.Bytes(), nil
}

// readXDROpaque reads a variable-length XDR opaque value (length-prefixed with padding).
func readXDROpaque(reader *bytes.Reader) ([]byte, error) {
	var length uint32
	if err := binary.Read(reader, binary.BigEndian, &length); err != nil {
		return nil, fmt.Errorf("read opaque length: %w", err)
	}

	const maxOpaqueLen = 1 << 20 // 1MB safety limit
	if length > maxOpaqueLen {
		return nil, fmt.Errorf("opaque length %d exceeds maximum %d", length, maxOpaqueLen)
	}

	data := make([]byte, length)
	if _, err := reader.Read(data); err != nil {
		return nil, fmt.Errorf("read opaque data: %w", err)
	}

	// Skip XDR padding (align to 4 bytes)
	padding := (4 - (length % 4)) % 4
	for range int(padding) {
		if _, err := reader.ReadByte(); err != nil {
			return nil, fmt.Errorf("skip opaque padding: %w", err)
		}
	}

	return data, nil
}

// Package gss provides RPCSEC_GSS krb5p (privacy) wrapping and unwrapping.
//
// Per RFC 2203 Section 5.3.3.4.3, when the security service is rpc_gss_svc_privacy,
// the call body is replaced with rpc_gss_priv_data:
//
//	struct rpc_gss_priv_data {
//	    opaque  databody_priv<>;  // GSS Wrap token (encrypted + integrity-protected)
//	};
//
// The Wrap token is a GSS-API WrapToken per RFC 4121 Section 4.2.6.2.
// For krb5p, the token provides both confidentiality and integrity.
// For client->server: KeyUsageInitiatorSeal (24)
// For server->client: KeyUsageAcceptorSeal (26)
//
// RFC 4121 Section 4.2.4 defines the encrypted Wrap token format:
//   - Wire format: header (16 bytes) | encrypt(plaintext | filler | header_copy)
//   - The header_copy inside encryption has RRC=0 for checksum calculation
//   - RRC (Right Rotation Count) may rotate the encrypted portion
//
// The gokrb5 WrapToken does NOT implement decryption for the Sealed flag.
// This implementation manually handles RFC 4121 encrypted Wrap tokens.
package gss

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"

	"github.com/jcmturner/gokrb5/v8/crypto"
	"github.com/jcmturner/gokrb5/v8/gssapi"
	"github.com/jcmturner/gokrb5/v8/types"
	"github.com/marmos91/dittofs/internal/logger"
)

// Wrap token constants per RFC 4121 Section 4.2.6.2
const (
	wrapTokenHdrLen = 16 // Wrap token header length

	// Wrap token flags (same bit positions as MIC token)
	wrapFlagSentByAcceptor = 0x01 // Bit 0: sender is context acceptor
	wrapFlagSealed         = 0x02 // Bit 1: confidentiality provided (encrypted)
	wrapFlagAcceptorSubkey = 0x04 // Bit 2: acceptor subkey used
)

// UnwrapPrivacy decodes and decrypts an rpc_gss_priv_data request body.
//
// Per RFC 2203 Section 5.3.3.4.3:
// 1. Decode rpc_gss_priv_data: databody_priv (XDR opaque -- Wrap token)
// 2. Parse Wrap token header (16 bytes plaintext)
// 3. If Sealed flag is set, decrypt the ciphertext per RFC 4121 Section 4.2.4
// 4. Extract seq_num (first 4 bytes of plaintext)
// 5. Validate seq_num matches credential (dual validation)
// 6. Return remaining bytes as procedure arguments
//
// Parameters:
//   - sessionKey: The session key from the GSS context
//   - credSeqNum: The sequence number from the RPCSEC_GSS credential (for dual validation)
//   - requestBody: The raw rpc_gss_priv_data bytes
//
// Returns:
//   - procedureArgs: The unwrapped procedure arguments
//   - seqNum: The sequence number extracted from the payload
//   - error: If decryption/verification fails or seq_num mismatch
func UnwrapPrivacy(sessionKey types.EncryptionKey, credSeqNum uint32, requestBody []byte) ([]byte, uint32, error) {
	reader := bytes.NewReader(requestBody)

	// 1. Decode databody_priv (XDR opaque: the Wrap token bytes)
	wrapTokenBytes, err := readXDROpaque(reader)
	if err != nil {
		return nil, 0, fmt.Errorf("decode databody_priv: %w", err)
	}

	// 2. Validate minimum length and token ID
	if len(wrapTokenBytes) < wrapTokenHdrLen {
		return nil, 0, fmt.Errorf("wrap token too short: %d bytes, need at least %d", len(wrapTokenBytes), wrapTokenHdrLen)
	}

	// Verify Wrap token ID (0x05 0x04)
	if wrapTokenBytes[0] != 0x05 || wrapTokenBytes[1] != 0x04 {
		return nil, 0, fmt.Errorf("invalid Wrap token ID: 0x%02x%02x, expected 0x0504", wrapTokenBytes[0], wrapTokenBytes[1])
	}

	// 3. Parse header fields
	flags := wrapTokenBytes[2]
	// byte 3 is filler (0xFF)
	ec := binary.BigEndian.Uint16(wrapTokenBytes[4:6])  // Extra count (checksum size for non-sealed, filler size for sealed)
	rrc := binary.BigEndian.Uint16(wrapTokenBytes[6:8]) // Right Rotation Count
	sndSeqNum := binary.BigEndian.Uint64(wrapTokenBytes[8:16])

	logger.Debug("Wrap token header parsed",
		"flags", fmt.Sprintf("0x%02x", flags),
		"sealed", flags&wrapFlagSealed != 0,
		"ec", ec,
		"rrc", rrc,
		"snd_seq_num", sndSeqNum,
		"total_len", len(wrapTokenBytes),
	)

	// Check sender: should be from initiator (client), not acceptor
	if flags&wrapFlagSentByAcceptor != 0 {
		return nil, 0, fmt.Errorf("unexpected acceptor flag set: expecting token from initiator")
	}

	// 4. Handle based on Sealed flag
	var plaintext []byte

	if flags&wrapFlagSealed != 0 {
		// SEALED: Encrypted Wrap token per RFC 4121 Section 4.2.4
		// Wire format: header (16 bytes) | ciphertext
		// Ciphertext = encrypt(plaintext | filler | header_copy)
		// After decryption, we get: plaintext | filler | header_copy (16 bytes)

		ciphertext := wrapTokenBytes[wrapTokenHdrLen:]

		// Handle RRC (Right Rotation Count) - rotate left to undo the right rotation
		// The token was rotated right by RRC bytes, so we rotate left to restore original order
		if rrc > 0 && len(ciphertext) > 0 {
			ciphertext = rotateLeft(ciphertext, int(rrc))
		}

		logger.Debug("Decrypting sealed Wrap token",
			"ciphertext_len", len(ciphertext),
			"rrc", rrc,
			"key_type", sessionKey.KeyType,
		)

		// Decrypt using initiator seal key usage (24)
		decrypted, err := crypto.DecryptMessage(ciphertext, sessionKey, KeyUsageInitiatorSeal)
		if err != nil {
			return nil, 0, fmt.Errorf("decrypt Wrap token: %w", err)
		}

		logger.Debug("Decrypted Wrap token payload",
			"decrypted_len", len(decrypted),
			"first_16", hex.EncodeToString(firstN(decrypted, 16)),
			"last_16", hex.EncodeToString(lastN(decrypted, 16)),
		)

		// Per RFC 4121 Section 4.2.4:
		// Decrypted content = plaintext | filler | header_copy (16 bytes)
		// The header_copy is the original 16-byte header with RRC=0
		// EC field contains the filler size for sealed tokens
		if len(decrypted) < wrapTokenHdrLen {
			return nil, 0, fmt.Errorf("decrypted data too short for header: %d bytes", len(decrypted))
		}

		// Extract header_copy (last 16 bytes of decrypted data)
		headerCopy := decrypted[len(decrypted)-wrapTokenHdrLen:]

		// Verify header_copy matches original header (except EC/RRC which should be 0 in the copy)
		// Per RFC 4121: "the RRC field in the to-be-encrypted header contains the hex value 00 00"
		expectedHeader := make([]byte, wrapTokenHdrLen)
		copy(expectedHeader, wrapTokenBytes[:wrapTokenHdrLen])
		// Set EC and RRC to 0 for comparison (they're 0 in the header_copy inside ciphertext)
		binary.BigEndian.PutUint16(expectedHeader[4:6], 0) // EC = 0
		binary.BigEndian.PutUint16(expectedHeader[6:8], 0) // RRC = 0

		if !bytes.Equal(headerCopy[:2], expectedHeader[:2]) { // Token ID
			return nil, 0, fmt.Errorf("header_copy token ID mismatch: got %s, expected %s",
				hex.EncodeToString(headerCopy[:2]), hex.EncodeToString(expectedHeader[:2]))
		}
		if headerCopy[2] != expectedHeader[2] { // Flags
			return nil, 0, fmt.Errorf("header_copy flags mismatch: got 0x%02x, expected 0x%02x",
				headerCopy[2], expectedHeader[2])
		}

		// Verify sequence number in header_copy matches
		copySeqNum := binary.BigEndian.Uint64(headerCopy[8:16])
		if copySeqNum != sndSeqNum {
			return nil, 0, fmt.Errorf("header_copy seq_num mismatch: got %d, expected %d", copySeqNum, sndSeqNum)
		}

		// Plaintext is everything before the header_copy
		// Note: filler (if any) is between plaintext and header_copy, but EC tells us filler size
		// For sealed tokens, EC = filler size. The plaintext ends at (len - headerLen - ec)
		fillerSize := int(ec)
		plaintextEnd := len(decrypted) - wrapTokenHdrLen - fillerSize
		if plaintextEnd < 0 {
			return nil, 0, fmt.Errorf("invalid EC value %d: would make plaintext negative", ec)
		}
		plaintext = decrypted[:plaintextEnd]

		logger.Debug("Extracted plaintext from sealed Wrap token",
			"plaintext_len", len(plaintext),
			"filler_size", fillerSize,
		)

	} else {
		// NOT SEALED: Integrity-only Wrap token (like krb5i but in Wrap format)
		// Wire format: header (16 bytes) | plaintext | checksum
		// Use gokrb5's WrapToken for non-sealed tokens (it handles this case correctly)

		var wrapToken gssapi.WrapToken
		if err := wrapToken.Unmarshal(wrapTokenBytes, false /* from initiator */); err != nil {
			return nil, 0, fmt.Errorf("unmarshal non-sealed Wrap token: %w", err)
		}

		// Verify integrity using initiator seal key usage (24)
		ok, err := wrapToken.Verify(sessionKey, KeyUsageInitiatorSeal)
		if err != nil {
			return nil, 0, fmt.Errorf("verify non-sealed Wrap token: %w", err)
		}
		if !ok {
			return nil, 0, fmt.Errorf("non-sealed Wrap token verification failed")
		}

		plaintext = wrapToken.Payload
	}

	// 5. Extract seq_num from plaintext (first 4 bytes per RFC 2203)
	// The plaintext is: XDR(seq_num) | procedure_args
	if len(plaintext) < 4 {
		return nil, 0, fmt.Errorf("plaintext too short for seq_num: %d bytes", len(plaintext))
	}

	bodySeqNum := binary.BigEndian.Uint32(plaintext[0:4])

	// 6. Dual validation: body seq_num must match credential seq_num
	if bodySeqNum != credSeqNum {
		return nil, 0, fmt.Errorf("seq_num mismatch: credential=%d, body=%d", credSeqNum, bodySeqNum)
	}

	// 7. Remaining bytes are the procedure arguments
	procedureArgs := plaintext[4:]

	logger.Debug("UnwrapPrivacy complete",
		"procedure_args_len", len(procedureArgs),
		"seq_num", bodySeqNum,
	)

	return procedureArgs, bodySeqNum, nil
}

// rotateLeft rotates a byte slice left by n positions.
// This is used to undo the RRC (Right Rotation Count) applied by the sender.
func rotateLeft(data []byte, n int) []byte {
	if len(data) == 0 || n <= 0 {
		return data
	}
	n = n % len(data)
	if n == 0 {
		return data
	}
	result := make([]byte, len(data))
	copy(result, data[n:])
	copy(result[len(data)-n:], data[:n])
	return result
}

// WrapPrivacy wraps reply data as rpc_gss_priv_data for the client.
//
// Per RFC 2203 Section 5.3.3.4.3 and RFC 4121 Section 4.2.4:
// 1. Build plaintext: XDR(seq_num) + replyBody
// 2. Build to-be-encrypted: plaintext | filler | header_copy (with EC=0, RRC=0)
// 3. Encrypt using acceptor seal key usage (26)
// 4. Build wire format: header (16 bytes) | ciphertext
// 5. Encode as rpc_gss_priv_data: XDR opaque(wrap_token_bytes)
//
// Parameters:
//   - sessionKey: The session key from the GSS context
//   - seqNum: The sequence number for this reply
//   - replyBody: The XDR-encoded procedure results
//
// Returns:
//   - []byte: The encoded rpc_gss_priv_data
//   - error: If encryption/wrapping fails
func WrapPrivacy(sessionKey types.EncryptionKey, seqNum uint32, replyBody []byte) ([]byte, error) {
	// 1. Build plaintext: XDR(seq_num) + replyBody
	plaintext := make([]byte, 4+len(replyBody))
	binary.BigEndian.PutUint32(plaintext[0:4], seqNum)
	copy(plaintext[4:], replyBody)

	// 2. Get encryption type info
	encType, err := crypto.GetEtype(sessionKey.KeyType)
	if err != nil {
		return nil, fmt.Errorf("get encryption type: %w", err)
	}

	// 3. Build the header (to be sent in plaintext)
	// Flags: SentByAcceptor (0x01) | Sealed (0x02) = 0x03
	flags := byte(wrapFlagSentByAcceptor | wrapFlagSealed)

	// For sealed tokens, EC = filler size (we use 0 for simplicity)
	// The encryption provides integrity, so we don't need padding for checksum
	ec := uint16(0)
	rrc := uint16(0)

	header := make([]byte, wrapTokenHdrLen)
	header[0] = 0x05 // Token ID high byte
	header[1] = 0x04 // Token ID low byte
	header[2] = flags
	header[3] = 0xFF // Filler byte
	binary.BigEndian.PutUint16(header[4:6], ec)
	binary.BigEndian.PutUint16(header[6:8], rrc)
	binary.BigEndian.PutUint64(header[8:16], uint64(seqNum))

	// 4. Build header_copy for encryption (with EC=0, RRC=0)
	// Per RFC 4121: "the RRC field in the to-be-encrypted header contains the hex value 00 00"
	headerCopy := make([]byte, wrapTokenHdrLen)
	copy(headerCopy, header)
	binary.BigEndian.PutUint16(headerCopy[4:6], 0) // EC = 0 in copy
	binary.BigEndian.PutUint16(headerCopy[6:8], 0) // RRC = 0 in copy

	// 5. Build to-be-encrypted: plaintext | filler | header_copy
	// We use no filler (EC=0) for simplicity
	toEncrypt := make([]byte, len(plaintext)+wrapTokenHdrLen)
	copy(toEncrypt, plaintext)
	copy(toEncrypt[len(plaintext):], headerCopy)

	// 6. Encrypt using acceptor seal key usage (26)
	_, ciphertext, err := encType.EncryptMessage(sessionKey.KeyValue, toEncrypt, KeyUsageAcceptorSeal)
	if err != nil {
		return nil, fmt.Errorf("encrypt Wrap token: %w", err)
	}

	logger.Debug("WrapPrivacy encrypted",
		"plaintext_len", len(plaintext),
		"ciphertext_len", len(ciphertext),
		"seq_num", seqNum,
	)

	// 7. Build wire format: header | ciphertext
	wrapTokenBytes := make([]byte, wrapTokenHdrLen+len(ciphertext))
	copy(wrapTokenBytes, header)
	copy(wrapTokenBytes[wrapTokenHdrLen:], ciphertext)

	// 8. Encode as rpc_gss_priv_data: XDR opaque(wrap_token_bytes)
	var buf bytes.Buffer
	if err := writeOpaque(&buf, wrapTokenBytes); err != nil {
		return nil, fmt.Errorf("encode databody_priv: %w", err)
	}

	return buf.Bytes(), nil
}

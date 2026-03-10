package signing

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
)

// GMACSigner implements the Signer interface using AES-128-GMAC.
// This is used for SMB 3.1.1 sessions when GMAC is negotiated via
// SIGNING_CAPABILITIES negotiate context.
//
// GMAC = AES-GCM with empty plaintext, message as AAD.
// Nonce is derived from the MessageId field (bytes 24-31 of SMB2 header)
// plus server/cancel flag bits in byte 8.
type GMACSigner struct {
	key [KeySize]byte
	gcm cipher.AEAD
}

// NewGMACSigner creates a GMACSigner from a signing key.
// Returns nil if the key is empty or cipher initialization fails.
func NewGMACSigner(key []byte) *GMACSigner {
	if len(key) == 0 {
		return nil
	}

	s := &GMACSigner{key: copyKey(key)}
	block, err := aes.NewCipher(s.key[:])
	if err != nil {
		return nil
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil
	}
	s.gcm = gcm
	return s
}

// Sign computes the GMAC signature for an SMB2 message.
// The signature field (bytes 48-63) is zeroed before computation.
//
// Per [MS-SMB2] 3.1.4.1, the 12-byte nonce is constructed as:
//   - Bytes 0-7: MessageId (8 bytes at header offset 24)
//   - Byte 8: bit 0 = 1 if sender is server (FlagResponse set), 0 if client;
//     bit 1 = 1 if SMB2 CANCEL, 0 otherwise; bits 2-7 = 0
//   - Bytes 9-11: zero
func (s *GMACSigner) Sign(message []byte) [SignatureSize]byte {
	var sig [SignatureSize]byte
	if len(message) < SMB2HeaderSize {
		return sig
	}

	msgCopy := make([]byte, len(message))
	copy(msgCopy, message)
	zeroSignatureField(msgCopy)

	// Nonce: 8 bytes MessageId (offset 24) + 4 bytes flags
	var nonce [12]byte
	copy(nonce[:8], msgCopy[24:32]) // MessageId at header offset 24

	// Byte 8: bit 0 = server sender, bit 1 = cancel request
	flags := binary.LittleEndian.Uint32(msgCopy[16:20])
	if flags&0x00000001 != 0 { // SMB2_FLAGS_SERVER_TO_REDIR (response)
		nonce[8] |= 0x01
	}
	command := binary.LittleEndian.Uint16(msgCopy[12:14])
	if command == 0x000C { // SMB2 CANCEL
		nonce[8] |= 0x02
	}

	// GMAC = GCM with empty plaintext, message as AAD
	tag := s.gcm.Seal(nil, nonce[:], nil, msgCopy)
	copy(sig[:], tag[:SignatureSize])
	return sig
}

// Verify checks if the message signature is valid using constant-time comparison.
func (s *GMACSigner) Verify(message []byte) bool {
	return verifySig(s, message)
}

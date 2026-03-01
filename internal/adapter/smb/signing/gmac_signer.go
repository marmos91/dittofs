package signing

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/subtle"
)

// GMACSigner implements the Signer interface using AES-128-GMAC.
// This is used for SMB 3.1.1 sessions when GMAC is negotiated via
// SIGNING_CAPABILITIES negotiate context.
//
// GMAC = AES-GCM with empty plaintext, message as AAD.
// Nonce is derived from the MessageId field (bytes 28-35 of SMB2 header),
// zero-padded to 12 bytes.
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

	s := &GMACSigner{}
	if len(key) >= KeySize {
		copy(s.key[:], key[:KeySize])
	} else {
		copy(s.key[:], key)
	}

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
// Nonce = MessageId (8 bytes at offset 28) zero-padded to 12 bytes.
func (s *GMACSigner) Sign(message []byte) [SignatureSize]byte {
	var sig [SignatureSize]byte

	if len(message) < SMB2HeaderSize {
		return sig
	}

	// Create a copy with zeroed signature field
	msgCopy := make([]byte, len(message))
	copy(msgCopy, message)
	for i := SignatureOffset; i < SignatureOffset+SignatureSize; i++ {
		msgCopy[i] = 0
	}

	// Extract MessageId (8 bytes at offset 28) into 12-byte nonce (zero-padded)
	var nonce [12]byte
	copy(nonce[:8], msgCopy[28:36])

	// GMAC = GCM with empty plaintext, message as AAD
	// Seal(dst, nonce, plaintext, additionalData) -> tag is appended to dst
	tag := s.gcm.Seal(nil, nonce[:], nil, msgCopy)

	copy(sig[:], tag[:SignatureSize])
	return sig
}

// Verify checks if the message signature is valid using constant-time comparison.
func (s *GMACSigner) Verify(message []byte) bool {
	if len(message) < SMB2HeaderSize {
		return false
	}

	// Extract the provided signature
	var providedSig [SignatureSize]byte
	copy(providedSig[:], message[SignatureOffset:SignatureOffset+SignatureSize])

	// Compute expected signature
	expectedSig := s.Sign(message)

	// Constant-time comparison
	return subtle.ConstantTimeCompare(providedSig[:], expectedSig[:]) == 1
}

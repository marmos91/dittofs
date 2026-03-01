package signing

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
)

// HMACSigner implements the Signer interface using HMAC-SHA256.
// This is used for SMB 2.x sessions.
type HMACSigner struct {
	key [KeySize]byte
}

// NewHMACSigner creates an HMACSigner from a session key.
// The key is padded or truncated to 16 bytes (same logic as the old NewSigningKey).
// Returns nil if the key is empty or nil.
func NewHMACSigner(sessionKey []byte) *HMACSigner {
	if len(sessionKey) == 0 {
		return nil
	}
	s := &HMACSigner{}
	if len(sessionKey) >= KeySize {
		copy(s.key[:], sessionKey[:KeySize])
	} else {
		copy(s.key[:], sessionKey)
		// Remaining bytes are already zero from struct initialization
	}
	return s
}

// Sign computes the HMAC-SHA256 signature for an SMB2 message.
// The signature field (bytes 48-63) is zeroed before computation.
// Returns the first 16 bytes of the HMAC output.
func (s *HMACSigner) Sign(message []byte) [SignatureSize]byte {
	var signature [SignatureSize]byte

	if len(message) < SMB2HeaderSize {
		return signature
	}

	// Create a copy with zeroed signature field
	msgCopy := make([]byte, len(message))
	copy(msgCopy, message)
	for i := SignatureOffset; i < SignatureOffset+SignatureSize; i++ {
		msgCopy[i] = 0
	}

	// Compute HMAC-SHA256
	mac := hmac.New(sha256.New, s.key[:])
	mac.Write(msgCopy)
	sum := mac.Sum(nil)

	// Take first 16 bytes
	copy(signature[:], sum[:SignatureSize])
	return signature
}

// Verify checks if the message signature is valid using constant-time comparison.
func (s *HMACSigner) Verify(message []byte) bool {
	if len(message) < SMB2HeaderSize {
		return false
	}

	// Extract the provided signature
	var providedSig [SignatureSize]byte
	copy(providedSig[:], message[SignatureOffset:SignatureOffset+SignatureSize])

	// Compute expected signature
	expectedSig := s.Sign(message)

	// Constant-time comparison
	return hmac.Equal(providedSig[:], expectedSig[:])
}

// IsValid returns true if the signing key is non-zero.
func (s *HMACSigner) IsValid() bool {
	var zero [KeySize]byte
	return !bytes.Equal(s.key[:], zero[:])
}

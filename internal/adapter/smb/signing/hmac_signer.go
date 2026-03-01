package signing

import (
	"crypto/hmac"
	"crypto/sha256"
)

// HMACSigner implements the Signer interface using HMAC-SHA256.
// This is used for SMB 2.x sessions.
type HMACSigner struct {
	key [KeySize]byte
}

// NewHMACSigner creates an HMACSigner from a session key.
// The key is padded or truncated to 16 bytes.
// Returns nil if the key is empty or nil.
func NewHMACSigner(sessionKey []byte) *HMACSigner {
	if len(sessionKey) == 0 {
		return nil
	}
	return &HMACSigner{key: copyKey(sessionKey)}
}

// Sign computes the HMAC-SHA256 signature for an SMB2 message.
// The signature field (bytes 48-63) is zeroed before computation.
// Returns the first 16 bytes of the HMAC output.
func (s *HMACSigner) Sign(message []byte) [SignatureSize]byte {
	var signature [SignatureSize]byte
	if len(message) < SMB2HeaderSize {
		return signature
	}

	msgCopy := make([]byte, len(message))
	copy(msgCopy, message)
	zeroSignatureField(msgCopy)

	mac := hmac.New(sha256.New, s.key[:])
	mac.Write(msgCopy)
	copy(signature[:], mac.Sum(nil))
	return signature
}

// Verify checks if the message signature is valid using constant-time comparison.
func (s *HMACSigner) Verify(message []byte) bool {
	return verifySig(s, message)
}

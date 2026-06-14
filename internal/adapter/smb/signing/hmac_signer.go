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
//
// Sign copies the message so it never mutates the caller's buffer. The
// outbound SignMessage path, which has already zeroed the signature field,
// uses SignInPlace to avoid the copy.
func (s *HMACSigner) Sign(message []byte) [SignatureSize]byte {
	if len(message) < SMB2HeaderSize {
		return [SignatureSize]byte{}
	}

	msgCopy := make([]byte, len(message))
	copy(msgCopy, message)
	zeroSignatureField(msgCopy)
	return s.SignInPlace(msgCopy)
}

// SignInPlace computes the HMAC-SHA256 signature assuming the 16-byte
// signature field (bytes 48-63) is already zeroed. It does NOT copy and does
// NOT mutate the message. Used by the outbound SignMessage path, which zeroes
// the field itself before signing — avoiding a full-message allocation per PDU.
func (s *HMACSigner) SignInPlace(message []byte) [SignatureSize]byte {
	var signature [SignatureSize]byte
	if len(message) < SMB2HeaderSize {
		return signature
	}

	mac := hmac.New(sha256.New, s.key[:])
	mac.Write(message)
	// Sum appends the 32-byte digest to dst; a stack-backed dst keeps it
	// off the heap. We only retain the first 16 bytes (SMB2 signature size).
	var dst [sha256.Size]byte
	copy(signature[:], mac.Sum(dst[:0]))
	return signature
}

// Verify checks if the message signature is valid using constant-time comparison.
func (s *HMACSigner) Verify(message []byte) bool {
	return verifySig(s, message)
}

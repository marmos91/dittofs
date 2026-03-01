package signing

// HMACSigner implements the Signer interface using HMAC-SHA256.
// This is used for SMB 2.x sessions.
type HMACSigner struct {
	key [KeySize]byte
}

// NewHMACSigner creates an HMACSigner from a session key.
// The key is padded or truncated to 16 bytes.
// Returns nil if the key is empty.
func NewHMACSigner(sessionKey []byte) *HMACSigner {
	// TODO: implement
	return nil
}

// Sign computes the HMAC-SHA256 signature for an SMB2 message.
func (s *HMACSigner) Sign(message []byte) [SignatureSize]byte {
	var sig [SignatureSize]byte
	// TODO: implement
	return sig
}

// Verify checks if the message signature is valid.
func (s *HMACSigner) Verify(message []byte) bool {
	// TODO: implement
	return false
}

// IsValid returns true if the signing key is non-zero.
func (s *HMACSigner) IsValid() bool {
	// TODO: implement
	return false
}

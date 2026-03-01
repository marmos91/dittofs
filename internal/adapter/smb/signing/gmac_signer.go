package signing

// GMACSigner implements the Signer interface using AES-128-GMAC.
// This is used for SMB 3.1.1 sessions when GMAC is negotiated.
//
// GMAC = AES-GCM with empty plaintext, message as AAD.
// Nonce is derived from the MessageId field (bytes 28-35 of SMB2 header).
type GMACSigner struct {
	key [KeySize]byte
}

// NewGMACSigner creates a GMACSigner from a signing key.
// Returns nil if the key is empty.
func NewGMACSigner(key []byte) *GMACSigner {
	// TODO: implement
	return nil
}

// Sign computes the GMAC signature for an SMB2 message.
// The signature field (bytes 48-63) is zeroed before computation.
// Nonce = MessageId (8 bytes at offset 28) zero-padded to 12 bytes.
func (s *GMACSigner) Sign(message []byte) [SignatureSize]byte {
	var sig [SignatureSize]byte
	// TODO: implement
	return sig
}

// Verify checks if the message signature is valid.
func (s *GMACSigner) Verify(message []byte) bool {
	// TODO: implement
	return false
}

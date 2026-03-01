package signing

// CMACSigner implements the Signer interface using AES-128-CMAC per RFC 4493.
// This is used for SMB 3.x sessions.
type CMACSigner struct {
	key [KeySize]byte
	k1  [16]byte // Subkey K1
	k2  [16]byte // Subkey K2
}

// NewCMACSigner creates a CMACSigner from a signing key.
// Returns nil if the key is empty.
func NewCMACSigner(key []byte) *CMACSigner {
	// TODO: implement
	return nil
}

// cmacMAC computes the raw AES-CMAC over the given data.
// This is the pure RFC 4493 algorithm without SMB2 header handling.
func (s *CMACSigner) cmacMAC(data []byte) [16]byte {
	var mac [16]byte
	// TODO: implement
	return mac
}

// Sign computes the AES-CMAC signature for an SMB2 message.
// The signature field (bytes 48-63) is zeroed before computation.
func (s *CMACSigner) Sign(message []byte) [SignatureSize]byte {
	var sig [SignatureSize]byte
	// TODO: implement
	return sig
}

// Verify checks if the message signature is valid.
func (s *CMACSigner) Verify(message []byte) bool {
	// TODO: implement
	return false
}

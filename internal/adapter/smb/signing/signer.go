package signing

import (
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// Signing algorithm ID constants per [MS-SMB2] Section 2.2.3.1.7.
const (
	// SigningAlgHMACSHA256 is the HMAC-SHA256 signing algorithm (SMB 2.x default).
	SigningAlgHMACSHA256 uint16 = 0x0000
	// SigningAlgAESCMAC is the AES-128-CMAC signing algorithm (SMB 3.x default).
	SigningAlgAESCMAC uint16 = 0x0001
	// SigningAlgAESGMAC is the AES-128-GMAC signing algorithm (SMB 3.1.1 optional).
	SigningAlgAESGMAC uint16 = 0x0002
)

// Signer provides signing and verification for SMB2 messages.
// All implementations produce a 16-byte signature.
type Signer interface {
	// Sign computes the signature for an SMB2 message.
	// The signature field (bytes 48-63) is zeroed internally before computation.
	Sign(message []byte) [SignatureSize]byte

	// Verify checks if the message signature is valid.
	// Returns true if the signature is correct.
	Verify(message []byte) bool
}

// NewSigner creates the appropriate Signer for the negotiated dialect and
// signing algorithm.
//
// Dispatch logic:
//   - dialect < 3.0: HMACSigner (HMAC-SHA256)
//   - signingAlgorithmId == SigningAlgAESGMAC: GMACSigner
//   - otherwise (3.0/3.0.2, or 3.1.1 without GMAC): CMACSigner
func NewSigner(dialect types.Dialect, signingAlgorithmId uint16, key []byte) Signer {
	// TODO: implement
	return nil
}

// SignMessage signs an SMB2 message in place using the given Signer.
// It sets the SMB2_FLAGS_SIGNED flag and writes the computed signature.
func SignMessage(signer Signer, message []byte) {
	// TODO: implement
}

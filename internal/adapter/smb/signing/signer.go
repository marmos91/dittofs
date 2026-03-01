package signing

import (
	"encoding/binary"

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
	if dialect < types.Dialect0300 {
		return NewHMACSigner(key)
	}
	if signingAlgorithmId == SigningAlgAESGMAC {
		return NewGMACSigner(key)
	}
	return NewCMACSigner(key)
}

// SignMessage signs an SMB2 message in place using the given Signer.
// It sets the SMB2_FLAGS_SIGNED flag (bit 3 of flags at offset 16) and
// writes the computed signature to bytes 48-63.
//
// This replaces the old SigningKey.SignMessage method and decouples the
// protocol concern (flag setting, signature placement) from the crypto concern.
func SignMessage(signer Signer, message []byte) {
	if signer == nil || len(message) < SMB2HeaderSize {
		return
	}

	// Set the signed flag (SMB2_FLAGS_SIGNED = 0x00000008)
	flags := binary.LittleEndian.Uint32(message[16:20])
	flags |= 0x00000008
	binary.LittleEndian.PutUint32(message[16:20], flags)

	// Zero the signature field first
	for i := SignatureOffset; i < SignatureOffset+SignatureSize; i++ {
		message[i] = 0
	}

	// Compute and write signature
	sig := signer.Sign(message)
	copy(message[SignatureOffset:], sig[:])
}

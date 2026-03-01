// Package signing provides SMB2 message signing abstractions.
//
// SMB2 message signing ensures message integrity by computing a cryptographic
// signature over the entire message. This prevents tampering and man-in-the-middle
// attacks on SMB2 communications.
//
// # Signing Algorithms (MS-SMB2 3.1.4.1)
//
// Three signing algorithms are supported, dispatched by negotiated dialect:
//   - HMAC-SHA256 (SMB 2.x): truncated to 16 bytes
//   - AES-128-CMAC (SMB 3.0/3.0.2, and 3.1.1 default): per RFC 4493
//   - AES-128-GMAC (SMB 3.1.1 optional): GCM with empty plaintext
//
// # Signer Interface
//
// The Signer interface abstracts over all three algorithms. Use NewSigner()
// factory to create the appropriate implementation based on dialect and
// negotiated signing algorithm ID.
//
// # Session Key
//
// For SMB 2.0.2/2.1, the signing key is derived directly from the NTLM session key.
// For SMB 3.x, the signing key is derived via SP800-108 KDF (see kdf package).
//
// Reference: [MS-SMB2] 3.1.4.1 - Signing an Outgoing Message
package signing

import (
	"crypto/subtle"
)

const (
	// SignatureOffset is the position of the signature in the SMB2 header.
	SignatureOffset = 48

	// SignatureSize is the size of the signature field (16 bytes).
	SignatureSize = 16

	// KeySize is the required size of the signing key (16 bytes).
	KeySize = 16

	// SMB2HeaderSize is the fixed size of SMB2 header.
	SMB2HeaderSize = 64
)

// SigningConfig holds configuration for SMB2 signing.
type SigningConfig struct {
	// Enabled indicates signing capability is advertised.
	Enabled bool
	// Required indicates signing is mandatory.
	Required bool
}

// DefaultSigningConfig returns the default signing configuration.
// Signing is enabled but not required by default.
func DefaultSigningConfig() SigningConfig {
	return SigningConfig{
		Enabled:  true,
		Required: false,
	}
}

// copyKey copies up to KeySize bytes from src into a fixed-size key array.
// Short keys are zero-padded; long keys are truncated. This is the standard
// SMB2 key normalization used by all signer constructors.
func copyKey(src []byte) [KeySize]byte {
	var key [KeySize]byte
	copy(key[:], src)
	return key
}

// zeroSignatureField zeroes the 16-byte signature field in an SMB2 message copy.
// The caller must ensure msgCopy has at least SMB2HeaderSize bytes.
func zeroSignatureField(msgCopy []byte) {
	clear(msgCopy[SignatureOffset : SignatureOffset+SignatureSize])
}

// verifySig extracts the signature from a message, computes the expected
// signature using the given Signer, and compares them in constant time.
// This is the shared verification logic for all signer implementations.
func verifySig(signer Signer, message []byte) bool {
	if len(message) < SMB2HeaderSize {
		return false
	}

	var providedSig [SignatureSize]byte
	copy(providedSig[:], message[SignatureOffset:SignatureOffset+SignatureSize])

	expectedSig := signer.Sign(message)

	return subtle.ConstantTimeCompare(providedSig[:], expectedSig[:]) == 1
}

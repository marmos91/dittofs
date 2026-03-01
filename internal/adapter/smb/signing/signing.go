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
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
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

// SigningKey represents an SMB2 signing key (legacy type for backward compatibility).
// The key is always 16 bytes, derived from the session key.
//
// Deprecated: Use HMACSigner instead. This type is kept temporarily for
// existing test compatibility and will be removed in a future cleanup.
type SigningKey struct {
	key [KeySize]byte
}

// NewSigningKey creates a signing key from a session key.
// The session key is padded or truncated to 16 bytes as required.
//
// Returns nil if sessionKey is empty or nil.
func NewSigningKey(sessionKey []byte) *SigningKey {
	if len(sessionKey) == 0 {
		return nil
	}
	sk := &SigningKey{}
	if len(sessionKey) >= KeySize {
		copy(sk.key[:], sessionKey[:KeySize])
	} else {
		copy(sk.key[:], sessionKey)
	}
	return sk
}

// IsValid returns true if the signing key is non-zero.
func (sk *SigningKey) IsValid() bool {
	var zero [KeySize]byte
	return !bytes.Equal(sk.key[:], zero[:])
}

// Sign computes the HMAC-SHA256 signature for an SMB2 message.
func (sk *SigningKey) Sign(message []byte) [SignatureSize]byte {
	var signature [SignatureSize]byte
	if len(message) < SMB2HeaderSize {
		return signature
	}

	msgCopy := make([]byte, len(message))
	copy(msgCopy, message)
	for i := SignatureOffset; i < SignatureOffset+SignatureSize; i++ {
		msgCopy[i] = 0
	}

	mac := hmac.New(sha256.New, sk.key[:])
	mac.Write(msgCopy)
	sum := mac.Sum(nil)
	copy(signature[:], sum[:SignatureSize])
	return signature
}

// Verify checks if the message signature is valid.
func (sk *SigningKey) Verify(message []byte) bool {
	if len(message) < SMB2HeaderSize {
		return false
	}
	var providedSig [SignatureSize]byte
	copy(providedSig[:], message[SignatureOffset:SignatureOffset+SignatureSize])
	expectedSig := sk.Sign(message)
	return hmac.Equal(providedSig[:], expectedSig[:])
}

// SignMessage signs an SMB2 message in place (legacy method).
func (sk *SigningKey) SignMessage(message []byte) {
	if len(message) < SMB2HeaderSize {
		return
	}
	flags := uint32(message[16]) | uint32(message[17])<<8 | uint32(message[18])<<16 | uint32(message[19])<<24
	flags |= 0x00000008
	message[16] = byte(flags)
	message[17] = byte(flags >> 8)
	message[18] = byte(flags >> 16)
	message[19] = byte(flags >> 24)
	for i := SignatureOffset; i < SignatureOffset+SignatureSize; i++ {
		message[i] = 0
	}
	sig := sk.Sign(message)
	copy(message[SignatureOffset:], sig[:])
}

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

// Note: SessionSigningState has been replaced by session.SessionCryptoState.
// See internal/adapter/smb/session/crypto_state.go for the new abstraction.

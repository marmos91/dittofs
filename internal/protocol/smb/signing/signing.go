// Package signing provides SMB2 message signing using HMAC-SHA256.
//
// SMB2 message signing ensures message integrity by computing a cryptographic
// signature over the entire message. This prevents tampering and man-in-the-middle
// attacks on SMB2 communications.
//
// # Signing Algorithm (MS-SMB2 3.1.4.1)
//
// For SMB 2.0.2 and 2.1 (which DittoFS supports), HMAC-SHA256 is used:
//  1. The signature field (16 bytes at offset 48) is zeroed
//  2. HMAC-SHA256 is computed over the entire message (header + body)
//  3. The first 16 bytes of the HMAC output become the signature
//
// # Session Key
//
// For SMB 2.0.2/2.1, the signing key is derived directly from the NTLM session key:
//   - If session key < 16 bytes: pad with zeros
//   - If session key > 16 bytes: truncate to 16 bytes
//
// # When Signing is Required (MS-SMB2 3.2.5.1.3)
//
//   - Signing is negotiated during NEGOTIATE and SESSION_SETUP
//   - Messages with SessionID == 0 are never signed
//   - Once a session has signing enabled, all messages must be signed
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

// SigningKey represents an SMB2 signing key.
// The key is always 16 bytes, derived from the session key.
type SigningKey struct {
	key [KeySize]byte
}

// NewSigningKey creates a signing key from a session key.
// The session key is padded or truncated to 16 bytes as required.
func NewSigningKey(sessionKey []byte) *SigningKey {
	sk := &SigningKey{}
	if len(sessionKey) >= KeySize {
		copy(sk.key[:], sessionKey[:KeySize])
	} else {
		copy(sk.key[:], sessionKey)
		// Remaining bytes are already zero from struct initialization
	}
	return sk
}

// IsValid returns true if the signing key is non-zero.
func (sk *SigningKey) IsValid() bool {
	var zero [KeySize]byte
	return !bytes.Equal(sk.key[:], zero[:])
}

// Sign computes the HMAC-SHA256 signature for an SMB2 message.
//
// The message parameter should contain the complete SMB2 message (header + body).
// The signature field (bytes 48-64) should be zeroed before calling this function.
//
// Returns the 16-byte signature.
func (sk *SigningKey) Sign(message []byte) [SignatureSize]byte {
	var signature [SignatureSize]byte

	if len(message) < SMB2HeaderSize {
		return signature
	}

	// Create a copy with zeroed signature field
	msgCopy := make([]byte, len(message))
	copy(msgCopy, message)

	// Zero the signature field
	for i := SignatureOffset; i < SignatureOffset+SignatureSize; i++ {
		msgCopy[i] = 0
	}

	// Compute HMAC-SHA256
	mac := hmac.New(sha256.New, sk.key[:])
	mac.Write(msgCopy)
	sum := mac.Sum(nil)

	// Take first 16 bytes
	copy(signature[:], sum[:SignatureSize])

	return signature
}

// Verify checks if the message signature is valid.
//
// Returns true if the signature is valid, false otherwise.
func (sk *SigningKey) Verify(message []byte) bool {
	if len(message) < SMB2HeaderSize {
		return false
	}

	// Extract the provided signature
	var providedSig [SignatureSize]byte
	copy(providedSig[:], message[SignatureOffset:SignatureOffset+SignatureSize])

	// Compute expected signature
	expectedSig := sk.Sign(message)

	// Constant-time comparison
	return hmac.Equal(providedSig[:], expectedSig[:])
}

// SignMessage signs an SMB2 message in place.
// It sets the signed flag in the header and computes the signature.
//
// The message must be at least SMB2HeaderSize bytes.
// The signature is written to bytes 48-64 of the message.
func (sk *SigningKey) SignMessage(message []byte) {
	if len(message) < SMB2HeaderSize {
		return
	}

	// Set the signed flag (bit 3 of flags at offset 16)
	// Flags are a uint32 at offset 16, SMB2_FLAGS_SIGNED = 0x00000008
	flags := uint32(message[16]) | uint32(message[17])<<8 | uint32(message[18])<<16 | uint32(message[19])<<24
	flags |= 0x00000008 // SMB2_FLAGS_SIGNED
	message[16] = byte(flags)
	message[17] = byte(flags >> 8)
	message[18] = byte(flags >> 16)
	message[19] = byte(flags >> 24)

	// Zero the signature field first
	for i := SignatureOffset; i < SignatureOffset+SignatureSize; i++ {
		message[i] = 0
	}

	// Compute and write signature
	sig := sk.Sign(message)
	copy(message[SignatureOffset:], sig[:])
}

// SigningConfig holds configuration for SMB2 signing.
type SigningConfig struct {
	// Enabled indicates signing capability is advertised.
	// When true, the server indicates it supports signing.
	Enabled bool

	// Required indicates signing is mandatory.
	// When true, all sessions must use signing.
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

// SessionSigningState tracks signing state for a session.
type SessionSigningState struct {
	// SigningKey is the HMAC-SHA256 key for this session.
	SigningKey *SigningKey

	// SigningRequired indicates if signing is mandatory for this session.
	SigningRequired bool

	// SigningEnabled indicates if signing is active for this session.
	SigningEnabled bool
}

// NewSessionSigningState creates a new signing state for a session.
func NewSessionSigningState() *SessionSigningState {
	return &SessionSigningState{}
}

// SetSessionKey sets the signing key from the session key.
func (s *SessionSigningState) SetSessionKey(sessionKey []byte) {
	s.SigningKey = NewSigningKey(sessionKey)
}

// ShouldSign returns true if outgoing messages should be signed.
func (s *SessionSigningState) ShouldSign() bool {
	return s.SigningEnabled && s.SigningKey != nil && s.SigningKey.IsValid()
}

// ShouldVerify returns true if incoming messages should have signatures verified.
func (s *SessionSigningState) ShouldVerify() bool {
	return s.SigningEnabled && s.SigningKey != nil && s.SigningKey.IsValid()
}

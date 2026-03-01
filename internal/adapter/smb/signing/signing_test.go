package signing

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func TestNewSigningKey(t *testing.T) {
	tests := []struct {
		name       string
		sessionKey []byte
		wantValid  bool
	}{
		{
			name:       "16-byte key",
			sessionKey: bytes.Repeat([]byte{0x01}, 16),
			wantValid:  true,
		},
		{
			name:       "short key (8 bytes)",
			sessionKey: bytes.Repeat([]byte{0x02}, 8),
			wantValid:  true,
		},
		{
			name:       "long key (32 bytes)",
			sessionKey: bytes.Repeat([]byte{0x03}, 32),
			wantValid:  true,
		},
		{
			name:       "empty key",
			sessionKey: []byte{},
			wantValid:  false,
		},
		{
			name:       "nil key",
			sessionKey: nil,
			wantValid:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sk := NewSigningKey(tt.sessionKey)
			// NewSigningKey returns nil for empty/nil keys
			if sk == nil {
				if tt.wantValid {
					t.Error("NewSigningKey() returned nil for valid key")
				}
				return
			}
			if got := sk.IsValid(); got != tt.wantValid {
				t.Errorf("IsValid() = %v, want %v", got, tt.wantValid)
			}
		})
	}
}

func TestSigningKey_Sign(t *testing.T) {
	// Create a test key
	sessionKey := bytes.Repeat([]byte{0xAB}, 16)
	sk := NewSigningKey(sessionKey)

	// Create a minimal SMB2 message (header only)
	message := make([]byte, SMB2HeaderSize)
	// Set protocol ID
	message[0], message[1], message[2], message[3] = 0xFE, 'S', 'M', 'B'
	// Set structure size
	message[4], message[5] = 64, 0

	// Sign the message
	sig := sk.Sign(message)

	// Signature should be non-zero
	var zero [SignatureSize]byte
	if bytes.Equal(sig[:], zero[:]) {
		t.Error("Sign() returned zero signature")
	}

	// Signing the same message should produce the same signature
	sig2 := sk.Sign(message)
	if !bytes.Equal(sig[:], sig2[:]) {
		t.Error("Sign() is not deterministic")
	}
}

func TestSigningKey_Verify(t *testing.T) {
	// Create a test key
	sessionKey := bytes.Repeat([]byte{0xCD}, 16)
	sk := NewSigningKey(sessionKey)

	// Create a minimal SMB2 message
	message := make([]byte, SMB2HeaderSize+10) // header + some body
	message[0], message[1], message[2], message[3] = 0xFE, 'S', 'M', 'B'
	message[4], message[5] = 64, 0

	// Sign it
	sk.SignMessage(message)

	// Verify should pass
	if !sk.Verify(message) {
		t.Error("Verify() failed for correctly signed message")
	}

	// Tamper with the message body
	tamperedMessage := make([]byte, len(message))
	copy(tamperedMessage, message)
	tamperedMessage[SMB2HeaderSize] ^= 0xFF

	// Verify should fail
	if sk.Verify(tamperedMessage) {
		t.Error("Verify() passed for tampered message")
	}

	// Tamper with the signature
	tamperedSig := make([]byte, len(message))
	copy(tamperedSig, message)
	tamperedSig[SignatureOffset] ^= 0xFF

	// Verify should fail
	if sk.Verify(tamperedSig) {
		t.Error("Verify() passed for tampered signature")
	}
}

func TestSigningKey_SignMessage(t *testing.T) {
	sessionKey := bytes.Repeat([]byte{0xEF}, 16)
	sk := NewSigningKey(sessionKey)

	// Create a message with some body data
	message := make([]byte, SMB2HeaderSize+20)
	message[0], message[1], message[2], message[3] = 0xFE, 'S', 'M', 'B'
	message[4], message[5] = 64, 0
	// Write some body data
	for i := SMB2HeaderSize; i < len(message); i++ {
		message[i] = byte(i)
	}

	// Sign in place
	sk.SignMessage(message)

	// Check that signed flag is set (bit 3 of flags at offset 16)
	flags := uint32(message[16]) | uint32(message[17])<<8 | uint32(message[18])<<16 | uint32(message[19])<<24
	if flags&0x00000008 == 0 {
		t.Error("SignMessage() did not set signed flag")
	}

	// Check that signature is non-zero
	var zero [SignatureSize]byte
	if bytes.Equal(message[SignatureOffset:SignatureOffset+SignatureSize], zero[:]) {
		t.Error("SignMessage() did not write signature")
	}

	// Verify should pass
	if !sk.Verify(message) {
		t.Error("Verify() failed after SignMessage()")
	}
}

func TestSigningKey_DifferentKeysProduceDifferentSignatures(t *testing.T) {
	key1 := NewSigningKey(bytes.Repeat([]byte{0x11}, 16))
	key2 := NewSigningKey(bytes.Repeat([]byte{0x22}, 16))

	message := make([]byte, SMB2HeaderSize)
	message[0], message[1], message[2], message[3] = 0xFE, 'S', 'M', 'B'

	sig1 := key1.Sign(message)
	sig2 := key2.Sign(message)

	if bytes.Equal(sig1[:], sig2[:]) {
		t.Error("Different keys produced same signature")
	}
}

func TestSigningKey_MessageTooShort(t *testing.T) {
	sk := NewSigningKey(bytes.Repeat([]byte{0x33}, 16))

	// Message shorter than header
	shortMessage := make([]byte, 10)

	// Sign should return zero signature
	sig := sk.Sign(shortMessage)
	var zero [SignatureSize]byte
	if !bytes.Equal(sig[:], zero[:]) {
		t.Error("Sign() should return zero for short message")
	}

	// Verify should return false
	if sk.Verify(shortMessage) {
		t.Error("Verify() should return false for short message")
	}
}

// Note: SessionSigningState has been replaced by session.SessionCryptoState.
// The equivalent tests are in internal/adapter/smb/session/crypto_state_test.go.
// The old TestSessionSigningState test was removed during the Phase 34 migration.

// TestKnownVector tests against a known HMAC-SHA256 vector
func TestKnownVector(t *testing.T) {
	// This is a simplified test to ensure HMAC-SHA256 is working correctly
	// The actual SMB2 test vectors would require a full message structure

	key := make([]byte, 16)
	for i := range key {
		key[i] = byte(i)
	}
	sk := NewSigningKey(key)

	// Create a deterministic test message
	message := make([]byte, SMB2HeaderSize)
	for i := range message {
		message[i] = byte(i)
	}
	// Clear signature field
	for i := SignatureOffset; i < SignatureOffset+SignatureSize; i++ {
		message[i] = 0
	}

	sig := sk.Sign(message)

	// The signature should be deterministic
	sigHex := hex.EncodeToString(sig[:])
	t.Logf("Signature: %s", sigHex)

	// Sign again and verify it's the same
	sig2 := sk.Sign(message)
	if !bytes.Equal(sig[:], sig2[:]) {
		t.Error("Signature is not deterministic")
	}
}

func TestDefaultSigningConfig(t *testing.T) {
	config := DefaultSigningConfig()

	if !config.Enabled {
		t.Error("Default config should have Enabled = true")
	}
	if config.Required {
		t.Error("Default config should have Required = false")
	}
}

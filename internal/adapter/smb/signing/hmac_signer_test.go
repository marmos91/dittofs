package signing

import (
	"bytes"
	"testing"
)

// TestHMACSigner_MatchesOldSigningKey verifies that HMACSigner produces
// identical output to the old SigningKey for the same input.
func TestHMACSigner_MatchesOldSigningKey(t *testing.T) {
	sessionKey := bytes.Repeat([]byte{0xAB}, 16)

	// Old path
	oldKey := NewSigningKey(sessionKey)

	// New path
	newSigner := NewHMACSigner(sessionKey)

	// Create a test message
	message := make([]byte, SMB2HeaderSize+20)
	message[0], message[1], message[2], message[3] = 0xFE, 'S', 'M', 'B'
	for i := SMB2HeaderSize; i < len(message); i++ {
		message[i] = byte(i)
	}

	oldSig := oldKey.Sign(message)
	newSig := newSigner.Sign(message)

	if !bytes.Equal(oldSig[:], newSig[:]) {
		t.Errorf("HMACSigner output differs from SigningKey:\n  old: %x\n  new: %x", oldSig, newSig)
	}
}

// TestHMACSigner_Verify tests verification works correctly.
func TestHMACSigner_Verify(t *testing.T) {
	signer := NewHMACSigner(bytes.Repeat([]byte{0xCD}, 16))

	message := make([]byte, SMB2HeaderSize+10)
	message[0], message[1], message[2], message[3] = 0xFE, 'S', 'M', 'B'

	// Sign using SignMessage helper
	SignMessage(signer, message)

	// Verify should pass
	if !signer.Verify(message) {
		t.Error("Verify() failed for correctly signed message")
	}

	// Tamper
	tampered := make([]byte, len(message))
	copy(tampered, message)
	tampered[SMB2HeaderSize] ^= 0xFF
	if signer.Verify(tampered) {
		t.Error("Verify() passed for tampered message")
	}
}

// TestHMACSigner_EmptyKey tests that NewHMACSigner returns nil for empty key.
func TestHMACSigner_EmptyKey(t *testing.T) {
	if s := NewHMACSigner([]byte{}); s != nil {
		t.Error("NewHMACSigner should return nil for empty key")
	}
	if s := NewHMACSigner(nil); s != nil {
		t.Error("NewHMACSigner should return nil for nil key")
	}
}

// TestHMACSigner_ShortKey tests padding of short keys.
func TestHMACSigner_ShortKey(t *testing.T) {
	signer := NewHMACSigner([]byte{0x01, 0x02})
	if signer == nil {
		t.Fatal("NewHMACSigner returned nil for short key")
	}

	message := make([]byte, SMB2HeaderSize)
	message[0], message[1], message[2], message[3] = 0xFE, 'S', 'M', 'B'

	sig := signer.Sign(message)
	var zero [SignatureSize]byte
	if bytes.Equal(sig[:], zero[:]) {
		t.Error("Sign() returned zero signature for short key")
	}
}

// TestHMACSigner_LongKey tests truncation of long keys.
func TestHMACSigner_LongKey(t *testing.T) {
	signer := NewHMACSigner(bytes.Repeat([]byte{0xFF}, 32))
	if signer == nil {
		t.Fatal("NewHMACSigner returned nil for long key")
	}

	message := make([]byte, SMB2HeaderSize)
	message[0], message[1], message[2], message[3] = 0xFE, 'S', 'M', 'B'

	sig := signer.Sign(message)
	var zero [SignatureSize]byte
	if bytes.Equal(sig[:], zero[:]) {
		t.Error("Sign() returned zero signature for long key")
	}
}

// TestHMACSigner_IsValid tests the IsValid check.
func TestHMACSigner_IsValid(t *testing.T) {
	signer := NewHMACSigner(bytes.Repeat([]byte{0x01}, 16))
	if signer == nil || !signer.IsValid() {
		t.Error("HMACSigner with non-zero key should be valid")
	}
}

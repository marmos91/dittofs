package signing

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// TestGMACSigner_Sign tests basic GMAC signing with an SMB2-like message.
func TestGMACSigner_Sign(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 16)
	signer := NewGMACSigner(key)
	if signer == nil {
		t.Fatal("NewGMACSigner returned nil")
	}

	// Build an SMB2-like message with MessageId at offset 28
	message := make([]byte, SMB2HeaderSize+20)
	message[0], message[1], message[2], message[3] = 0xFE, 'S', 'M', 'B'
	binary.LittleEndian.PutUint64(message[28:36], 0x0000000000000001) // MessageId = 1

	sig := signer.Sign(message)

	// Signature should be non-zero
	var zero [SignatureSize]byte
	if bytes.Equal(sig[:], zero[:]) {
		t.Error("Sign() returned zero signature")
	}

	// Deterministic
	sig2 := signer.Sign(message)
	if !bytes.Equal(sig[:], sig2[:]) {
		t.Error("Sign() is not deterministic")
	}
}

// TestGMACSigner_Verify tests GMAC verification.
func TestGMACSigner_Verify(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 16)
	signer := NewGMACSigner(key)

	message := make([]byte, SMB2HeaderSize+20)
	message[0], message[1], message[2], message[3] = 0xFE, 'S', 'M', 'B'
	binary.LittleEndian.PutUint64(message[28:36], 42)

	// Sign using SignMessage helper
	SignMessage(signer, message)

	// Verify should pass
	if !signer.Verify(message) {
		t.Error("Verify() failed for correctly signed message")
	}

	// Tamper body
	tampered := make([]byte, len(message))
	copy(tampered, message)
	tampered[SMB2HeaderSize] ^= 0xFF
	if signer.Verify(tampered) {
		t.Error("Verify() passed for tampered message")
	}
}

// TestGMACSigner_NonceFromMessageId tests that the nonce is correctly
// extracted from MessageId (8 bytes at offset 28) and zero-padded to 12 bytes.
func TestGMACSigner_NonceFromMessageId(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 16)
	signer := NewGMACSigner(key)

	// Two messages with different MessageIds should produce different signatures
	msg1 := make([]byte, SMB2HeaderSize+20)
	msg2 := make([]byte, SMB2HeaderSize+20)
	msg1[0], msg1[1], msg1[2], msg1[3] = 0xFE, 'S', 'M', 'B'
	msg2[0], msg2[1], msg2[2], msg2[3] = 0xFE, 'S', 'M', 'B'

	binary.LittleEndian.PutUint64(msg1[28:36], 1)
	binary.LittleEndian.PutUint64(msg2[28:36], 2)

	sig1 := signer.Sign(msg1)
	sig2 := signer.Sign(msg2)

	if bytes.Equal(sig1[:], sig2[:]) {
		t.Error("Different MessageIds should produce different signatures")
	}
}

// TestGMACSigner_EmptyKey tests nil key handling.
func TestGMACSigner_EmptyKey(t *testing.T) {
	if s := NewGMACSigner(nil); s != nil {
		t.Error("NewGMACSigner should return nil for nil key")
	}
	if s := NewGMACSigner([]byte{}); s != nil {
		t.Error("NewGMACSigner should return nil for empty key")
	}
}

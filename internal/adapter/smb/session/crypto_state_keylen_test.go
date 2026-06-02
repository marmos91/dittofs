package session

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/signing"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// TestDeriveAllKeys_AES256UsesFullSessionKey verifies the cipher-aware KDF input
// length that fixes smb2.session.encryption-aes-256-{ccm,gcm} under Kerberos
// (#686). Matching Samba smb2_signing_key_create:
//
//   - AES-256 encryption/decryption keys derive from the FULL session key, so
//     they MUST change when bytes beyond the first 16 change (e.g. a 32-byte
//     Kerberos AES-256 ticket key).
//   - AES-128 encryption keys, signing keys, and application keys derive from
//     only the first 16 bytes, so they MUST NOT change when bytes beyond 16
//     change.
func TestDeriveAllKeys_AES256UsesFullSessionKey(t *testing.T) {
	var preauth [64]byte

	// Two 32-byte keys identical in the first 16 bytes, differing in the tail.
	keyA := make([]byte, 32)
	for i := range keyA {
		keyA[i] = byte(i + 1)
	}
	keyB := make([]byte, 32)
	copy(keyB, keyA)
	for i := 16; i < 32; i++ {
		keyB[i] ^= 0xFF // mutate only the tail (bytes 16..31)
	}

	csA := DeriveAllKeys(keyA, types.Dialect0311, preauth, types.CipherAES256GCM, signing.SigningAlgAESGMAC, true)
	csB := DeriveAllKeys(keyB, types.Dialect0311, preauth, types.CipherAES256GCM, signing.SigningAlgAESGMAC, true)

	if len(csA.EncryptionKey) != 32 || len(csA.DecryptionKey) != 32 {
		t.Fatalf("AES-256 enc/dec key length = %d/%d, want 32/32",
			len(csA.EncryptionKey), len(csA.DecryptionKey))
	}

	// AES-256 cipher keys depend on the full key: mutating the tail must change them.
	if bytes.Equal(csA.EncryptionKey, csB.EncryptionKey) {
		t.Error("AES-256 EncryptionKey unchanged when session-key tail changed: " +
			"KDF input was truncated to 16 bytes (the #686 bug)")
	}
	if bytes.Equal(csA.DecryptionKey, csB.DecryptionKey) {
		t.Error("AES-256 DecryptionKey unchanged when session-key tail changed: " +
			"KDF input was truncated to 16 bytes (the #686 bug)")
	}

	// Signing and application keys use only the first 16 bytes: tail change is invisible.
	if !bytes.Equal(csA.SigningKey, csB.SigningKey) {
		t.Error("SigningKey changed when session-key tail changed: signing must use 16-byte input")
	}
	if !bytes.Equal(csA.ApplicationKey, csB.ApplicationKey) {
		t.Error("ApplicationKey changed when session-key tail changed: app key must use 16-byte input")
	}
}

// TestDeriveAllKeys_AES128UsesTruncatedSessionKey verifies AES-128 cipher keys
// ignore bytes beyond the first 16 (16-byte KDF input, 16-byte output).
func TestDeriveAllKeys_AES128UsesTruncatedSessionKey(t *testing.T) {
	var preauth [64]byte

	keyA := make([]byte, 32)
	for i := range keyA {
		keyA[i] = byte(i + 1)
	}
	keyB := make([]byte, 32)
	copy(keyB, keyA)
	for i := 16; i < 32; i++ {
		keyB[i] ^= 0xFF
	}

	csA := DeriveAllKeys(keyA, types.Dialect0311, preauth, types.CipherAES128GCM, signing.SigningAlgAESGMAC, true)
	csB := DeriveAllKeys(keyB, types.Dialect0311, preauth, types.CipherAES128GCM, signing.SigningAlgAESGMAC, true)

	if len(csA.EncryptionKey) != 16 || len(csA.DecryptionKey) != 16 {
		t.Fatalf("AES-128 enc/dec key length = %d/%d, want 16/16",
			len(csA.EncryptionKey), len(csA.DecryptionKey))
	}
	if !bytes.Equal(csA.EncryptionKey, csB.EncryptionKey) {
		t.Error("AES-128 EncryptionKey changed when session-key tail changed: must use 16-byte input")
	}
	if !bytes.Equal(csA.DecryptionKey, csB.DecryptionKey) {
		t.Error("AES-128 DecryptionKey changed when session-key tail changed: must use 16-byte input")
	}
}

// TestDeriveAllKeys_NTLM16ByteKeyUnaffected verifies the fix is a no-op for the
// 16-byte NTLM session key path: AES-256 derivation from a 16-byte key still
// produces a valid 32-byte key (zero-pad-free path equivalence is not required,
// but length and determinism are).
func TestDeriveAllKeys_NTLM16ByteKeyUnaffected(t *testing.T) {
	var preauth [64]byte
	key := make([]byte, 16)
	for i := range key {
		key[i] = byte(i + 1)
	}

	cs := DeriveAllKeys(key, types.Dialect0311, preauth, types.CipherAES256CCM, signing.SigningAlgAESCMAC, true)
	if len(cs.EncryptionKey) != 32 || len(cs.DecryptionKey) != 32 {
		t.Fatalf("AES-256 enc/dec key length from 16-byte key = %d/%d, want 32/32",
			len(cs.EncryptionKey), len(cs.DecryptionKey))
	}
	if len(cs.SigningKey) != 16 {
		t.Fatalf("SigningKey length = %d, want 16", len(cs.SigningKey))
	}
}

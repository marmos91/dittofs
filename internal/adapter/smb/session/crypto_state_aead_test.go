package session

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/encryption"
	"github.com/marmos91/dittofs/internal/adapter/smb/signing"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// TestDeriveAllKeys_EncryptKeyLength asserts that the negotiated cipher ID
// drives the encryption/decryption key length: 32 bytes for the AES-256
// ciphers and 16 bytes for AES-128. This is the load-bearing switch for
// AES-256 support — if the cipher ID does not select a 256-bit KDF output,
// the server derives a 16-byte key but builds a 256-bit AEAD, which silently
// produces ciphertext the client cannot decrypt (the IO_TIMEOUT failure mode).
func TestDeriveAllKeys_EncryptKeyLength(t *testing.T) {
	sessionKey := bytes.Repeat([]byte{0xAB}, 16)
	var preauth [64]byte
	for i := range preauth {
		preauth[i] = byte(i)
	}

	tests := []struct {
		name       string
		cipherID   uint16
		wantKeyLen int
	}{
		{"AES-128-CCM", types.CipherAES128CCM, 16},
		{"AES-128-GCM", types.CipherAES128GCM, 16},
		{"AES-256-CCM", types.CipherAES256CCM, 32},
		{"AES-256-GCM", types.CipherAES256GCM, 32},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := DeriveAllKeys(sessionKey, types.Dialect0311, preauth, tt.cipherID, signing.SigningAlgAESGMAC, true)

			if got := len(cs.EncryptionKey); got != tt.wantKeyLen {
				t.Errorf("EncryptionKey length = %d, want %d", got, tt.wantKeyLen)
			}
			if got := len(cs.DecryptionKey); got != tt.wantKeyLen {
				t.Errorf("DecryptionKey length = %d, want %d", got, tt.wantKeyLen)
			}
			// Signing and application keys are always 128-bit regardless of cipher.
			if got := len(cs.SigningKey); got != 16 {
				t.Errorf("SigningKey length = %d, want 16", got)
			}
			if got := len(cs.ApplicationKey); got != 16 {
				t.Errorf("ApplicationKey length = %d, want 16", got)
			}
		})
	}
}

// TestDeriveAllKeys_AEADRoundTrip drives the full server path that smbtorture's
// encryption-aes-* suites exercise: negotiated cipher ID -> SP800-108 KDF with
// the correct L parameter -> AEAD construction with the derived key. It then
// confirms a client encrypting with the client-to-server key produces ciphertext
// the server's Decryptor (keyed on the same EncryptionKey) can open, for every
// supported cipher. This catches a mismatch between the KDF key length and the
// AEAD key length — the class of bug that makes AES-256 hang while AES-128 works.
func TestDeriveAllKeys_AEADRoundTrip(t *testing.T) {
	sessionKey := bytes.Repeat([]byte{0x5C}, 16)
	var preauth [64]byte
	for i := range preauth {
		preauth[i] = byte(0xF0 ^ i)
	}

	ciphers := []struct {
		name     string
		cipherID uint16
	}{
		{"AES-128-CCM", types.CipherAES128CCM},
		{"AES-128-GCM", types.CipherAES128GCM},
		{"AES-256-CCM", types.CipherAES256CCM},
		{"AES-256-GCM", types.CipherAES256GCM},
	}

	plaintext := []byte("smbtorture AES round-trip payload \x00\x01\x02 with binary bytes")

	for _, tc := range ciphers {
		t.Run(tc.name, func(t *testing.T) {
			cs := DeriveAllKeys(sessionKey, types.Dialect0311, preauth, tc.cipherID, signing.SigningAlgAESGMAC, true)
			if err := cs.CreateEncryptors(tc.cipherID); err != nil {
				t.Fatalf("CreateEncryptors: %v", err)
			}

			// The server's Decryptor opens client-to-server traffic and is keyed
			// on EncryptionKey ("ServerIn"). Build a matching client-side encryptor
			// from the same EncryptionKey to model the client encrypting a request.
			clientEnc, err := encryption.NewEncryptor(tc.cipherID, cs.EncryptionKey)
			if err != nil {
				t.Fatalf("client NewEncryptor: %v", err)
			}

			aad := []byte("transform-header-AAD")
			nonce, ciphertext, err := clientEnc.Encrypt(plaintext, aad)
			if err != nil {
				t.Fatalf("client Encrypt: %v", err)
			}

			got, err := cs.Decryptor.Decrypt(nonce, ciphertext, aad)
			if err != nil {
				t.Fatalf("server Decryptor.Decrypt: %v", err)
			}
			if !bytes.Equal(got, plaintext) {
				t.Fatalf("decrypted payload mismatch:\n got = %q\nwant = %q", got, plaintext)
			}
		})
	}
}

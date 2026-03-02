package encryption

import (
	"bytes"
	"crypto/rand"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// encryptorTestCase defines parameters for table-driven encryptor tests.
type encryptorTestCase struct {
	name      string
	keySize   int
	newFunc   func([]byte) (*aeadEncryptor, error)
	nonceSize int
}

var encryptorCases = []encryptorTestCase{
	{"GCM-128", 16, NewGCMEncryptor, 12},
	{"GCM-256", 32, NewGCMEncryptor, 12},
	{"CCM-128", 16, NewCCMEncryptor, 11},
	{"CCM-256", 32, NewCCMEncryptor, 11},
}

func makeEncryptor(t *testing.T, tc encryptorTestCase) *aeadEncryptor {
	t.Helper()
	key := make([]byte, tc.keySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	enc, err := tc.newFunc(key)
	if err != nil {
		t.Fatalf("new encryptor: %v", err)
	}
	return enc
}

func TestEncryptor_RoundTrip(t *testing.T) {
	for _, tc := range encryptorCases {
		t.Run(tc.name, func(t *testing.T) {
			enc := makeEncryptor(t, tc)

			plaintext := []byte("Hello, SMB3 encryption!")
			aad := make([]byte, 32)
			if _, err := rand.Read(aad); err != nil {
				t.Fatal(err)
			}

			nonce, ciphertext, err := enc.Encrypt(plaintext, aad)
			if err != nil {
				t.Fatalf("Encrypt: %v", err)
			}
			if len(nonce) != tc.nonceSize {
				t.Errorf("nonce size = %d, want %d", len(nonce), tc.nonceSize)
			}

			decrypted, err := enc.Decrypt(nonce, ciphertext, aad)
			if err != nil {
				t.Fatalf("Decrypt: %v", err)
			}
			if !bytes.Equal(decrypted, plaintext) {
				t.Errorf("decrypted = %q, want %q", decrypted, plaintext)
			}
		})
	}
}

func TestEncryptor_TamperedCiphertext(t *testing.T) {
	for _, tc := range encryptorCases {
		t.Run(tc.name, func(t *testing.T) {
			enc := makeEncryptor(t, tc)

			plaintext := []byte("Tamper test data")
			aad := make([]byte, 32)
			if _, err := rand.Read(aad); err != nil {
				t.Fatal(err)
			}

			nonce, ciphertext, err := enc.Encrypt(plaintext, aad)
			if err != nil {
				t.Fatal(err)
			}

			ciphertext[0] ^= 0xFF
			if _, err := enc.Decrypt(nonce, ciphertext, aad); err == nil {
				t.Error("expected error for tampered ciphertext, got nil")
			}
		})
	}
}

func TestEncryptor_TamperedAAD(t *testing.T) {
	for _, tc := range encryptorCases {
		t.Run(tc.name, func(t *testing.T) {
			enc := makeEncryptor(t, tc)

			plaintext := []byte("AAD tamper test")
			aad := make([]byte, 32)
			if _, err := rand.Read(aad); err != nil {
				t.Fatal(err)
			}

			nonce, ciphertext, err := enc.Encrypt(plaintext, aad)
			if err != nil {
				t.Fatal(err)
			}

			aad[0] ^= 0xFF
			if _, err := enc.Decrypt(nonce, ciphertext, aad); err == nil {
				t.Error("expected error for tampered AAD, got nil")
			}
		})
	}
}

func TestEncryptor_WrongKey(t *testing.T) {
	for _, tc := range encryptorCases {
		t.Run(tc.name, func(t *testing.T) {
			key1 := make([]byte, tc.keySize)
			key2 := make([]byte, tc.keySize)
			if _, err := rand.Read(key1); err != nil {
				t.Fatal(err)
			}
			if _, err := rand.Read(key2); err != nil {
				t.Fatal(err)
			}

			enc1, err := tc.newFunc(key1)
			if err != nil {
				t.Fatal(err)
			}
			enc2, err := tc.newFunc(key2)
			if err != nil {
				t.Fatal(err)
			}

			plaintext := []byte("Wrong key test")
			aad := make([]byte, 32)
			if _, err := rand.Read(aad); err != nil {
				t.Fatal(err)
			}

			nonce, ciphertext, err := enc1.Encrypt(plaintext, aad)
			if err != nil {
				t.Fatal(err)
			}

			if _, err := enc2.Decrypt(nonce, ciphertext, aad); err == nil {
				t.Error("expected error for wrong key, got nil")
			}
		})
	}
}

func TestEncryptor_NonceAndOverhead(t *testing.T) {
	for _, tc := range encryptorCases {
		t.Run(tc.name, func(t *testing.T) {
			enc := makeEncryptor(t, tc)

			if enc.NonceSize() != tc.nonceSize {
				t.Errorf("NonceSize() = %d, want %d", enc.NonceSize(), tc.nonceSize)
			}
			if enc.Overhead() != 16 {
				t.Errorf("Overhead() = %d, want 16", enc.Overhead())
			}
		})
	}
}

func TestEncryptor_UniqueNonces(t *testing.T) {
	for _, tc := range encryptorCases {
		t.Run(tc.name, func(t *testing.T) {
			enc := makeEncryptor(t, tc)

			plaintext := []byte("nonce uniqueness test")
			aad := make([]byte, 32)

			nonce1, _, err := enc.Encrypt(plaintext, aad)
			if err != nil {
				t.Fatal(err)
			}
			nonce2, _, err := enc.Encrypt(plaintext, aad)
			if err != nil {
				t.Fatal(err)
			}

			if bytes.Equal(nonce1, nonce2) {
				t.Error("two Encrypt calls produced identical nonces")
			}
		})
	}
}

func TestEncryptor_LargePayload(t *testing.T) {
	for _, tc := range encryptorCases {
		t.Run(tc.name, func(t *testing.T) {
			enc := makeEncryptor(t, tc)

			plaintext := make([]byte, 64*1024)
			if _, err := rand.Read(plaintext); err != nil {
				t.Fatal(err)
			}
			aad := make([]byte, 32)
			if _, err := rand.Read(aad); err != nil {
				t.Fatal(err)
			}

			nonce, ciphertext, err := enc.Encrypt(plaintext, aad)
			if err != nil {
				t.Fatal(err)
			}

			expectedLen := len(plaintext) + 16
			if len(ciphertext) != expectedLen {
				t.Errorf("ciphertext len = %d, want %d", len(ciphertext), expectedLen)
			}

			decrypted, err := enc.Decrypt(nonce, ciphertext, aad)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(decrypted, plaintext) {
				t.Error("large payload round-trip failed")
			}
		})
	}
}

func TestNewEncryptor_Factory(t *testing.T) {
	tests := []struct {
		name      string
		cipherID  uint16
		keySize   int
		nonceSize int
		wantErr   bool
	}{
		{"GCM-128", types.CipherAES128GCM, 16, 12, false},
		{"GCM-256", types.CipherAES256GCM, 32, 12, false},
		{"CCM-128", types.CipherAES128CCM, 16, 11, false},
		{"CCM-256", types.CipherAES256CCM, 32, 11, false},
		{"Unsupported", 0xFFFF, 16, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := make([]byte, tt.keySize)
			if _, err := rand.Read(key); err != nil {
				t.Fatal(err)
			}

			enc, err := NewEncryptor(tt.cipherID, key)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("NewEncryptor: %v", err)
			}
			if enc.NonceSize() != tt.nonceSize {
				t.Errorf("NonceSize() = %d, want %d", enc.NonceSize(), tt.nonceSize)
			}
		})
	}
}

func BenchmarkEncryptGCM(b *testing.B) {
	benchmarkEncrypt(b, NewGCMEncryptor, 16)
}

func BenchmarkDecryptGCM(b *testing.B) {
	benchmarkDecrypt(b, NewGCMEncryptor, 16)
}

func BenchmarkEncryptCCM(b *testing.B) {
	benchmarkEncrypt(b, NewCCMEncryptor, 16)
}

func BenchmarkDecryptCCM(b *testing.B) {
	benchmarkDecrypt(b, NewCCMEncryptor, 16)
}

func benchmarkEncrypt(b *testing.B, newFunc func([]byte) (*aeadEncryptor, error), keySize int) {
	b.Helper()
	key := make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		b.Fatal(err)
	}
	enc, err := newFunc(key)
	if err != nil {
		b.Fatal(err)
	}
	plaintext := make([]byte, 4096)
	if _, err := rand.Read(plaintext); err != nil {
		b.Fatal(err)
	}
	aad := make([]byte, 32)

	b.ResetTimer()
	b.SetBytes(int64(len(plaintext)))
	for i := 0; i < b.N; i++ {
		_, _, _ = enc.Encrypt(plaintext, aad)
	}
}

func benchmarkDecrypt(b *testing.B, newFunc func([]byte) (*aeadEncryptor, error), keySize int) {
	b.Helper()
	key := make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		b.Fatal(err)
	}
	enc, err := newFunc(key)
	if err != nil {
		b.Fatal(err)
	}
	plaintext := make([]byte, 4096)
	if _, err := rand.Read(plaintext); err != nil {
		b.Fatal(err)
	}
	aad := make([]byte, 32)
	nonce, ciphertext, err := enc.Encrypt(plaintext, aad)
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	b.SetBytes(int64(len(plaintext)))
	for i := 0; i < b.N; i++ {
		_, _ = enc.Decrypt(nonce, ciphertext, aad)
	}
}

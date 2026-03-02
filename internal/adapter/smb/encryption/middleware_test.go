package encryption

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/header"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// testSession is a minimal mock that provides the crypto methods needed by the middleware.
type testSession struct {
	encryptor   Encryptor
	decryptor   Encryptor
	encryptData bool
}

func (s *testSession) ShouldEncrypt() bool {
	return s.encryptData && s.encryptor != nil
}

func (s *testSession) EncryptWithNonce(nonce, plaintext, aad []byte) ([]byte, error) {
	return s.encryptor.EncryptWithNonce(nonce, plaintext, aad)
}

func (s *testSession) DecryptMessage(nonce, ciphertext, aad []byte) ([]byte, error) {
	return s.decryptor.Decrypt(nonce, ciphertext, aad)
}

func (s *testSession) EncryptorNonceSize() int { return s.encryptor.NonceSize() }
func (s *testSession) DecryptorNonceSize() int { return s.decryptor.NonceSize() }
func (s *testSession) EncryptorOverhead() int  { return s.encryptor.Overhead() }

func makeTestSession(t *testing.T, cipherId uint16) *testSession {
	t.Helper()
	keySize := 16
	if cipherId == types.CipherAES256GCM || cipherId == types.CipherAES256CCM {
		keySize = 32
	}
	key := make([]byte, keySize)
	for i := range key {
		key[i] = byte(i + 1)
	}

	enc, err := NewEncryptor(cipherId, key)
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}
	dec, err := NewEncryptor(cipherId, key)
	if err != nil {
		t.Fatalf("NewEncryptor for decryptor: %v", err)
	}

	return &testSession{
		encryptor:   enc,
		decryptor:   dec,
		encryptData: true,
	}
}

func makeMiddleware(sessionID uint64, sess *testSession) EncryptionMiddleware {
	return NewEncryptionMiddleware(func(id uint64) (EncryptableSession, bool) {
		if id == sessionID {
			return sess, true
		}
		return nil, false
	})
}

func TestMiddleware_RoundTrip(t *testing.T) {
	tests := []struct {
		name     string
		cipherID uint16
	}{
		{"AES-128-GCM", types.CipherAES128GCM},
		{"AES-128-CCM", types.CipherAES128CCM},
		{"AES-256-GCM", types.CipherAES256GCM},
		{"AES-256-CCM", types.CipherAES256CCM},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessionID := uint64(0x1234)
			sess := makeTestSession(t, tt.cipherID)
			mw := makeMiddleware(sessionID, sess)

			original := []byte("Round-trip test message for " + tt.name)

			encrypted, err := mw.EncryptResponse(sessionID, original)
			if err != nil {
				t.Fatalf("EncryptResponse: %v", err)
			}

			decrypted, decSessionID, err := mw.DecryptRequest(encrypted)
			if err != nil {
				t.Fatalf("DecryptRequest: %v", err)
			}
			if decSessionID != sessionID {
				t.Errorf("sessionID = %d, want %d", decSessionID, sessionID)
			}
			if string(decrypted) != string(original) {
				t.Errorf("decrypted = %q, want %q", decrypted, original)
			}
		})
	}
}

func TestMiddleware_EncryptResponse_TransformHeaderFields(t *testing.T) {
	sessionID := uint64(0xCAFEBABE)
	sess := makeTestSession(t, types.CipherAES128GCM)
	mw := makeMiddleware(sessionID, sess)

	original := []byte("check transform header fields")
	encrypted, err := mw.EncryptResponse(sessionID, original)
	if err != nil {
		t.Fatalf("EncryptResponse: %v", err)
	}

	if len(encrypted) < header.TransformHeaderSize {
		t.Fatalf("encrypted message too short: %d", len(encrypted))
	}
	th, err := header.ParseTransformHeader(encrypted[:header.TransformHeaderSize])
	if err != nil {
		t.Fatalf("ParseTransformHeader: %v", err)
	}

	if th.SessionId != sessionID {
		t.Errorf("SessionId = 0x%x, want 0x%x", th.SessionId, sessionID)
	}
	if th.Flags != 0x0001 {
		t.Errorf("Flags = 0x%04x, want 0x0001", th.Flags)
	}
	if th.OriginalMessageSize != uint32(len(original)) {
		t.Errorf("OriginalMessageSize = %d, want %d", th.OriginalMessageSize, len(original))
	}
	// Signature should not be all zeros (it contains the AEAD tag)
	allZero := true
	for _, b := range th.Signature {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Error("Signature should not be all zeros")
	}
}

func TestMiddleware_DecryptRequest_TamperedCiphertext(t *testing.T) {
	sessionID := uint64(0x1234)
	sess := makeTestSession(t, types.CipherAES128GCM)
	mw := makeMiddleware(sessionID, sess)

	original := []byte("test message")
	encrypted, err := mw.EncryptResponse(sessionID, original)
	if err != nil {
		t.Fatalf("EncryptResponse: %v", err)
	}

	if len(encrypted) > header.TransformHeaderSize+1 {
		encrypted[header.TransformHeaderSize+1] ^= 0xFF
	}

	if _, _, err := mw.DecryptRequest(encrypted); err == nil {
		t.Fatal("expected error on tampered ciphertext, got nil")
	}
}

func TestMiddleware_DecryptRequest_UnknownSession(t *testing.T) {
	mw := NewEncryptionMiddleware(func(id uint64) (EncryptableSession, bool) {
		return nil, false
	})

	th := &header.TransformHeader{
		SessionId:           0x9999,
		OriginalMessageSize: 10,
		Flags:               0x0001,
	}
	encoded := append(th.Encode(), make([]byte, 10)...)

	if _, _, err := mw.DecryptRequest(encoded); err == nil {
		t.Fatal("expected error for unknown session, got nil")
	}
}

func TestMiddleware_ShouldEncrypt(t *testing.T) {
	sessionID := uint64(0x1234)

	tests := []struct {
		name      string
		sessionID uint64
		encrypt   bool
		want      bool
	}{
		{"encrypted session", sessionID, true, true},
		{"unencrypted session", sessionID, false, false},
		{"unknown session", 0x9999, true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sess := makeTestSession(t, types.CipherAES128GCM)
			sess.encryptData = tt.encrypt
			mw := makeMiddleware(sessionID, sess)

			if got := mw.ShouldEncrypt(tt.sessionID); got != tt.want {
				t.Errorf("ShouldEncrypt(%d) = %v, want %v", tt.sessionID, got, tt.want)
			}
		})
	}
}

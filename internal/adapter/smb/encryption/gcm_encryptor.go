package encryption

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
)

// aeadEncryptor implements Encryptor by wrapping a cipher.AEAD instance.
// Both GCM and CCM encryptors share identical delegation logic; only their
// construction differs (via NewGCMEncryptor and NewCCMEncryptor).
type aeadEncryptor struct {
	aead cipher.AEAD
}

// NewGCMEncryptor creates an Encryptor using AES-GCM (Galois/Counter Mode).
//
// AES-GCM is the preferred cipher for SMB 3.1.1 due to hardware acceleration
// on modern CPUs (AES-NI + PCLMULQDQ).
//
// Key must be 16 bytes (AES-128) or 32 bytes (AES-256).
// Nonce: 12 bytes, Auth tag: 16 bytes.
func NewGCMEncryptor(key []byte) (*aeadEncryptor, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}
	return &aeadEncryptor{aead: gcm}, nil
}

// NewCCMEncryptor creates an Encryptor using AES-CCM (Counter with CBC-MAC).
//
// AES-CCM is the mandatory cipher for SMB 3.0 and 3.0.2 clients that do not
// support cipher negotiation. It is also available for SMB 3.1.1 clients that
// negotiate it.
//
// Key must be 16 bytes (AES-128) or 32 bytes (AES-256).
// Nonce: 11 bytes, Auth tag: 16 bytes (required by SMB3 specification).
func NewCCMEncryptor(key []byte) (*aeadEncryptor, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}
	ccm, err := NewCCM(block, 16, 11)
	if err != nil {
		return nil, fmt.Errorf("create CCM: %w", err)
	}
	return &aeadEncryptor{aead: ccm}, nil
}

func (e *aeadEncryptor) Encrypt(plaintext, aad []byte) (nonce []byte, ciphertext []byte, err error) {
	nonce = make([]byte, e.aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("generate nonce: %w", err)
	}
	ciphertext = e.aead.Seal(nil, nonce, plaintext, aad)
	return nonce, ciphertext, nil
}

func (e *aeadEncryptor) EncryptWithNonce(nonce, plaintext, aad []byte) ([]byte, error) {
	return e.aead.Seal(nil, nonce, plaintext, aad), nil
}

func (e *aeadEncryptor) Decrypt(nonce, ciphertext, aad []byte) ([]byte, error) {
	return e.aead.Open(nil, nonce, ciphertext, aad)
}

func (e *aeadEncryptor) NonceSize() int {
	return e.aead.NonceSize()
}

func (e *aeadEncryptor) Overhead() int {
	return e.aead.Overhead()
}

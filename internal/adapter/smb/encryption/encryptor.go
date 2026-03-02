package encryption

import (
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// Encryptor provides AEAD encryption and decryption for SMB3 messages.
// All implementations produce a 16-byte authentication tag.
type Encryptor interface {
	// Encrypt generates a fresh random nonce and encrypts plaintext with the given AAD.
	Encrypt(plaintext, aad []byte) (nonce []byte, ciphertext []byte, err error)

	// EncryptWithNonce encrypts with a caller-provided nonce. Needed by the
	// encryption middleware which must set the nonce in the transform header
	// before computing the AAD.
	EncryptWithNonce(nonce, plaintext, aad []byte) ([]byte, error)

	// Decrypt decrypts ciphertext with the given nonce and AAD.
	Decrypt(nonce, ciphertext, aad []byte) ([]byte, error)

	// NonceSize returns the nonce size (GCM: 12, CCM: 11).
	NonceSize() int

	// Overhead returns the authentication tag size (always 16 for SMB3).
	Overhead() int
}

// NewEncryptor creates an Encryptor for the given cipher ID and key.
func NewEncryptor(cipherId uint16, key []byte) (Encryptor, error) {
	switch cipherId {
	case types.CipherAES128GCM, types.CipherAES256GCM:
		return NewGCMEncryptor(key)
	case types.CipherAES128CCM, types.CipherAES256CCM:
		return NewCCMEncryptor(key)
	default:
		return nil, fmt.Errorf("unsupported cipher ID: 0x%04x", cipherId)
	}
}

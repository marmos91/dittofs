// Package kdf implements SP800-108 Counter Mode KDF with HMAC-SHA256 for SMB 3.x
// session key derivation.
//
// The KDF derives four types of session keys: signing, encryption, decryption,
// and application keys. For SMB 3.0/3.0.2, constant label/context strings are used.
// For SMB 3.1.1, the preauth integrity hash is used as the context.
//
// Reference: [SP800-108] Section 5.1, [MS-SMB2] Section 3.1.4.2
package kdf

import (
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// KeyPurpose identifies the purpose of a derived key.
type KeyPurpose uint8

const (
	// SigningKeyPurpose derives the session signing key.
	SigningKeyPurpose KeyPurpose = iota
	// EncryptionKeyPurpose derives the session encryption key (client-to-server).
	EncryptionKeyPurpose
	// DecryptionKeyPurpose derives the session decryption key (server-to-client).
	DecryptionKeyPurpose
	// ApplicationKeyPurpose derives the application key for higher-layer protocols.
	ApplicationKeyPurpose
)

// String returns a human-readable name for the key purpose.
func (p KeyPurpose) String() string {
	switch p {
	case SigningKeyPurpose:
		return "Signing"
	case EncryptionKeyPurpose:
		return "Encryption"
	case DecryptionKeyPurpose:
		return "Decryption"
	case ApplicationKeyPurpose:
		return "Application"
	default:
		return "Unknown"
	}
}

// DeriveKey implements SP800-108 Counter Mode KDF with HMAC-SHA256.
// TODO: implement
func DeriveKey(ki, label, context []byte, keyLenBits uint32) []byte {
	return nil
}

// LabelAndContext returns the correct label and context byte slices for the given
// key purpose and dialect, per [MS-SMB2] Section 3.1.4.2.
// TODO: implement
func LabelAndContext(purpose KeyPurpose, dialect types.Dialect, preauthHash [64]byte) (label, context []byte) {
	return nil, nil
}

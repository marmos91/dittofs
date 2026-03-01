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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"

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

// DeriveKey implements SP800-108 Counter Mode KDF with HMAC-SHA256 PRF.
//
// Wire format: counter(4 bytes BE) || label || 0x00 || context || L(4 bytes BE)
//
// Parameters:
//   - ki: key derivation key (the session key)
//   - label: purpose-specific label bytes (including null terminator)
//   - context: purpose-specific context bytes
//   - keyLenBits: desired key length in bits (128 or 256)
//
// Returns the derived key as a byte slice of length keyLenBits/8.
//
// For SMB3, a single iteration (counter=1) with HMAC-SHA256 produces 256 bits,
// which is sufficient for both 128-bit and 256-bit keys.
func DeriveKey(ki, label, context []byte, keyLenBits uint32) []byte {
	h := hmac.New(sha256.New, ki)

	// Counter i = 1 (4 bytes, big-endian)
	var counter [4]byte
	binary.BigEndian.PutUint32(counter[:], 1)
	h.Write(counter[:])

	// Label (includes null terminator as part of the label)
	h.Write(label)

	// Separator 0x00
	h.Write([]byte{0x00})

	// Context
	h.Write(context)

	// L value (4 bytes, big-endian) - desired key length in bits
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], keyLenBits)
	h.Write(length[:])

	result := h.Sum(nil)
	return result[:keyLenBits/8]
}

// Label/context constants for SMB 3.0/3.0.2 per [MS-SMB2] Section 3.1.4.2.
// Each label includes its null terminator as part of the byte literal.
var (
	// SMB 3.0/3.0.2 labels and contexts
	label30Signing    = []byte("SMB2AESCMAC\x00")
	label30Encryption = []byte("SMB2AESCCM\x00")
	label30Decryption = []byte("SMB2AESCCM\x00")
	label30App        = []byte("SMB2APP\x00")

	ctx30Signing    = []byte("SmbSign\x00")
	ctx30Encryption = []byte("ServerIn \x00") // note trailing space before null
	ctx30Decryption = []byte("ServerOut\x00")
	ctx30App        = []byte("SmbRpc\x00")

	// SMB 3.1.1 labels (context is always the preauth integrity hash)
	label311Signing    = []byte("SMBSigningKey\x00")
	label311Encryption = []byte("SMBC2SCipherKey\x00")
	label311Decryption = []byte("SMBS2CCipherKey\x00")
	label311App        = []byte("SMBAppKey\x00")
)

// LabelAndContext returns the correct label and context byte slices for the given
// key purpose and dialect, per [MS-SMB2] Section 3.1.4.2.
//
// For SMB 3.0/3.0.2: constant label/context strings are used.
// For SMB 3.1.1: different labels with preauthHash as context.
func LabelAndContext(purpose KeyPurpose, dialect types.Dialect, preauthHash [64]byte) (label, context []byte) {
	if dialect == types.Dialect0311 {
		// SMB 3.1.1: preauth integrity hash as context for all purposes
		ctx := make([]byte, 64)
		copy(ctx, preauthHash[:])

		switch purpose {
		case SigningKeyPurpose:
			return label311Signing, ctx
		case EncryptionKeyPurpose:
			return label311Encryption, ctx
		case DecryptionKeyPurpose:
			return label311Decryption, ctx
		case ApplicationKeyPurpose:
			return label311App, ctx
		}
	}

	// SMB 3.0/3.0.2: constant label/context strings
	switch purpose {
	case SigningKeyPurpose:
		return label30Signing, ctx30Signing
	case EncryptionKeyPurpose:
		return label30Encryption, ctx30Encryption
	case DecryptionKeyPurpose:
		return label30Decryption, ctx30Decryption
	case ApplicationKeyPurpose:
		return label30App, ctx30App
	}

	return nil, nil
}

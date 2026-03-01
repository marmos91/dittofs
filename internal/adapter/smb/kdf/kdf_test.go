package kdf

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// mustHex decodes a hex string or panics.
func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// TestDeriveKey_SMB30_SigningKey tests the SP800-108 KDF against the MS-SMB2
// spec test vector for SMB 3.0 signing key derivation.
//
// Source: https://learn.microsoft.com/en-us/archive/blogs/openspecification/smb-2-and-smb-3-security-in-windows-10-the-anatomy-of-signing-and-cryptographic-keys
// SessionKey = 0x7CD451825D0450D235424E44BA6E78CC
// SigningKey  = 0x0B7E9C5CAC36C0F6EA9AB275298CEDCE
func TestDeriveKey_SMB30_SigningKey(t *testing.T) {
	sessionKey := mustHex("7CD451825D0450D235424E44BA6E78CC")
	expectedSigningKey := mustHex("0B7E9C5CAC36C0F6EA9AB275298CEDCE")

	label, context := LabelAndContext(SigningKeyPurpose, types.Dialect0300, [64]byte{})
	signingKey := DeriveKey(sessionKey, label, context, 128)

	if !bytes.Equal(signingKey, expectedSigningKey) {
		t.Errorf("SMB 3.0 signing key mismatch:\n  got:  %x\n  want: %x", signingKey, expectedSigningKey)
	}
}

// TestDeriveKey_SMB311_SigningKey tests the SP800-108 KDF for SMB 3.1.1
// signing key derivation with preauth hash context.
//
// The KDF uses the 3.1.1 label "SMBSigningKey\0" with the preauth hash
// as context (instead of the constant "SmbSign\0" used by 3.0).
// We verify:
//  1. The KDF produces deterministic output
//  2. The output differs from SMB 3.0 (different label + different context)
//  3. Different preauth hashes produce different keys
func TestDeriveKey_SMB311_SigningKey(t *testing.T) {
	sessionKey := mustHex("270E1BA896585EEB7AF3472D3B4C75A7")

	var preauthHash [64]byte
	for i := range preauthHash {
		preauthHash[i] = byte(i)
	}

	label, context := LabelAndContext(SigningKeyPurpose, types.Dialect0311, preauthHash)
	signingKey := DeriveKey(sessionKey, label, context, 128)

	// Must be 16 bytes
	if len(signingKey) != 16 {
		t.Fatalf("signing key should be 16 bytes, got %d", len(signingKey))
	}

	// Must be deterministic
	signingKey2 := DeriveKey(sessionKey, label, context, 128)
	if !bytes.Equal(signingKey, signingKey2) {
		t.Error("KDF is not deterministic")
	}

	// Must differ from SMB 3.0 derivation with same session key
	label30, ctx30 := LabelAndContext(SigningKeyPurpose, types.Dialect0300, [64]byte{})
	signingKey30 := DeriveKey(sessionKey, label30, ctx30, 128)
	if bytes.Equal(signingKey, signingKey30) {
		t.Error("3.1.1 signing key should differ from 3.0 signing key")
	}

	// Must differ with different preauth hash
	var otherHash [64]byte
	for i := range otherHash {
		otherHash[i] = byte(i + 100)
	}
	labelOther, ctxOther := LabelAndContext(SigningKeyPurpose, types.Dialect0311, otherHash)
	signingKeyOther := DeriveKey(sessionKey, labelOther, ctxOther, 128)
	if bytes.Equal(signingKey, signingKeyOther) {
		t.Error("different preauth hashes should produce different signing keys")
	}
}

// TestLabelAndContext_AllPurposes_SMB30 verifies that LabelAndContext returns
// the correct label/context strings for all 4 key purposes with SMB 3.0.
func TestLabelAndContext_AllPurposes_SMB30(t *testing.T) {
	dummyHash := [64]byte{} // not used for 3.0

	tests := []struct {
		purpose       KeyPurpose
		expectedLabel []byte
		expectedCtx   []byte
	}{
		{
			purpose:       SigningKeyPurpose,
			expectedLabel: []byte("SMB2AESCMAC\x00"),
			expectedCtx:   []byte("SmbSign\x00"),
		},
		{
			purpose:       EncryptionKeyPurpose,
			expectedLabel: []byte("SMB2AESCCM\x00"),
			expectedCtx:   []byte("ServerIn \x00"),
		},
		{
			purpose:       DecryptionKeyPurpose,
			expectedLabel: []byte("SMB2AESCCM\x00"),
			expectedCtx:   []byte("ServerOut\x00"),
		},
		{
			purpose:       ApplicationKeyPurpose,
			expectedLabel: []byte("SMB2APP\x00"),
			expectedCtx:   []byte("SmbRpc\x00"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.purpose.String(), func(t *testing.T) {
			label, ctx := LabelAndContext(tt.purpose, types.Dialect0300, dummyHash)
			if !bytes.Equal(label, tt.expectedLabel) {
				t.Errorf("label mismatch:\n  got:  %q\n  want: %q", label, tt.expectedLabel)
			}
			if !bytes.Equal(ctx, tt.expectedCtx) {
				t.Errorf("context mismatch:\n  got:  %q\n  want: %q", ctx, tt.expectedCtx)
			}
		})
	}
}

// TestLabelAndContext_AllPurposes_SMB302 verifies SMB 3.0.2 uses the same
// constant label/context strings as SMB 3.0.
func TestLabelAndContext_AllPurposes_SMB302(t *testing.T) {
	dummyHash := [64]byte{}

	label30, ctx30 := LabelAndContext(SigningKeyPurpose, types.Dialect0300, dummyHash)
	label302, ctx302 := LabelAndContext(SigningKeyPurpose, types.Dialect0302, dummyHash)

	if !bytes.Equal(label30, label302) {
		t.Error("SMB 3.0 and 3.0.2 signing labels should be identical")
	}
	if !bytes.Equal(ctx30, ctx302) {
		t.Error("SMB 3.0 and 3.0.2 signing contexts should be identical")
	}
}

// TestLabelAndContext_AllPurposes_SMB311 verifies that LabelAndContext returns
// the correct labels for 3.1.1 and uses preauth hash as context.
func TestLabelAndContext_AllPurposes_SMB311(t *testing.T) {
	var preauthHash [64]byte
	for i := range preauthHash {
		preauthHash[i] = byte(i)
	}

	tests := []struct {
		purpose       KeyPurpose
		expectedLabel []byte
	}{
		{
			purpose:       SigningKeyPurpose,
			expectedLabel: []byte("SMBSigningKey\x00"),
		},
		{
			purpose:       EncryptionKeyPurpose,
			expectedLabel: []byte("SMBC2SCipherKey\x00"),
		},
		{
			purpose:       DecryptionKeyPurpose,
			expectedLabel: []byte("SMBS2CCipherKey\x00"),
		},
		{
			purpose:       ApplicationKeyPurpose,
			expectedLabel: []byte("SMBAppKey\x00"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.purpose.String(), func(t *testing.T) {
			label, ctx := LabelAndContext(tt.purpose, types.Dialect0311, preauthHash)
			if !bytes.Equal(label, tt.expectedLabel) {
				t.Errorf("label mismatch:\n  got:  %q\n  want: %q", label, tt.expectedLabel)
			}
			if !bytes.Equal(ctx, preauthHash[:]) {
				t.Error("3.1.1 context should be the preauth hash")
			}
		})
	}
}

// TestSigningKeyAlways128Bit verifies that signing key derivation always uses
// 128 bits regardless of cipher, while encryption/decryption can be 256 bits.
func TestSigningKeyAlways128Bit(t *testing.T) {
	sessionKey := mustHex("7CD451825D0450D235424E44BA6E78CC")
	label, context := LabelAndContext(SigningKeyPurpose, types.Dialect0300, [64]byte{})

	// Signing key is always 128-bit
	signingKey := DeriveKey(sessionKey, label, context, 128)
	if len(signingKey) != 16 {
		t.Errorf("signing key should be 16 bytes, got %d", len(signingKey))
	}

	// Encryption key can be 256-bit (for AES-256 ciphers)
	encLabel, encCtx := LabelAndContext(EncryptionKeyPurpose, types.Dialect0300, [64]byte{})
	encKey256 := DeriveKey(sessionKey, encLabel, encCtx, 256)
	if len(encKey256) != 32 {
		t.Errorf("256-bit encryption key should be 32 bytes, got %d", len(encKey256))
	}
}

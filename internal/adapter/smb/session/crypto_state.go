package session

import (
	"github.com/marmos91/dittofs/internal/adapter/smb/kdf"
	"github.com/marmos91/dittofs/internal/adapter/smb/signing"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// SessionCryptoState holds per-session cryptographic state for all SMB dialects.
//
// For SMB 2.x sessions, only SigningKey and Signer are populated (HMAC-SHA256).
// For SMB 3.x sessions, all four keys are derived via SP800-108 KDF and the
// Signer is created based on the negotiated signing algorithm (CMAC or GMAC).
//
// This replaces the old SessionSigningState and provides a unified abstraction
// across all dialect versions.
type SessionCryptoState struct {
	// Signer is the polymorphic signing implementation for this session.
	// For 2.x: HMACSigner, for 3.0+: CMACSigner or GMACSigner.
	Signer signing.Signer

	// SigningKey is the raw signing key bytes.
	// For 2.x: copy of the raw session key (signer handles normalization).
	// For 3.x: KDF-derived 16-byte signing key.
	SigningKey []byte

	// EncryptionKey is the client-to-server encryption key (Phase 35).
	// Only populated for SMB 3.x sessions.
	EncryptionKey []byte

	// DecryptionKey is the server-to-client decryption key (Phase 35).
	// Only populated for SMB 3.x sessions.
	DecryptionKey []byte

	// ApplicationKey is the key for higher-layer protocols.
	// Only populated for SMB 3.x sessions.
	ApplicationKey []byte

	// SigningEnabled indicates if signing is active for this session.
	SigningEnabled bool

	// SigningRequired indicates if signing is mandatory for this session.
	SigningRequired bool
}

// DeriveAllKeys creates a fully constructed SessionCryptoState with all keys
// derived from the session key using the appropriate method for the dialect.
//
// For SMB 2.x (dialect < 3.0): creates an HMACSigner directly from the session
// key. Only SigningKey is set; encryption/decryption/application keys are not
// used for 2.x.
//
// For SMB 3.x (dialect >= 3.0): derives all 4 keys via SP800-108 KDF with
// dialect-specific label/context. Encryption/decryption key length is 256 bits
// for AES-256 ciphers, 128 bits otherwise. The Signer is created via the
// NewSigner factory based on dialect and signing algorithm ID.
//
// Parameters:
//   - sessionKey: the raw session key from NTLM/Kerberos authentication
//   - dialect: the negotiated SMB dialect
//   - preauthHash: the preauth integrity hash (only used for 3.1.1)
//   - cipherId: the negotiated cipher ID (determines encryption key length)
//   - signingAlgId: the negotiated signing algorithm ID
func DeriveAllKeys(sessionKey []byte, dialect types.Dialect, preauthHash [64]byte, cipherId uint16, signingAlgId uint16) *SessionCryptoState {
	cs := &SessionCryptoState{}

	if dialect < types.Dialect0300 {
		// SMB 2.x: direct HMAC-SHA256 from session key, no KDF
		cs.Signer = signing.NewHMACSigner(sessionKey)
		// Store a copy of the signing key
		cs.SigningKey = make([]byte, len(sessionKey))
		copy(cs.SigningKey, sessionKey)
		return cs
	}

	// SMB 3.x: derive all 4 keys via SP800-108 KDF

	// Signing key is always 128-bit
	sigLabel, sigCtx := kdf.LabelAndContext(kdf.SigningKeyPurpose, dialect, preauthHash)
	cs.SigningKey = kdf.DeriveKey(sessionKey, sigLabel, sigCtx, 128)
	cs.Signer = signing.NewSigner(dialect, signingAlgId, cs.SigningKey)

	// Encryption key length depends on cipher: 256-bit for AES-256, 128-bit otherwise
	encKeyBits := uint32(128)
	if cipherId == types.CipherAES256CCM || cipherId == types.CipherAES256GCM {
		encKeyBits = 256
	}

	encLabel, encCtx := kdf.LabelAndContext(kdf.EncryptionKeyPurpose, dialect, preauthHash)
	cs.EncryptionKey = kdf.DeriveKey(sessionKey, encLabel, encCtx, encKeyBits)

	decLabel, decCtx := kdf.LabelAndContext(kdf.DecryptionKeyPurpose, dialect, preauthHash)
	cs.DecryptionKey = kdf.DeriveKey(sessionKey, decLabel, decCtx, encKeyBits)

	// Application key is always 128-bit
	appLabel, appCtx := kdf.LabelAndContext(kdf.ApplicationKeyPurpose, dialect, preauthHash)
	cs.ApplicationKey = kdf.DeriveKey(sessionKey, appLabel, appCtx, 128)

	return cs
}

// Destroy zeros all key material for defense-in-depth.
// Should be called when the session is being destroyed.
func (cs *SessionCryptoState) Destroy() {
	if cs == nil {
		return
	}
	clear(cs.SigningKey)
	clear(cs.EncryptionKey)
	clear(cs.DecryptionKey)
	clear(cs.ApplicationKey)
	cs.Signer = nil
}

// ShouldSign returns true if outgoing messages should be signed.
func (cs *SessionCryptoState) ShouldSign() bool {
	return cs != nil && cs.SigningEnabled && cs.Signer != nil
}

// ShouldVerify returns true if incoming messages should have signatures verified.
func (cs *SessionCryptoState) ShouldVerify() bool {
	return cs != nil && cs.SigningEnabled && cs.Signer != nil
}

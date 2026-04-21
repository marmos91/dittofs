package session

import (
	"errors"
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/smb/encryption"
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

	// SessionKey is the raw session key from NTLM/Kerberos authentication,
	// before any SP800-108 KDF derivation. Retained on the session so that
	// SMB2_SESSION_FLAG_BINDING requests on a later connection can derive
	// per-channel signing keys from the same input (MS-SMB2 §3.1.4.2,
	// §3.3.5.5.2). For SMB 2.x this is the same material as SigningKey, but
	// for 3.x SigningKey is a KDF output — only SessionKey is the raw input
	// needed for per-channel derivation. Zeroed by Destroy.
	SessionKey []byte

	// SigningKey is the signing key bytes actually used by Signer.
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

	// EncryptData indicates whether this session requires encryption.
	// Set to true when the session is established with encryption negotiated.
	EncryptData bool

	// Encryptor encrypts outgoing messages (server-to-client).
	// Uses DecryptionKey ("ServerOut") because key names are from CLIENT perspective:
	//   DecryptionKey = "ServerOut" = server encrypts TO client = server uses for ENCRYPTION
	Encryptor encryption.Encryptor

	// Decryptor decrypts incoming messages (client-to-server).
	// Uses EncryptionKey ("ServerIn") because key names are from CLIENT perspective:
	//   EncryptionKey = "ServerIn" = client encrypts TO server = server uses for DECRYPTION
	Decryptor encryption.Encryptor

	// CipherId is the negotiated cipher ID for this session.
	CipherId uint16
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
	cs.SessionKey = make([]byte, len(sessionKey))
	copy(cs.SessionKey, sessionKey)

	if dialect < types.Dialect0300 {
		// SMB 2.x: direct HMAC-SHA256 from session key, no KDF
		cs.Signer = signing.NewHMACSigner(sessionKey)
		// Store a copy of the signing key
		cs.SigningKey = make([]byte, len(sessionKey))
		copy(cs.SigningKey, sessionKey)
		return cs
	}

	// SMB 3.x: derive all 4 keys via SP800-108 KDF
	sigLabel, sigCtx := kdf.LabelAndContext(kdf.SigningKeyPurpose, dialect, preauthHash)
	cs.SigningKey = kdf.DeriveKey(sessionKey, sigLabel, sigCtx, 128)
	cs.Signer = signing.NewSigner(dialect, signingAlgId, cs.SigningKey)

	encKeyBits := uint32(128)
	if cipherId == types.CipherAES256CCM || cipherId == types.CipherAES256GCM {
		encKeyBits = 256
	}

	encLabel, encCtx := kdf.LabelAndContext(kdf.EncryptionKeyPurpose, dialect, preauthHash)
	cs.EncryptionKey = kdf.DeriveKey(sessionKey, encLabel, encCtx, encKeyBits)

	decLabel, decCtx := kdf.LabelAndContext(kdf.DecryptionKeyPurpose, dialect, preauthHash)
	cs.DecryptionKey = kdf.DeriveKey(sessionKey, decLabel, decCtx, encKeyBits)

	appLabel, appCtx := kdf.LabelAndContext(kdf.ApplicationKeyPurpose, dialect, preauthHash)
	cs.ApplicationKey = kdf.DeriveKey(sessionKey, appLabel, appCtx, 128)

	return cs
}

// DeriveChannelSigningKey derives a per-channel signing key for a connection
// bound via SMB2_SESSION_FLAG_BINDING (MS-SMB2 §3.1.4.2 and §3.3.5.5.2).
//
// The input session key is the session-wide key established by the original
// authentication. The output is a channel-specific 16-byte signing key used
// to verify signatures on requests arriving over the bound connection.
//
// Label/context rules (matching Samba libcli/smb/smb2_signing.c:38-84):
//   - SMB 3.1.1 (Dialect0311): label "SMBSigningKey\0", context = the
//     channel's own preauth integrity hash (64 bytes).
//   - SMB 3.0 / 3.0.2: label "SMB2AESCMAC\0", context = "SmbSign\0" (the
//     preauth hash is ignored — SMB 3.0 has no preauth).
//
// Returns an error if the dialect is SMB 2.x (binding requires 3.0+) or the
// session key is empty.
func DeriveChannelSigningKey(sessionKey []byte, dialect types.Dialect, preauthHash [64]byte) ([]byte, error) {
	if dialect < types.Dialect0300 {
		return nil, fmt.Errorf("session binding requires SMB 3.0+, got dialect 0x%04x", uint16(dialect))
	}
	if len(sessionKey) == 0 {
		return nil, errors.New("empty session key")
	}
	label, context := kdf.LabelAndContext(kdf.SigningKeyPurpose, dialect, preauthHash)
	return kdf.DeriveKey(sessionKey, label, context, 128), nil
}

// CreateEncryptors creates Encryptor and Decryptor instances from the derived keys.
//
// Key direction (from CLIENT perspective):
//   - EncryptionKey = "ServerIn" = client-to-server = server uses for DECRYPTION
//   - DecryptionKey = "ServerOut" = server-to-client = server uses for ENCRYPTION
//
// So: Encryptor (outgoing) uses DecryptionKey, Decryptor (incoming) uses EncryptionKey.
func (cs *SessionCryptoState) CreateEncryptors(cipherId uint16) error {
	if cs == nil {
		return errors.New("nil SessionCryptoState")
	}
	if len(cs.DecryptionKey) == 0 || len(cs.EncryptionKey) == 0 {
		return errors.New("encryption/decryption keys not derived")
	}

	enc, err := encryption.NewEncryptor(cipherId, cs.DecryptionKey)
	if err != nil {
		return fmt.Errorf("create encryptor: %w", err)
	}

	dec, err := encryption.NewEncryptor(cipherId, cs.EncryptionKey)
	if err != nil {
		return fmt.Errorf("create decryptor: %w", err)
	}

	cs.Encryptor = enc
	cs.Decryptor = dec
	cs.CipherId = cipherId
	return nil
}

// Destroy zeros all key material for defense-in-depth.
// Should be called when the session is being destroyed.
func (cs *SessionCryptoState) Destroy() {
	if cs == nil {
		return
	}
	clear(cs.SessionKey)
	clear(cs.SigningKey)
	clear(cs.EncryptionKey)
	clear(cs.DecryptionKey)
	clear(cs.ApplicationKey)
	cs.Signer = nil
	cs.Encryptor = nil
	cs.Decryptor = nil
}

// ShouldEncrypt returns true if outgoing messages should be encrypted.
// True when EncryptData is set and an Encryptor has been created.
func (cs *SessionCryptoState) ShouldEncrypt() bool {
	return cs != nil && cs.EncryptData && cs.Encryptor != nil
}

// ShouldSign returns true if outgoing messages should be signed.
func (cs *SessionCryptoState) ShouldSign() bool {
	return cs.signingReady()
}

// ShouldVerify returns true if incoming messages should have signatures verified.
// Currently identical to ShouldSign; kept as a separate method for semantic clarity
// and to allow divergence if verification-only modes are added later.
func (cs *SessionCryptoState) ShouldVerify() bool {
	return cs.signingReady()
}

// signingReady returns true if signing state is initialized and enabled.
func (cs *SessionCryptoState) signingReady() bool {
	return cs != nil && cs.SigningEnabled && cs.Signer != nil
}

package encryption

import (
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/smb/header"
)

// ErrDecryptFailed is a sentinel error indicating SMB3 decryption failure.
// Use errors.Is(err, ErrDecryptFailed) to detect decryption errors without
// relying on string matching.
var ErrDecryptFailed = errors.New("SMB3 decryption failed")

// ErrUnknownSession is a sentinel error indicating that a transform-header
// message was received for a SessionId not present in the session table.
// Returned wrapped together with ErrDecryptFailed so existing error-matching
// continues to work; callers can use errors.Is to branch on this distinct
// case and synthesize a plaintext STATUS_USER_SESSION_DELETED response
// (MS-SMB2 §3.3.5.2.7) instead of silently dropping the message.
var ErrUnknownSession = errors.New("encrypted message for unknown session")

// ErrNoDecryptor indicates the session exists but has no derivable AEAD
// key (anonymous / guest / SMB 2.x). Encrypting requests on such a session
// is a protocol error — smbtorture's smb2.session.anon-encryption{1,2,3}
// drives exactly this path and asserts CONNECTION_RESET. Callers terminate
// the TCP connection on this error instead of treating it as a transient
// decrypt failure (which would let the client retry indefinitely).
var ErrNoDecryptor = errors.New("session has no decryptor (anonymous / guest / SMB 2.x)")

// EncryptableSession is the minimal interface for a session that supports encryption.
// This decouples the middleware from the full session.Session type to avoid circular imports.
type EncryptableSession interface {
	ShouldEncrypt() bool
	EncryptWithNonce(nonce, plaintext, aad []byte) ([]byte, error)
	DecryptMessage(nonce, ciphertext, aad []byte) ([]byte, error)
	EncryptorNonceSize() int
	DecryptorNonceSize() int
	EncryptorOverhead() int
	// IsNullSession reports whether the session was created with anonymous
	// NTLM credentials (SMB2_SESSION_FLAG_IS_NULL). Used by the framing
	// layer to map any decryption failure on such a session to
	// ErrAnonEncryption — Samba treats decrypt errors on anonymous sessions
	// as catastrophic and drops the TCP connection (smbtorture
	// smb2.session.anon-encryption3 with a forced-wrong client session key
	// expects CONNECTION_RESET, not the regular 5-failures-then-drop path).
	IsNullSession() bool
}

// EncryptionMiddleware handles transparent encryption/decryption of SMB3 messages.
//
// It wraps outgoing SMB2 responses in transform headers with AEAD encryption,
// and unwraps incoming transform-header messages by decrypting them back to
// plain SMB2 bytes.
type EncryptionMiddleware interface {
	// DecryptRequest decrypts a transform-header-wrapped message.
	// Returns the decrypted inner SMB2 bytes and the session ID from the transform header.
	DecryptRequest(transformMessage []byte) (smb2Message []byte, sessionID uint64, err error)

	// EncryptResponse encrypts an SMB2 message for the given session.
	// Returns the complete transform header + encrypted payload ready for wire transmission.
	EncryptResponse(sessionID uint64, smb2Message []byte) ([]byte, error)

	// ShouldEncrypt returns true if the given session requires encryption.
	ShouldEncrypt(sessionID uint64) bool
}

// sessionEncryptionMiddleware implements EncryptionMiddleware using session-based AEAD crypto.
type sessionEncryptionMiddleware struct {
	sessionLookup func(sessionID uint64) (EncryptableSession, bool)
}

// NewEncryptionMiddleware creates an EncryptionMiddleware that uses the provided
// session lookup function to resolve sessions for encryption/decryption.
//
// The sessionLookup function decouples the middleware from the session manager,
// allowing it to be used in both production and test contexts.
func NewEncryptionMiddleware(sessionLookup func(sessionID uint64) (EncryptableSession, bool)) EncryptionMiddleware {
	return &sessionEncryptionMiddleware{
		sessionLookup: sessionLookup,
	}
}

// DecryptRequest decrypts a transform-header-wrapped message.
//
// Wire format: TransformHeader (52 bytes) + encrypted_data (no tag).
// The AEAD authentication tag is stored in TransformHeader.Signature (16 bytes).
// To decrypt, we reconstruct ciphertextWithTag = encrypted_data + Signature,
// then call AEAD Open with the nonce from the header and AAD from header bytes 20-51.
//
// Per MS-SMB2 3.3.5.2.1.1: messages inside transform headers are NOT signed.
// AEAD provides integrity, so no signature verification is needed.
func (m *sessionEncryptionMiddleware) DecryptRequest(transformMessage []byte) ([]byte, uint64, error) {
	th, err := header.ParseTransformHeader(transformMessage)
	if err != nil {
		return nil, 0, fmt.Errorf("parse transform header: %w", err)
	}

	sess, ok := m.sessionLookup(th.SessionId)
	if !ok {
		// Return th.SessionId so the caller can build a synthetic
		// STATUS_USER_SESSION_DELETED plaintext reply. This is the common
		// race when a parallel connection has previously-session-IDed
		// the session via SESSION_SETUP.PreviousSessionId (reconnect1,
		// MS-SMB2 §3.3.5.5.1).
		return nil, th.SessionId, fmt.Errorf("session 0x%x not found for decryption: %w: %w",
			th.SessionId, ErrUnknownSession, ErrDecryptFailed)
	}

	// Anonymous / guest / SMB 2.x sessions never derive AEAD keys, so they
	// have no decryptor. An encrypted request reaching such a session is a
	// protocol violation (MS-SMB2 §3.3.5.2.1 — "If Session.SessionFlags has
	// the SMB2_SESSION_FLAG_IS_NULL or SMB2_SESSION_FLAG_IS_GUEST bit set,
	// the request MUST be rejected"). Surface a typed error so the connection
	// layer can drop the TCP connection rather than retrying decrypt
	// (smbtorture smb2.session.anon-encryption{1,2,3} asserts CONNECTION_RESET).
	nonceSize := sess.DecryptorNonceSize()
	if nonceSize == 0 {
		return nil, th.SessionId, fmt.Errorf("session 0x%x has no decryptor: %w: %w",
			th.SessionId, ErrNoDecryptor, ErrDecryptFailed)
	}

	// Extract encrypted data (everything after the 52-byte transform header)
	encryptedData := transformMessage[header.TransformHeaderSize:]

	// Reconstruct ciphertextWithTag: encrypted_data + Signature (16-byte auth tag).
	// The AEAD Seal output is ciphertext+tag. On the wire, the tag is stored in
	// the Signature field, and only the ciphertext (without tag) follows the header.
	ciphertextWithTag := make([]byte, len(encryptedData)+16)
	copy(ciphertextWithTag, encryptedData)
	copy(ciphertextWithTag[len(encryptedData):], th.Signature[:])

	// Extract nonce (first NonceSize bytes from the 16-byte Nonce field)
	nonce := make([]byte, nonceSize)
	copy(nonce, th.Nonce[:nonceSize])

	// Compute AAD from the transform header (bytes 20-51)
	aad := th.AAD()

	plaintext, err := sess.DecryptMessage(nonce, ciphertextWithTag, aad)
	if err != nil {
		// Decryption failures on anonymous (IsNull) sessions are treated as
		// catastrophic — Samba's smb2_server.c terminates the connection on
		// any non-OK status from inbuf_parse_compound. Map to ErrNoDecryptor
		// so the connection layer drops the TCP socket immediately
		// (smbtorture smb2.session.anon-encryption3, where a forced-wrong
		// client session key forces decrypt failure on the encrypted tcon).
		if sess.IsNullSession() {
			return nil, th.SessionId, fmt.Errorf("decrypt failed on anonymous session 0x%x: %w: %w",
				th.SessionId, ErrNoDecryptor, ErrDecryptFailed)
		}
		return nil, th.SessionId, fmt.Errorf("decrypt message for session 0x%x: %w: %w", th.SessionId, err, ErrDecryptFailed)
	}

	return plaintext, th.SessionId, nil
}

// EncryptResponse encrypts an SMB2 message for the given session.
//
// Per MS-SMB2 3.1.4.3 (Encrypting the Message):
//  1. Generate a fresh random nonce
//  2. Build TransformHeader with the nonce, session ID, original message size, flags
//  3. Compute AAD from header bytes 20-51 (Nonce through SessionId)
//  4. AEAD Seal(nonce, plaintext, aad) -> ciphertextWithTag
//  5. Split tag into header.Signature, ciphertext follows the header
//
// Wire format: TransformHeader (52 bytes, Signature=tag) + encrypted_data (no tag)
func (m *sessionEncryptionMiddleware) EncryptResponse(sessionID uint64, smb2Message []byte) ([]byte, error) {
	sess, ok := m.sessionLookup(sessionID)
	if !ok {
		return nil, fmt.Errorf("session 0x%x not found for encryption", sessionID)
	}

	// Build transform header (Signature is filled after encryption)
	th := &header.TransformHeader{
		OriginalMessageSize: uint32(len(smb2Message)),
		Flags:               0x0001, // Encrypted
		SessionId:           sessionID,
	}

	// Generate nonce externally so we can set it in the header before computing AAD.
	// The nonce in the TransformHeader IS the AEAD nonce, and the AAD includes the
	// nonce bytes. We must know the nonce before computing AAD.
	nonceSize := sess.EncryptorNonceSize()
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}

	// Set nonce in header (zero-padded in the 16-byte Nonce field)
	copy(th.Nonce[:], nonce)

	// Compute AAD from the header (includes the nonce we just set)
	aad := th.AAD()

	// Encrypt using EncryptWithNonce so we control the nonce
	ciphertextWithTag, err := sess.EncryptWithNonce(nonce, smb2Message, aad)
	if err != nil {
		return nil, fmt.Errorf("encrypt response for session 0x%x: %w", sessionID, err)
	}

	// Split ciphertextWithTag into ciphertext + 16-byte auth tag
	overhead := sess.EncryptorOverhead()
	if len(ciphertextWithTag) < overhead {
		return nil, fmt.Errorf("ciphertext too short: %d bytes", len(ciphertextWithTag))
	}
	ciphertext := ciphertextWithTag[:len(ciphertextWithTag)-overhead]
	tag := ciphertextWithTag[len(ciphertextWithTag)-overhead:]

	// Copy auth tag into header Signature and build wire format
	copy(th.Signature[:], tag)
	headerBytes := th.Encode()

	return append(headerBytes, ciphertext...), nil
}

func (m *sessionEncryptionMiddleware) ShouldEncrypt(sessionID uint64) bool {
	sess, ok := m.sessionLookup(sessionID)
	if !ok {
		return false
	}
	return sess.ShouldEncrypt()
}

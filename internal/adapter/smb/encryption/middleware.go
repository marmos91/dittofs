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

// EncryptableSession is the minimal interface for a session that supports encryption.
// This decouples the middleware from the full session.Session type to avoid circular imports.
type EncryptableSession interface {
	ShouldEncrypt() bool
	EncryptWithNonce(nonce, plaintext, aad []byte) ([]byte, error)
	DecryptMessage(nonce, ciphertext, aad []byte) ([]byte, error)
	EncryptorNonceSize() int
	DecryptorNonceSize() int
	EncryptorOverhead() int
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
		return nil, 0, fmt.Errorf("session 0x%x not found for decryption: %w", th.SessionId, ErrDecryptFailed)
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
	nonceSize := sess.DecryptorNonceSize()
	nonce := make([]byte, nonceSize)
	copy(nonce, th.Nonce[:nonceSize])

	// Compute AAD from the transform header (bytes 20-51)
	aad := th.AAD()

	plaintext, err := sess.DecryptMessage(nonce, ciphertextWithTag, aad)
	if err != nil {
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

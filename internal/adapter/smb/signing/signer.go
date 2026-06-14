package signing

import (
	"encoding/binary"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// Signing algorithm ID constants per [MS-SMB2] Section 2.2.3.1.7.
const (
	// SigningAlgHMACSHA256 is the HMAC-SHA256 signing algorithm (SMB 2.x default).
	SigningAlgHMACSHA256 uint16 = 0x0000
	// SigningAlgAESCMAC is the AES-128-CMAC signing algorithm (SMB 3.x default).
	SigningAlgAESCMAC uint16 = 0x0001
	// SigningAlgAESGMAC is the AES-128-GMAC signing algorithm (SMB 3.1.1 optional).
	SigningAlgAESGMAC uint16 = 0x0002
)

// Signer provides signing and verification for SMB2 messages.
// All implementations produce a 16-byte signature.
type Signer interface {
	// Sign computes the signature for an SMB2 message.
	// The signature field (bytes 48-63) is zeroed internally before computation.
	// Sign does not mutate the caller's buffer.
	Sign(message []byte) [SignatureSize]byte

	// SignInPlace computes the signature assuming the 16-byte signature field
	// (bytes 48-63) is already zeroed. It does NOT copy the message and does
	// NOT mutate it, avoiding a full-message allocation per PDU. Callers must
	// zero the signature field before calling. Used by the outbound
	// SignMessage path.
	SignInPlace(message []byte) [SignatureSize]byte

	// Verify checks if the message signature is valid.
	// Returns true if the signature is correct.
	Verify(message []byte) bool
}

// NewSigner creates the appropriate Signer for the negotiated dialect and
// signing algorithm.
//
// SigningAlgHMACSHA256 has the wire value 0 per MS-SMB2 §2.2.3.1.7, which is
// indistinguishable from an unset/default-zero signingAlgorithmId. To avoid
// accidentally returning HMACSigner for a 3.1.1 session that never explicitly
// negotiated HMAC-SHA256 (e.g. no SIGNING_CAPABILITIES context, or a default-
// zero plumbing path), HMAC-SHA256 is only selected when explicitlyNegotiated
// is true.
//
// Dispatch logic:
//   - dialect < 3.0: HMACSigner (HMAC-SHA256, the only signer for 2.x)
//   - signingAlgorithmId == SigningAlgAESGMAC: GMACSigner
//   - signingAlgorithmId == SigningAlgHMACSHA256 on 3.1.1 AND
//     explicitlyNegotiated: HMACSigner
//   - otherwise (3.0/3.0.2, or 3.1.1 with CMAC, or any non-explicit zero):
//     CMACSigner
func NewSigner(dialect types.Dialect, signingAlgorithmId uint16, explicitlyNegotiated bool, key []byte) Signer {
	if dialect < types.Dialect0300 {
		return NewHMACSigner(key)
	}
	switch signingAlgorithmId {
	case SigningAlgAESGMAC:
		return NewGMACSigner(key)
	case SigningAlgHMACSHA256:
		// Wire value 0 — ambiguous with default-zero. Only honour HMAC-SHA256
		// on 3.1.1 when the caller signals an explicit SIGNING_CAPABILITIES
		// selection. Otherwise fall through to CMAC (the 3.x default).
		if dialect >= types.Dialect0311 && explicitlyNegotiated {
			return NewHMACSigner(key)
		}
	}
	return NewCMACSigner(key)
}

// SignMessage signs an SMB2 message in place using the given Signer.
// It sets the SMB2_FLAGS_SIGNED flag (bit 3 of flags at offset 16) and
// writes the computed signature to bytes 48-63.
//
// This replaces the old SigningKey.SignMessage method and decouples the
// protocol concern (flag setting, signature placement) from the crypto concern.
func SignMessage(signer Signer, message []byte) {
	if signer == nil || len(message) < SMB2HeaderSize {
		return
	}

	// Set the signed flag (SMB2_FLAGS_SIGNED = 0x00000008)
	flags := binary.LittleEndian.Uint32(message[16:20])
	flags |= 0x00000008
	binary.LittleEndian.PutUint32(message[16:20], flags)

	// SignMessage owns the buffer here: it has just zeroed the signature
	// field, so use SignInPlace to avoid the redundant full-message copy
	// that Sign would make.
	zeroSignatureField(message)
	sig := signer.SignInPlace(message)
	copy(message[SignatureOffset:], sig[:])
}

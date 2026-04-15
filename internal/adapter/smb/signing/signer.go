package signing

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"runtime"
	"strconv"
	"strings"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
)

// goroutineID extracts the current goroutine ID from runtime.Stack output.
// TEMP debug-only helper for #362 investigation.
func goroutineID() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	prefix := strings.TrimPrefix(string(buf[:n]), "goroutine ")
	idStr := prefix[:strings.Index(prefix, " ")]
	id, _ := strconv.ParseUint(idStr, 10, 64)
	return id
}

// fingerprint returns the first 8 bytes of SHA-256(b) as hex.
func fingerprint(b []byte) string {
	if len(b) == 0 {
		return "<empty>"
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:4])
}

// keyFP returns the first 4 bytes of a key as hex (full key would leak).
func keyFP(b []byte) string {
	if len(b) < 4 {
		return "<short>"
	}
	return hex.EncodeToString(b[:4])
}

// peekHeader extracts MessageId, SessionId, Command, Status, Flags from an
// SMB2 message for tracing.
func peekHeader(message []byte) (msgID uint64, sessID uint64, cmd uint16, status uint32, flags uint32) {
	if len(message) < SMB2HeaderSize {
		return
	}
	cmd = binary.LittleEndian.Uint16(message[12:14])
	status = binary.LittleEndian.Uint32(message[8:12])
	flags = binary.LittleEndian.Uint32(message[16:20])
	msgID = binary.LittleEndian.Uint64(message[24:32])
	sessID = binary.LittleEndian.Uint64(message[40:48])
	return
}

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
	Sign(message []byte) [SignatureSize]byte

	// Verify checks if the message signature is valid.
	// Returns true if the signature is correct.
	Verify(message []byte) bool
}

// NewSigner creates the appropriate Signer for the negotiated dialect and
// signing algorithm.
//
// Dispatch logic:
//   - dialect < 3.0: HMACSigner (HMAC-SHA256)
//   - signingAlgorithmId == SigningAlgAESGMAC: GMACSigner
//   - otherwise (3.0/3.0.2, or 3.1.1 without GMAC): CMACSigner
func NewSigner(dialect types.Dialect, signingAlgorithmId uint16, key []byte) Signer {
	if dialect < types.Dialect0300 {
		return NewHMACSigner(key)
	}
	if signingAlgorithmId == SigningAlgAESGMAC {
		return NewGMACSigner(key)
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
		// TEMP #362 trace
		if signer == nil {
			msgID, sessID, cmd, _, _ := peekHeader(message)
			logger.Warn("SIGN_TRACE: signer is NIL — sending unsigned",
				"gid", goroutineID(),
				"sessionID", fmt.Sprintf("0x%x", sessID),
				"messageID", msgID,
				"command", cmd,
				"msgLen", len(message))
		}
		return
	}

	msgID, sessID, cmd, status, flagsBefore := peekHeader(message)
	payloadFPBefore := fingerprint(message[SMB2HeaderSize:])

	// Set the signed flag (SMB2_FLAGS_SIGNED = 0x00000008)
	flags := binary.LittleEndian.Uint32(message[16:20])
	flags |= 0x00000008
	binary.LittleEndian.PutUint32(message[16:20], flags)

	zeroSignatureField(message)
	sig := signer.Sign(message)
	copy(message[SignatureOffset:], sig[:])

	// TEMP #362 self-verify: sign-then-verify on the same goroutine using the
	// same Signer must succeed deterministically. If this fails, the bug is in
	// the signing primitive or in the Signer state itself (e.g., key was just
	// rotated, AEAD is racing). If it succeeds but the client still rejects,
	// the bug is in nonce derivation, header layout, or an after-the-fact
	// payload mutation downstream.
	verifyOK := signer.Verify(message)
	payloadFPAfter := fingerprint(message[SMB2HeaderSize:])
	sigHex := hex.EncodeToString(sig[:])

	if !verifyOK {
		logger.Error("SIGN_TRACE: SELF-VERIFY FAILED — sign(msg) tag does not verify on same Signer",
			"gid", goroutineID(),
			"sessionID", fmt.Sprintf("0x%x", sessID),
			"messageID", msgID,
			"command", cmd,
			"status", fmt.Sprintf("0x%x", status),
			"flagsBefore", fmt.Sprintf("0x%x", flagsBefore),
			"flagsAfter", fmt.Sprintf("0x%x", flags),
			"signature", sigHex,
			"payloadFP_before", payloadFPBefore,
			"payloadFP_after", payloadFPAfter,
			"msgLen", len(message))
	} else {
		logger.Debug("SIGN_TRACE: signed",
			"gid", goroutineID(),
			"sessionID", fmt.Sprintf("0x%x", sessID),
			"messageID", msgID,
			"command", cmd,
			"status", fmt.Sprintf("0x%x", status),
			"signature", sigHex,
			"payloadFP", payloadFPAfter,
			"msgLen", len(message))
	}
}

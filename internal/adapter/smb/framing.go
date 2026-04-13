package smb

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/pool"
	"github.com/marmos91/dittofs/internal/adapter/smb/encryption"
	"github.com/marmos91/dittofs/internal/adapter/smb/header"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/adapter/smb/v2/handlers"
	"github.com/marmos91/dittofs/internal/logger"
)

// SigningVerifier verifies SMB2 message signatures during request reading.
// This decouples the framing layer from session management.
type SigningVerifier interface {
	// VerifyRequest checks the signature of a request message.
	// Returns an error if signature verification fails, nil otherwise.
	VerifyRequest(hdr *header.SMB2Header, message []byte) error
}

// ReadRequest reads a complete SMB2 message from a connection.
//
// SMB2 messages are framed with a 4-byte NetBIOS session header containing
// the message length, followed by the SMB2 header (64 bytes) and body.
// For compound requests, remainingCompound contains the bytes after the first command.
//
// When an encrypted message (0xFD protocol ID) is detected, the encMiddleware
// is used to decrypt the inner SMB2 message transparently. Per MS-SMB2 3.3.5.2.1.1,
// messages inside transform headers are NOT signed -- AEAD provides integrity.
//
// Parameters:
//   - ctx: context for cancellation
//   - conn: the TCP connection to read from
//   - maxMsgSize: maximum allowed message size (DoS protection)
//   - readTimeout: deadline for reading the request (0 = no timeout)
//   - verifier: optional signature verifier (nil = skip verification)
//   - encMiddleware: optional encryption middleware (nil = no encryption support)
//   - handleSMB1: callback to handle SMB1 NEGOTIATE upgrade (returns error)
//
// Returns parsed header, body bytes, remaining compound bytes, whether the
// message was encrypted (received inside an SMB3 Transform Header), and error.
func ReadRequest(
	ctx context.Context,
	conn net.Conn,
	maxMsgSize int,
	readTimeout time.Duration,
	verifier SigningVerifier,
	encMiddleware encryption.EncryptionMiddleware,
	handleSMB1 func(ctx context.Context, message []byte) error,
) (*header.SMB2Header, []byte, []byte, bool, error) {
	message, err := readNetBIOSPayload(ctx, conn, maxMsgSize, readTimeout, 4)
	if err != nil {
		return nil, nil, nil, false, err
	}

	protocolID := binary.LittleEndian.Uint32(message[0:4])

	switch protocolID {
	case types.SMB1ProtocolID:
		// Legacy SMB1 negotiate - upgrade to SMB2
		if err := handleSMB1(ctx, message); err != nil {
			return nil, nil, nil, false, fmt.Errorf("handle SMB1 negotiate: %w", err)
		}
		// Read the next message non-recursively -- must be SMB2
		hdr, body, remaining, err := readSMB2Message(ctx, conn, maxMsgSize, readTimeout, verifier)
		return hdr, body, remaining, false, err

	case header.TransformProtocolID:
		// Encrypted SMB3 message (0xFD 'S' 'M' 'B')
		if encMiddleware == nil {
			return nil, nil, nil, false, fmt.Errorf("encrypted message received but encryption not configured")
		}
		decrypted, transformSessionID, err := encMiddleware.DecryptRequest(message)
		if err != nil {
			return nil, nil, nil, false, fmt.Errorf("decrypt transform message: %w", err)
		}

		// Parse the inner SMB2 message. Per MS-SMB2 3.3.5.2.1.1:
		// encrypted messages are NOT signed -- AEAD provides integrity.
		// Pass nil verifier to skip signature checks.
		hdr, body, remaining, err := parseSMB2Message(decrypted, nil, true)
		if err != nil {
			return nil, nil, nil, false, err
		}

		// Per MS-SMB2 3.3.5.2.1.1: validate that the inner SMB2 header's
		// SessionID matches the transform header's SessionId. A mismatch
		// would indicate cross-session request confusion.
		if hdr != nil && hdr.SessionID != transformSessionID {
			return nil, nil, nil, false, fmt.Errorf("session ID mismatch: transform header 0x%x vs inner SMB2 header 0x%x",
				transformSessionID, hdr.SessionID)
		}
		return hdr, body, remaining, true, nil

	default:
		// Normal SMB2 message (0xFE 'S' 'M' 'B') or unexpected protocol
		hdr, body, remaining, err := parseSMB2Message(message, verifier, true)
		return hdr, body, remaining, false, err
	}
}

// readSMB2Message reads a single SMB2 message (no SMB1 fallback).
// Used after SMB1 upgrade to avoid recursive ReadRequest calls.
func readSMB2Message(
	ctx context.Context,
	conn net.Conn,
	maxMsgSize int,
	readTimeout time.Duration,
	verifier SigningVerifier,
) (*header.SMB2Header, []byte, []byte, error) {
	message, err := readNetBIOSPayload(ctx, conn, maxMsgSize, readTimeout, header.HeaderSize)
	if err != nil {
		return nil, nil, nil, err
	}

	// Must be SMB2 after upgrade
	protocolID := binary.LittleEndian.Uint32(message[0:4])
	if protocolID != types.SMB2ProtocolID {
		return nil, nil, nil, fmt.Errorf("expected SMB2 after upgrade, got protocol 0x%x", protocolID)
	}

	return parseSMB2Message(message, verifier, false)
}

// readNetBIOSPayload reads a NetBIOS-framed message from conn.
// It handles keepalive frames transparently, validates message size bounds,
// and checks ctx for cancellation.
func readNetBIOSPayload(
	ctx context.Context,
	conn net.Conn,
	maxMsgSize int,
	readTimeout time.Duration,
	minMsgSize uint32,
) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	if readTimeout > 0 {
		if err := conn.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
			return nil, fmt.Errorf("set read deadline: %w", err)
		}
	}

	// Read NetBIOS session header (4 bytes)
	// Format: 1 byte type + 3 bytes length (big-endian)
	var nbHeader [4]byte
	var msgLen uint32
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		if _, err := io.ReadFull(conn, nbHeader[:]); err != nil {
			return nil, err
		}

		switch nbHeader[0] {
		case 0x00:
			msgLen = uint32(nbHeader[1])<<16 | uint32(nbHeader[2])<<8 | uint32(nbHeader[3])
		case 0x85:
			continue // NetBIOS keepalive
		default:
			return nil, fmt.Errorf("unsupported NetBIOS message type: 0x%02x", nbHeader[0])
		}
		break
	}

	if msgLen > uint32(maxMsgSize) {
		return nil, fmt.Errorf("SMB message too large: %d bytes (max %d)", msgLen, maxMsgSize)
	}
	if msgLen < minMsgSize {
		return nil, fmt.Errorf("SMB message too small: %d bytes (need %d)", msgLen, minMsgSize)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	message := make([]byte, msgLen)
	if _, err := io.ReadFull(conn, message); err != nil {
		return nil, fmt.Errorf("read SMB message: %w", err)
	}
	return message, nil
}

// parseSMB2Message parses an SMB2 message into header, body, and remaining
// compound bytes. If logRequest is true, it logs the parsed request details.
func parseSMB2Message(message []byte, verifier SigningVerifier, logRequest bool) (*header.SMB2Header, []byte, []byte, error) {
	if uint32(len(message)) < header.HeaderSize {
		return nil, nil, nil, fmt.Errorf("SMB2 message too small: %d bytes (need %d)", len(message), header.HeaderSize)
	}

	hdr, err := header.Parse(message[:header.HeaderSize])
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parse SMB2 header: %w", err)
	}

	if verifier != nil {
		if err := verifier.VerifyRequest(hdr, message); err != nil {
			return nil, nil, nil, err
		}
	}

	body, remainingCompound := splitCompoundBody(message, hdr)

	if logRequest {
		logger.Debug("SMB2 request",
			"command", hdr.Command.String(),
			"messageID", hdr.MessageID,
			"sessionID", fmt.Sprintf("0x%x", hdr.SessionID),
			"treeID", hdr.TreeID,
			"nextCommand", hdr.NextCommand,
			"flags", fmt.Sprintf("0x%x", hdr.Flags))
		if len(remainingCompound) > 0 {
			logger.Debug("Compound request detected",
				"remainingBytes", len(remainingCompound))
		}
	}

	return hdr, body, remainingCompound, nil
}

// splitCompoundBody extracts the body for the current command and any remaining
// compound data from a parsed SMB2 message.
func splitCompoundBody(message []byte, hdr *header.SMB2Header) (body, remaining []byte) {
	if hdr.NextCommand > 0 {
		bodyEnd := min(int(hdr.NextCommand), len(message))
		body = message[header.HeaderSize:bodyEnd]
		if int(hdr.NextCommand) < len(message) {
			remaining = message[hdr.NextCommand:]
		}
	} else {
		body = message[header.HeaderSize:]
	}
	return
}

// WriteNetBIOSFrame wraps an SMB2 payload in a NetBIOS session header and
// writes it to the writer. This is the single point for all wire writes,
// handling buffer pooling and NetBIOS framing.
//
// The writeMu mutex must be held by the caller or passed to ensure serialized writes.
//
// NetBIOS header format: Type (1 byte, 0x00) + Length (3 bytes, big-endian).
func WriteNetBIOSFrame(conn net.Conn, writeMu *LockedWriter, writeTimeout time.Duration, smbPayload []byte) error {
	writeMu.Lock()
	defer writeMu.Unlock()

	if writeTimeout > 0 {
		if err := conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
			return fmt.Errorf("set write deadline: %w", err)
		}
	}

	msgLen := len(smbPayload)
	frame := pool.Get(4 + msgLen)
	defer pool.Put(frame)

	// NetBIOS session header: type (0x00) + 3-byte big-endian length
	frame[0] = 0x00
	frame[1] = byte(msgLen >> 16)
	frame[2] = byte(msgLen >> 8)
	frame[3] = byte(msgLen)
	copy(frame[4:], smbPayload)

	_, err := conn.Write(frame)
	if err != nil {
		return fmt.Errorf("write SMB message: %w", err)
	}
	return nil
}

// sessionSigningVerifier implements SigningVerifier using the Handler's session state.
//
// Per MS-SMB2 3.3.5.2.4 — Verifying the Signature:
// The server MUST verify the signature on incoming requests when the session
// has SigningRequired set (or for 3.1.1: when any authenticated, non-guest,
// non-anonymous session exists). Encrypted messages (received inside a
// Transform Header) are NOT verified here — AEAD provides integrity.
//
// For compound requests, only the first command's bytes are verified here;
// subsequent commands are verified individually by VerifyCompoundCommandSignature.
type sessionSigningVerifier struct {
	handler    *handlers.Handler
	conn       net.Conn
	connCrypto *ConnectionCryptoState
}

// NewSessionSigningVerifier creates a SigningVerifier backed by the Handler's session
// state. It verifies message signatures per MS-SMB2 3.3.5.2.4 using session signing keys.
// The connCrypto parameter provides per-connection negotiation state (dialect, etc.).
func NewSessionSigningVerifier(handler *handlers.Handler, conn net.Conn, connCrypto *ConnectionCryptoState) SigningVerifier {
	return &sessionSigningVerifier{handler: handler, conn: conn, connCrypto: connCrypto}
}

// VerifyRequest implements MS-SMB2 3.3.5.2.4 signature verification.
//
// Verification is skipped for:
//   - SessionID == 0 (no session context)
//   - NEGOTIATE and SESSION_SETUP (signing keys not yet established)
//   - Logged-off sessions (deferred delete race; let prepareDispatch handle)
//   - Guest/null sessions on non-3.1.1 (signing not required)
//
// For SMB 3.1.1 authenticated sessions, unsigned+unencrypted requests are
// rejected per MS-SMB2 3.3.5.2.4 (implicit signing requirement). Per user
// decision: error response instead of TCP disconnect.
func (sv *sessionSigningVerifier) VerifyRequest(hdr *header.SMB2Header, message []byte) error {
	// Skip verification for messages without a session (SessionID == 0)
	// and for NEGOTIATE/SESSION_SETUP which may not have signing set up yet.
	if hdr.SessionID == 0 || hdr.Command == types.SMB2Negotiate || hdr.Command == types.SMB2SessionSetup {
		return nil
	}

	sess, ok := sv.handler.GetSession(hdr.SessionID)
	if !ok {
		return nil
	}

	// If the session has been logged off but not yet deleted (deferred delete
	// race), skip signing verification and let prepareDispatch return the
	// proper STATUS_USER_SESSION_DELETED.
	if sess.LoggedOff.Load() {
		return nil
	}

	// Skip verification for expired Kerberos sessions — let prepareDispatch
	// return STATUS_NETWORK_SESSION_EXPIRED.
	if sess.IsExpired() {
		return nil
	}

	isSigned := hdr.Flags.IsSigned()

	if sess.CryptoState != nil && sess.CryptoState.SigningRequired && !isSigned {
		logger.Warn("SMB2 message not signed but signing required",
			"command", hdr.Command.String(),
			"sessionID", hdr.SessionID,
			"client", sv.conn.RemoteAddr().String())
		return fmt.Errorf("STATUS_ACCESS_DENIED: message not signed")
	}

	// Per MS-SMB2 3.3.5.2.4: For dialect 3.1.1, if the request is not signed
	// and not encrypted (encrypted messages skip this verifier entirely), and
	// the session is authenticated (not guest/anonymous), the server MUST
	// disconnect the connection. This applies regardless of the server's
	// signing configuration because 3.1.1 implicitly requires message
	// integrity for all authenticated sessions.
	if !isSigned && sv.connCrypto != nil && sv.connCrypto.GetDialect() == types.Dialect0311 &&
		!sess.IsGuest && !sess.IsNull &&
		sess.CryptoState != nil && sess.CryptoState.ShouldVerify() {
		logger.Warn("SMB 3.1.1: unsigned unencrypted request from authenticated session, disconnecting",
			"command", hdr.Command.String(),
			"sessionID", hdr.SessionID,
			"client", sv.conn.RemoteAddr().String())
		return fmt.Errorf("SMB 3.1.1: unsigned unencrypted request requires disconnect")
	}

	if isSigned && sess.ShouldVerify() {
		// For compound requests, verify only the first command's bytes.
		verifyBytes := message
		if hdr.NextCommand > 0 && int(hdr.NextCommand) <= len(message) {
			verifyBytes = message[:hdr.NextCommand]
		}

		if !sess.VerifyMessage(verifyBytes) {
			logger.Warn("SMB2 message signature verification failed",
				"command", hdr.Command.String(),
				"sessionID", hdr.SessionID,
				"client", sv.conn.RemoteAddr().String(),
				"msgSignature", fmt.Sprintf("%x", message[48:64]))
			return fmt.Errorf("STATUS_ACCESS_DENIED: signature verification failed")
		}
		logger.Debug("Verified incoming SMB2 message signature",
			"command", hdr.Command.String(),
			"sessionID", hdr.SessionID)
	}

	return nil
}

// SendRawMessage sends pre-encoded header and body bytes with NetBIOS framing.
// Used for SMB1-to-SMB2 upgrade responses where the header is manually constructed.
func SendRawMessage(conn net.Conn, writeMu *LockedWriter, writeTimeout time.Duration, headerBytes, body []byte) error {
	return WriteNetBIOSFrame(conn, writeMu, writeTimeout, append(headerBytes, body...))
}

package smb

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/pool"
	"github.com/marmos91/dittofs/internal/adapter/smb/header"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
)

// SigningVerifier is called during request reading to verify message signatures.
// It takes the parsed header and full message bytes and returns an error if
// signature verification fails.
//
// This callback pattern decouples the framing layer from session management,
// allowing internal/ code to verify signatures without accessing Connection fields.
type SigningVerifier interface {
	// VerifyRequest checks and optionally verifies the signature of a request message.
	// Parameters:
	//   - hdr: parsed SMB2 header
	//   - message: complete message bytes (header + body)
	// Returns an error if signature verification fails, nil otherwise.
	VerifyRequest(hdr *header.SMB2Header, message []byte) error
}

// ReadRequest reads a complete SMB2 message from a connection.
//
// SMB2 messages are framed with a 4-byte NetBIOS session header containing
// the message length, followed by the SMB2 header (64 bytes) and body.
// For compound requests, remainingCompound contains the bytes after the first command.
//
// Parameters:
//   - ctx: context for cancellation
//   - conn: the TCP connection to read from
//   - maxMsgSize: maximum allowed message size (DoS protection)
//   - readTimeout: deadline for reading the request (0 = no timeout)
//   - verifier: optional signature verifier (nil = skip verification)
//   - handleSMB1: callback to handle SMB1 NEGOTIATE upgrade (returns error)
//
// Returns parsed header, body bytes, remaining compound bytes, and error.
func ReadRequest(
	ctx context.Context,
	conn net.Conn,
	maxMsgSize int,
	readTimeout time.Duration,
	verifier SigningVerifier,
	handleSMB1 func(ctx context.Context, message []byte) error,
) (*header.SMB2Header, []byte, []byte, error) {
	// Check context before starting
	select {
	case <-ctx.Done():
		return nil, nil, nil, ctx.Err()
	default:
	}

	// Apply read timeout if configured
	if readTimeout > 0 {
		deadline := time.Now().Add(readTimeout)
		if err := conn.SetReadDeadline(deadline); err != nil {
			return nil, nil, nil, fmt.Errorf("set read deadline: %w", err)
		}
	}

	// Read NetBIOS session header (4 bytes)
	// Format: 1 byte type (0x00 for session message) + 3 bytes length (big-endian)
	var nbHeader [4]byte
	if _, err := io.ReadFull(conn, nbHeader[:]); err != nil {
		return nil, nil, nil, err
	}

	// Parse NetBIOS length (24-bit big-endian)
	msgLen := uint32(nbHeader[1])<<16 | uint32(nbHeader[2])<<8 | uint32(nbHeader[3])

	// Validate message size (configurable via maxMsgSize)
	if msgLen > uint32(maxMsgSize) {
		return nil, nil, nil, fmt.Errorf("SMB message too large: %d bytes (max %d)", msgLen, maxMsgSize)
	}

	// SMB messages must be at least 4 bytes to read the protocol ID.
	const minProtocolIDSize = 4
	if msgLen < minProtocolIDSize {
		return nil, nil, nil, fmt.Errorf("SMB message too small: %d bytes", msgLen)
	}

	// Check context before reading potentially large message
	select {
	case <-ctx.Done():
		return nil, nil, nil, ctx.Err()
	default:
	}

	// Read the entire SMB message
	message := make([]byte, msgLen)
	if _, err := io.ReadFull(conn, message); err != nil {
		return nil, nil, nil, fmt.Errorf("read SMB message: %w", err)
	}

	// Check if this is SMB1 (legacy negotiate) - needs upgrade to SMB2
	protocolID := binary.LittleEndian.Uint32(message[0:4])
	if protocolID == types.SMB1ProtocolID {
		// Handle SMB1 NEGOTIATE by responding with SMB2 NEGOTIATE response
		if err := handleSMB1(ctx, message); err != nil {
			return nil, nil, nil, fmt.Errorf("handle SMB1 negotiate: %w", err)
		}
		// Read the next message which should be SMB2
		return ReadRequest(ctx, conn, maxMsgSize, readTimeout, verifier, handleSMB1)
	}

	// For SMB2, validate that we have at least a full header (64 bytes)
	if msgLen < header.HeaderSize {
		return nil, nil, nil, fmt.Errorf("SMB2 message too small: %d bytes (need %d)", msgLen, header.HeaderSize)
	}

	// Parse SMB2 header
	hdr, err := header.Parse(message[:header.HeaderSize])
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parse SMB2 header: %w", err)
	}

	// Verify message signature if a verifier is provided
	if verifier != nil {
		if err := verifier.VerifyRequest(hdr, message); err != nil {
			return nil, nil, nil, err
		}
	}

	// For compound requests, extract only the body for this command
	var body []byte
	var remainingCompound []byte
	if hdr.NextCommand > 0 {
		bodyEnd := int(hdr.NextCommand)
		if bodyEnd > len(message) {
			bodyEnd = len(message)
		}
		body = message[header.HeaderSize:bodyEnd]
		if int(hdr.NextCommand) < len(message) {
			remainingCompound = message[hdr.NextCommand:]
			logger.Debug("Compound request detected",
				"remainingBytes", len(remainingCompound))
		}
	} else {
		body = message[header.HeaderSize:]
	}

	logger.Debug("SMB2 request",
		"command", hdr.Command.String(),
		"messageId", hdr.MessageID,
		"sessionId", fmt.Sprintf("0x%x", hdr.SessionID),
		"treeId", hdr.TreeID,
		"nextCommand", hdr.NextCommand,
		"flags", fmt.Sprintf("0x%x", hdr.Flags))

	return hdr, body, remainingCompound, nil
}

// WriteNetBIOSFrame wraps an SMB2 payload in a NetBIOS session header and
// writes it to the connection. This is the single point for all wire writes,
// handling buffer pooling and NetBIOS framing.
//
// NetBIOS header format: Type (1 byte, 0x00) + Length (3 bytes, big-endian).
func WriteNetBIOSFrame(conn net.Conn, writeMu *LockedWriter, writeTimeout time.Duration, smbPayload []byte) error {
	writeMu.Lock()
	defer writeMu.Unlock()

	if writeTimeout > 0 {
		deadline := time.Now().Add(writeTimeout)
		if err := conn.SetWriteDeadline(deadline); err != nil {
			return fmt.Errorf("set write deadline: %w", err)
		}
	}

	msgLen := len(smbPayload)
	totalLen := 4 + msgLen
	frame := pool.Get(totalLen)
	defer pool.Put(frame)

	// NetBIOS session header
	frame[0] = 0x00 // Session message type
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

// SendRawMessage sends pre-encoded header and body bytes with NetBIOS framing.
// Used for SMB1-to-SMB2 upgrade responses where the header is manually constructed.
func SendRawMessage(conn net.Conn, writeMu *LockedWriter, writeTimeout time.Duration, headerBytes, body []byte) error {
	payload := make([]byte, len(headerBytes)+len(body))
	copy(payload, headerBytes)
	copy(payload[len(headerBytes):], body)

	return WriteNetBIOSFrame(conn, writeMu, writeTimeout, payload)
}

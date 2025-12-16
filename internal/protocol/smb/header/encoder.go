package header

import (
	"encoding/binary"

	"github.com/marmos91/dittofs/internal/protocol/smb/types"
)

// Encode serializes the header to wire format (little-endian).
//
// The header is encoded as a 64-byte structure following the SMB2 specification.
// All multi-byte fields use little-endian byte order.
//
// Example:
//
//	header := &SMB2Header{
//	    Command: types.CommandRead,
//	    Status:  types.StatusSuccess,
//	    Flags:   types.FlagResponse,
//	}
//	wireData := header.Encode()
//	conn.Write(wireData)
func (h *SMB2Header) Encode() []byte {
	buf := make([]byte, HeaderSize)

	// Protocol ID: 0xFE 'S' 'M' 'B' (little-endian: 0x424D53FE)
	binary.LittleEndian.PutUint32(buf[0:4], types.SMB2ProtocolID)

	// Structure size (always 64)
	binary.LittleEndian.PutUint16(buf[4:6], HeaderSize)

	// Encode typed fields with explicit conversion
	binary.LittleEndian.PutUint16(buf[6:8], h.CreditCharge)
	binary.LittleEndian.PutUint32(buf[8:12], uint32(h.Status))
	binary.LittleEndian.PutUint16(buf[12:14], uint16(h.Command))
	binary.LittleEndian.PutUint16(buf[14:16], h.Credits)
	binary.LittleEndian.PutUint32(buf[16:20], uint32(h.Flags))
	binary.LittleEndian.PutUint32(buf[20:24], h.NextCommand)
	binary.LittleEndian.PutUint64(buf[24:32], h.MessageID)
	binary.LittleEndian.PutUint32(buf[32:36], h.Reserved)
	binary.LittleEndian.PutUint32(buf[36:40], h.TreeID)
	binary.LittleEndian.PutUint64(buf[40:48], h.SessionID)
	copy(buf[48:64], h.Signature[:])

	return buf
}

// NewResponseHeader creates a response header from a request header.
//
// This is the standard way to create response headers. It:
//   - Copies the Command, MessageID, TreeID, SessionID from the request
//   - Sets the FlagResponse flag to indicate this is a server response
//   - Grants credits according to a default policy (minimum 256)
//
// The credit grant policy is conservative but client-friendly:
// clients can send multiple requests without waiting for responses.
// For more control over credits, use NewResponseHeaderWithCredits.
//
// Example:
//
//	resp := NewResponseHeader(req, types.StatusSuccess)
//	resp.Encode()
func NewResponseHeader(req *SMB2Header, status types.Status) *SMB2Header {
	// Grant generous credits to allow client to send multiple requests
	// without waiting. Typical servers grant 256+ credits.
	// See credits.go for detailed credit management documentation.
	credits := req.Credits
	if credits < 256 {
		credits = 256
	}

	return &SMB2Header{
		StructureSize: HeaderSize,
		CreditCharge:  req.CreditCharge,
		Status:        status,
		Command:       req.Command,
		Credits:       credits,
		Flags:         types.FlagResponse,
		NextCommand:   0,
		MessageID:     req.MessageID,
		Reserved:      0,
		TreeID:        req.TreeID,
		SessionID:     req.SessionID,
	}
}

// NewResponseHeaderWithCredits creates a response header with custom credit grant.
//
// Use this when you need explicit control over credit grants, such as:
//   - Throttling aggressive clients
//   - Implementing adaptive credit algorithms
//   - Testing credit exhaustion scenarios
//
// Example:
//
//	// Grant exactly 1 credit to slow down the client
//	resp := NewResponseHeaderWithCredits(req, types.StatusSuccess, 1)
func NewResponseHeaderWithCredits(req *SMB2Header, status types.Status, credits uint16) *SMB2Header {
	h := NewResponseHeader(req, status)
	h.Credits = credits
	return h
}

package header

import (
	"encoding/binary"

	"github.com/marmos91/dittofs/internal/protocol/smb/types"
)

// Encode serializes the header to wire format (little-endian)
func (h *SMB2Header) Encode() []byte {
	buf := make([]byte, HeaderSize)

	// Protocol ID
	binary.LittleEndian.PutUint32(buf[0:4], types.SMB2ProtocolID)

	// Structure size (always 64)
	binary.LittleEndian.PutUint16(buf[4:6], HeaderSize)

	binary.LittleEndian.PutUint16(buf[6:8], h.CreditCharge)
	binary.LittleEndian.PutUint32(buf[8:12], h.Status)
	binary.LittleEndian.PutUint16(buf[12:14], h.Command)
	binary.LittleEndian.PutUint16(buf[14:16], h.Credits)
	binary.LittleEndian.PutUint32(buf[16:20], h.Flags)
	binary.LittleEndian.PutUint32(buf[20:24], h.NextCommand)
	binary.LittleEndian.PutUint64(buf[24:32], h.MessageID)
	binary.LittleEndian.PutUint32(buf[32:36], h.Reserved)
	binary.LittleEndian.PutUint32(buf[36:40], h.TreeID)
	binary.LittleEndian.PutUint64(buf[40:48], h.SessionID)
	copy(buf[48:64], h.Signature[:])

	return buf
}

// NewResponseHeader creates a response header from a request header
func NewResponseHeader(req *SMB2Header, status uint32) *SMB2Header {
	// Grant generous credits to allow client to send multiple requests
	// without waiting. Typical servers grant 256+ credits.
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
		Flags:         types.SMB2FlagsServerToRedir,
		NextCommand:   0,
		MessageID:     req.MessageID,
		Reserved:      0,
		TreeID:        req.TreeID,
		SessionID:     req.SessionID,
	}
}

// NewResponseHeaderWithCredits creates a response header with custom credit grant
func NewResponseHeaderWithCredits(req *SMB2Header, status uint32, credits uint16) *SMB2Header {
	h := NewResponseHeader(req, status)
	h.Credits = credits
	return h
}

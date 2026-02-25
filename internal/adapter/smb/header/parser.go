package header

import (
	"encoding/binary"
	"errors"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// Parsing errors
var (
	// ErrInvalidProtocolID indicates the message doesn't have a valid SMB2 protocol ID.
	// SMB2 messages must start with 0xFE 'S' 'M' 'B'.
	ErrInvalidProtocolID = errors.New("invalid SMB2 protocol ID")

	// ErrMessageTooShort indicates the message is too short to contain an SMB2 header.
	// Valid SMB2 messages must be at least 64 bytes.
	ErrMessageTooShort = errors.New("message too short for SMB2 header")

	// ErrInvalidHeaderSize indicates the header structure size field is invalid.
	// The StructureSize field must always be 64.
	ErrInvalidHeaderSize = errors.New("invalid SMB2 header structure size")
)

// Parse extracts an SMB2Header from wire format (little-endian).
//
// The input data must be at least 64 bytes and start with a valid SMB2 protocol ID.
// Returns an error if the data is too short or has an invalid format.
//
// Example:
//
//	header, err := Parse(packetData)
//	if err != nil {
//	    return fmt.Errorf("invalid SMB2 header: %w", err)
//	}
//	fmt.Printf("Command: %s\n", header.CommandName())
func Parse(data []byte) (*SMB2Header, error) {
	if len(data) < HeaderSize {
		return nil, ErrMessageTooShort
	}

	// Validate protocol ID: 0xFE 'S' 'M' 'B'
	protocolID := binary.LittleEndian.Uint32(data[0:4])
	if protocolID != types.SMB2ProtocolID {
		return nil, ErrInvalidProtocolID
	}

	// Validate structure size
	structureSize := binary.LittleEndian.Uint16(data[4:6])
	if structureSize != HeaderSize {
		return nil, ErrInvalidHeaderSize
	}

	h := &SMB2Header{
		StructureSize: structureSize,
		CreditCharge:  binary.LittleEndian.Uint16(data[6:8]),
		Status:        types.Status(binary.LittleEndian.Uint32(data[8:12])),
		Command:       types.Command(binary.LittleEndian.Uint16(data[12:14])),
		Credits:       binary.LittleEndian.Uint16(data[14:16]),
		Flags:         types.HeaderFlags(binary.LittleEndian.Uint32(data[16:20])),
		NextCommand:   binary.LittleEndian.Uint32(data[20:24]),
		MessageID:     binary.LittleEndian.Uint64(data[24:32]),
		Reserved:      binary.LittleEndian.Uint32(data[32:36]),
		TreeID:        binary.LittleEndian.Uint32(data[36:40]),
		SessionID:     binary.LittleEndian.Uint64(data[40:48]),
	}

	copy(h.ProtocolID[:], data[0:4])
	copy(h.Signature[:], data[48:64])

	return h, nil
}

// IsSMB2Message checks if the data starts with a valid SMB2 protocol ID.
//
// This is a fast check that only examines the first 4 bytes.
// Use this for quick protocol detection before calling Parse.
//
// Example:
//
//	if !IsSMB2Message(data) {
//	    // Could be SMB1 or invalid data
//	    return handleLegacyOrError(data)
//	}
func IsSMB2Message(data []byte) bool {
	if len(data) < 4 {
		return false
	}
	protocolID := binary.LittleEndian.Uint32(data[0:4])
	return protocolID == types.SMB2ProtocolID
}

// IsSMB1Message checks if the data starts with a valid SMB1 protocol ID.
//
// SMB1 messages start with 0xFF 'S' 'M' 'B'. If detected, the client
// should be directed to use SMB2 via a NEGOTIATE response.
func IsSMB1Message(data []byte) bool {
	if len(data) < 4 {
		return false
	}
	protocolID := binary.LittleEndian.Uint32(data[0:4])
	return protocolID == types.SMB1ProtocolID
}

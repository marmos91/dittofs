// Package header provides SMB2 message header parsing and encoding.
// Reference: [MS-SMB2] 2.2.1
package header

import (
	"github.com/marmos91/dittofs/internal/protocol/smb/types"
)

// HeaderSize is the fixed size of SMB2 header (64 bytes)
const HeaderSize = 64

// SMB2Header represents the common SMB2 message header [MS-SMB2] 2.2.1
type SMB2Header struct {
	ProtocolID    [4]byte  // Offset 0: 0xFE 'S' 'M' 'B'
	StructureSize uint16   // Offset 4: Always 64
	CreditCharge  uint16   // Offset 6
	Status        uint32   // Offset 8: NT_STATUS (response) or ChannelSequence (request)
	Command       uint16   // Offset 12
	Credits       uint16   // Offset 14: CreditRequest/CreditResponse
	Flags         uint32   // Offset 16
	NextCommand   uint32   // Offset 20: Compound request offset
	MessageID     uint64   // Offset 24
	Reserved      uint32   // Offset 32: ProcessID for sync, high part of AsyncID for async
	TreeID        uint32   // Offset 36
	SessionID     uint64   // Offset 40
	Signature     [16]byte // Offset 48
}

// IsResponse returns true if this is a response header
func (h *SMB2Header) IsResponse() bool {
	return h.Flags&types.SMB2FlagsServerToRedir != 0
}

// IsAsync returns true if this is an async message
func (h *SMB2Header) IsAsync() bool {
	return h.Flags&types.SMB2FlagsAsyncCommand != 0
}

// IsSigned returns true if the message is signed
func (h *SMB2Header) IsSigned() bool {
	return h.Flags&types.SMB2FlagsSigned != 0
}

// IsRelated returns true if this is a related operation (compound)
func (h *SMB2Header) IsRelated() bool {
	return h.Flags&types.SMB2FlagsRelatedOps != 0
}

// CommandName returns the string name of the command
func (h *SMB2Header) CommandName() string {
	return types.CommandName(h.Command)
}

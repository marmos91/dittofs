// Package header provides SMB2 message header parsing and encoding.
//
// The SMB2 header is a 64-byte structure that prefixes every SMB2 message.
// It contains metadata about the message including the command type, session
// information, and sequencing data.
//
// # Header Structure (64 bytes)
//
// The header layout is:
//
//	┌─────────────────────────────────────────────────────────────────────┐
//	│ Offset │ Size │ Field           │ Description                       │
//	├────────┼──────┼─────────────────┼───────────────────────────────────┤
//	│   0    │  4   │ ProtocolID      │ 0xFE 'S' 'M' 'B' (0x424D53FE LE)  │
//	│   4    │  2   │ StructureSize   │ Always 64                         │
//	│   6    │  2   │ CreditCharge    │ Credits consumed by this request  │
//	│   8    │  4   │ Status          │ NT_STATUS (response only)         │
//	│  12    │  2   │ Command         │ SMB2 command code                 │
//	│  14    │  2   │ Credits         │ Credits requested/granted         │
//	│  16    │  4   │ Flags           │ Header flags                      │
//	│  20    │  4   │ NextCommand     │ Offset to next command (compound) │
//	│  24    │  8   │ MessageID       │ Unique message identifier         │
//	│  32    │  4   │ Reserved        │ Reserved (ProcessID in sync)      │
//	│  36    │  4   │ TreeID          │ Tree connection identifier        │
//	│  40    │  8   │ SessionID       │ Session identifier                │
//	│  48    │ 16   │ Signature       │ Message signature (if signed)     │
//	└────────┴──────┴─────────────────┴───────────────────────────────────┘
//
// # Message Flow
//
// A typical SMB2 session follows this flow:
//
//  1. Client: NEGOTIATE (establish dialect, capabilities)
//  2. Server: NEGOTIATE response
//  3. Client: SESSION_SETUP (authenticate via NTLM/Kerberos)
//  4. Server: SESSION_SETUP response (may require multiple round trips)
//  5. Client: TREE_CONNECT (connect to share)
//  6. Server: TREE_CONNECT response
//  7. Client/Server: File operations (CREATE, READ, WRITE, etc.)
//  8. Client: TREE_DISCONNECT, LOGOFF
//
// # Credits System
//
// SMB2 uses a credit-based flow control system. Credits limit how many
// requests a client can have outstanding simultaneously.
//
// See the credits.go file for detailed documentation on how credits work
// and production considerations.
//
// Reference: [MS-SMB2] Section 2.2.1
package header

import (
	"github.com/marmos91/dittofs/internal/protocol/smb/types"
)

// HeaderSize is the fixed size of SMB2 header (64 bytes).
// All SMB2 headers are exactly this size.
const HeaderSize = 64

// SMB2Header represents the common SMB2 message header.
//
// This structure is used for both requests and responses.
// Some fields have different meanings based on context:
//   - Status: Contains NT_STATUS in responses, ChannelSequence in requests
//   - Credits: Contains CreditRequest in requests, CreditResponse in responses
//   - Reserved: Contains ProcessID in sync requests, high AsyncID in async
//
// [MS-SMB2] Section 2.2.1
type SMB2Header struct {
	// ProtocolID contains 0xFE 'S' 'M' 'B' (little-endian: 0x424D53FE)
	ProtocolID [4]byte

	// StructureSize is always 64 for SMB2 headers
	StructureSize uint16

	// CreditCharge indicates how many credits this operation consumes.
	// For large read/write operations, this may be > 1.
	// See [MS-SMB2] 3.2.4.1.5 for credit charge calculation.
	CreditCharge uint16

	// Status contains the NT_STATUS code in responses.
	// In requests, this field contains ChannelSequence/Reserved.
	Status types.Status

	// Command identifies the SMB2 operation.
	Command types.Command

	// Credits contains CreditRequest (in requests) or CreditResponse (in responses).
	// Clients request credits; servers grant them.
	Credits uint16

	// Flags contains header flags (response, async, signed, related, etc.)
	Flags types.HeaderFlags

	// NextCommand contains the offset to the next command in a compound request.
	// Zero if this is the last (or only) command.
	NextCommand uint32

	// MessageID is a unique identifier for this message.
	// Used to match responses to requests and for ordering.
	MessageID uint64

	// Reserved contains ProcessID for synchronous requests.
	// For async responses, this contains the high 32 bits of AsyncID.
	Reserved uint32

	// TreeID identifies the tree connection (share) for this operation.
	// Set by TREE_CONNECT response, used in subsequent operations.
	TreeID uint32

	// SessionID identifies the session for this operation.
	// Set by SESSION_SETUP response, used in subsequent operations.
	SessionID uint64

	// Signature contains the message signature if FlagSigned is set.
	// Used for message integrity verification.
	Signature [16]byte
}

// IsResponse returns true if this is a response header.
func (h *SMB2Header) IsResponse() bool {
	return h.Flags.IsResponse()
}

// IsAsync returns true if this is an async message.
// Async messages use AsyncID instead of TreeID/ProcessID.
func (h *SMB2Header) IsAsync() bool {
	return h.Flags.IsAsync()
}

// IsSigned returns true if the message is signed.
// The Signature field contains a valid signature.
func (h *SMB2Header) IsSigned() bool {
	return h.Flags.IsSigned()
}

// IsRelated returns true if this is a related operation (compound).
// Related operations use the FileId from the previous operation.
func (h *SMB2Header) IsRelated() bool {
	return h.Flags.IsRelated()
}

// CommandName returns the string name of the command.
func (h *SMB2Header) CommandName() string {
	return h.Command.String()
}

// StatusName returns the string name of the status code.
func (h *SMB2Header) StatusName() string {
	return h.Status.String()
}

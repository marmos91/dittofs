// Package header provides SMB2 message header parsing and encoding.
//
// # Overview
//
// This package handles the 64-byte SMB2 header that prefixes every SMB2 message.
// The header contains metadata about the message including command type, session
// information, and sequencing data.
//
// # Header Structure
//
// The SMB2 header is exactly 64 bytes:
//
//	Offset  Size  Field           Description
//	------  ----  --------------  ----------------------------------
//	0       4     ProtocolID      Magic: 0xFE 'S' 'M' 'B' (0x424D53FE LE)
//	4       2     StructureSize   Always 64
//	6       2     CreditCharge    Credits consumed by this request
//	8       4     Status          NT_STATUS (responses only)
//	12      2     Command         SMB2 command code
//	14      2     Credits         Credits requested/granted
//	16      4     Flags           Header flags
//	20      4     NextCommand     Offset to next command (compound)
//	24      8     MessageID       Unique request identifier
//	32      4     Reserved        ProcessID (sync) or AsyncID high
//	36      4     TreeID          Share connection identifier
//	40      8     SessionID       Session identifier
//	48      16    Signature       Message signature (if signed)
//
// # Byte Order
//
// All fields are little-endian, unlike NFS which uses big-endian XDR encoding.
//
// # Key Types
//
//   - SMB2Header: The parsed 64-byte header structure
//   - HeaderSize: Constant 64 (bytes)
//
// # Parsing Flow
//
//	// Read raw bytes
//	headerBytes := make([]byte, header.HeaderSize)
//	io.ReadFull(conn, headerBytes)
//
//	// Parse header
//	hdr, err := header.Parse(headerBytes)
//
//	// Access fields
//	if hdr.IsResponse() { ... }
//	command := hdr.Command
//	sessionID := hdr.SessionID
//
// # Encoding Flow
//
//	// Create response header
//	respHdr := &header.SMB2Header{
//	    ProtocolID:    [4]byte{0xFE, 'S', 'M', 'B'},
//	    StructureSize: 64,
//	    Command:       types.SMB2Read,
//	    Status:        types.StatusSuccess,
//	    SessionID:     sessionID,
//	    // ...
//	}
//
//	// Encode to bytes
//	headerBytes := header.Encode(respHdr)
//
// # Flags
//
// The Flags field is a bitmask:
//
//   - FlagResponse (0x00000001): Message is a response
//   - FlagAsync (0x00000002): Async operation
//   - FlagRelated (0x00000004): Related compound request
//   - FlagSigned (0x00000008): Message is signed
//
// # Thread Safety
//
// Parsing and encoding operations are stateless and thread-safe.
// The SMB2Header struct should not be modified concurrently.
//
// # References
//
//   - [MS-SMB2] Section 2.2.1 - SMB2 Packet Header
package header

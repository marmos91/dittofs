// Package rpc implements DCE/RPC protocol for SMB named pipes.
//
// This file implements the SRVSVC (Server Service) RPC interface for share enumeration.
//
// Reference: [MS-SRVS] Server Service Remote Protocol
package rpc

import (
	"encoding/binary"

	"github.com/marmos91/dittofs/internal/logger"
)

// =============================================================================
// SRVSVC Constants
// =============================================================================

// SRVSVC interface UUID: 4b324fc8-1670-01d3-1278-5a47bf6ee188
var SRVSVCInterfaceUUID = [16]byte{
	0xc8, 0x4f, 0x32, 0x4b, // 4b324fc8
	0x70, 0x16,             // 1670
	0xd3, 0x01,             // 01d3
	0x12, 0x78,             // 1278
	0x5a, 0x47, 0xbf, 0x6e, 0xe1, 0x88, // 5a47bf6ee188
}

// NDR Transfer Syntax UUID: 8a885d04-1ceb-11c9-9fe8-08002b104860
var NDRTransferSyntaxUUID = [16]byte{
	0x04, 0x5d, 0x88, 0x8a, // 8a885d04
	0xeb, 0x1c,             // 1ceb
	0xc9, 0x11,             // 11c9
	0x9f, 0xe8,             // 9fe8
	0x08, 0x00, 0x2b, 0x10, 0x48, 0x60, // 08002b104860
}

// SRVSVC Operation Numbers [MS-SRVS Section 3.1.4]
const (
	OpNetrShareEnum        uint16 = 15 // NetrShareEnum
	OpNetrShareGetInfo     uint16 = 16 // NetrShareGetInfo
	OpNetrServerGetInfo    uint16 = 21 // NetrServerGetInfo
)

// Share Types [MS-SRVS Section 2.2.2.4]
const (
	STYPE_DISKTREE  uint32 = 0x00000000 // Disk drive
	STYPE_PRINTQ    uint32 = 0x00000001 // Print queue
	STYPE_DEVICE    uint32 = 0x00000002 // Communication device
	STYPE_IPC       uint32 = 0x00000003 // IPC
	STYPE_SPECIAL   uint32 = 0x80000000 // Special share (ADMIN$, IPC$, etc.)
	STYPE_TEMPORARY uint32 = 0x40000000 // Temporary share
)

// Status Codes
const (
	NERR_Success       uint32 = 0x00000000
	ERROR_MORE_DATA    uint32 = 0x000000EA
	ERROR_ACCESS_DENIED uint32 = 0x00000005
)

// =============================================================================
// Share Information Structures
// =============================================================================

// ShareInfo1 represents SHARE_INFO_1 structure [MS-SRVS Section 2.2.4.23]
type ShareInfo1 struct {
	Name    string
	Type    uint32
	Comment string
}

// =============================================================================
// SRVSVC Handler
// =============================================================================

// SRVSVCHandler handles SRVSVC RPC calls
type SRVSVCHandler struct {
	shares []ShareInfo1
}

// NewSRVSVCHandler creates a new SRVSVC handler with the given shares
func NewSRVSVCHandler(shares []ShareInfo1) *SRVSVCHandler {
	return &SRVSVCHandler{shares: shares}
}

// HandleBind processes a BIND request and returns a BIND_ACK
func (h *SRVSVCHandler) HandleBind(req *BindRequest) []byte {
	// Get the transfer syntax offered by the client (or use default)
	transferSyntax := SyntaxID{UUID: NDRTransferSyntaxUUID, Version: 0x00000002}

	if len(req.ContextList) > 0 && len(req.ContextList[0].TransferSyntaxes) > 0 {
		// Use the transfer syntax offered by the client
		transferSyntax = req.ContextList[0].TransferSyntaxes[0]
	}

	// Build bind ack response
	ack := &BindAck{
		MaxXmitFrag:  req.MaxXmitFrag,
		MaxRecvFrag:  req.MaxRecvFrag,
		AssocGroupID: 0x12345678, // Arbitrary group ID
		SecAddr:      "\\PIPE\\srvsvc",
		NumResults:   1,
		Results: []ContextResult{
			{
				Result:         0, // Acceptance
				Reason:         0,
				TransferSyntax: transferSyntax,
			},
		},
	}

	return ack.Encode(req.Header.CallID)
}

// HandleRequest processes an RPC request and returns a response
func (h *SRVSVCHandler) HandleRequest(req *Request) []byte {
	switch req.OpNum {
	case OpNetrShareEnum:
		return h.handleNetrShareEnum(req)
	default:
		// Return fault for unsupported operations
		return h.buildFault(req.Header.CallID, 0x1C010003) // nca_op_rng_error
	}
}

// handleNetrShareEnum handles NetrShareEnum (opnum 15) [MS-SRVS Section 3.1.4.8]
func (h *SRVSVCHandler) handleNetrShareEnum(req *Request) []byte {
	// Parse request to get info level
	level := uint32(1) // Default to level 1
	if len(req.StubData) >= 8 {
		// Skip server name pointer (4 bytes) and read level
		level = binary.LittleEndian.Uint32(req.StubData[4:8])
	}

	// Build response stub data based on level
	var stubData []byte
	switch level {
	case 1:
		stubData = h.buildShareEnumLevel1Response()
	default:
		stubData = h.buildShareEnumLevel1Response() // Fallback to level 1
	}

	resp := &Response{
		AllocHint:   uint32(len(stubData)),
		ContextID:   req.ContextID,
		CancelCount: 0,
		StubData:    stubData,
	}

	return resp.Encode(req.Header.CallID)
}

// buildShareEnumLevel1Response builds the NDR-encoded response for level 1 share enum
func (h *SRVSVCHandler) buildShareEnumLevel1Response() []byte {
	// Response structure for NetrShareEnum with level 1:
	// - Level (4 bytes)
	// - ShareInfo switch (4 bytes) - discriminant
	// - SHARE_INFO_1_CONTAINER pointer (4 bytes)
	// - EntriesRead (4 bytes)
	// - Buffer pointer (4 bytes)
	// - Conformant array max count (4 bytes) - only if numShares > 0
	// - Array of SHARE_INFO_1 entries
	// - TotalEntries (4 bytes)
	// - ResumeHandle pointer (4 bytes)
	// - Return status (4 bytes)

	numShares := len(h.shares)

	logger.Debug("Building share enum response", "numShares", numShares)

	// Calculate buffer size more conservatively
	// Use a dynamic buffer to avoid index out of bounds
	buf := make([]byte, 0, 1024)

	// Level = 1
	buf = appendUint32(buf, 1)

	// Switch value = 1 (for level 1)
	buf = appendUint32(buf, 1)

	// SHARE_INFO_1_CONTAINER pointer (non-null)
	buf = appendUint32(buf, 0x00020000)

	// EntriesRead
	buf = appendUint32(buf, uint32(numShares))

	// Buffer pointer (non-null if entries > 0)
	if numShares > 0 {
		buf = appendUint32(buf, 0x00020004)
	} else {
		buf = appendUint32(buf, 0)
	}

	// Only include conformant array and entries if there are shares
	if numShares > 0 {
		// Conformant array max count
		buf = appendUint32(buf, uint32(numShares))

		// Array of SHARE_INFO_1 structures (fixed-size parts)
		ptrValue := uint32(0x00020008)
		for i, share := range h.shares {
			// Name pointer
			buf = appendUint32(buf, ptrValue+uint32(i*8))
			// Type
			buf = appendUint32(buf, share.Type)
			// Comment pointer
			buf = appendUint32(buf, ptrValue+uint32(i*8)+4)
		}

		// String data with conformant array headers
		for _, share := range h.shares {
			// Name string: MaxCount, Offset, ActualCount, Data
			nameLen := len(share.Name) + 1 // Include null
			buf = appendUint32(buf, uint32(nameLen))
			buf = appendUint32(buf, 0) // Offset
			buf = appendUint32(buf, uint32(nameLen))

			// Copy UTF-16LE name
			nameUTF16 := encodeUTF16LE(share.Name)
			buf = append(buf, nameUTF16...)
			buf = append(buf, 0, 0) // Null terminator

			// Pad to 4-byte boundary
			for len(buf)%4 != 0 {
				buf = append(buf, 0)
			}

			// Comment string: MaxCount, Offset, ActualCount, Data
			commentLen := len(share.Comment) + 1
			buf = appendUint32(buf, uint32(commentLen))
			buf = appendUint32(buf, 0) // Offset
			buf = appendUint32(buf, uint32(commentLen))

			// Copy UTF-16LE comment
			commentUTF16 := encodeUTF16LE(share.Comment)
			buf = append(buf, commentUTF16...)
			buf = append(buf, 0, 0) // Null terminator

			// Pad to 4-byte boundary
			for len(buf)%4 != 0 {
				buf = append(buf, 0)
			}
		}
	}

	// TotalEntries
	buf = appendUint32(buf, uint32(numShares))

	// ResumeHandle pointer (null)
	buf = appendUint32(buf, 0)

	// Return status (NERR_Success)
	buf = appendUint32(buf, NERR_Success)

	return buf
}

// appendUint32 appends a little-endian uint32 to the buffer
func appendUint32(buf []byte, v uint32) []byte {
	return append(buf,
		byte(v),
		byte(v>>8),
		byte(v>>16),
		byte(v>>24),
	)
}

// buildFault builds a DCE/RPC fault response
func (h *SRVSVCHandler) buildFault(callID uint32, status uint32) []byte {
	// Fault PDU: header + alloc_hint(4) + context_id(2) + cancel_count(1) + reserved(1) + status(4) + reserved(4)
	fragLen := HeaderSize + 16

	hdr := Header{
		VersionMajor: 5,
		VersionMinor: 0,
		PacketType:   PDUFault,
		Flags:        FlagFirstFrag | FlagLastFrag,
		DataRep:      [4]byte{0x10, 0x00, 0x00, 0x00},
		FragLength:   uint16(fragLen),
		AuthLength:   0,
		CallID:       callID,
	}

	buf := make([]byte, fragLen)
	copy(buf[0:16], hdr.Encode())
	binary.LittleEndian.PutUint32(buf[16:20], 0)      // alloc_hint
	binary.LittleEndian.PutUint16(buf[20:22], 0)      // context_id
	buf[22] = 0                                        // cancel_count
	buf[23] = 0                                        // reserved
	binary.LittleEndian.PutUint32(buf[24:28], status) // status
	binary.LittleEndian.PutUint32(buf[28:32], 0)      // reserved

	return buf
}

// encodeUTF16LE encodes a string as UTF-16LE
func encodeUTF16LE(s string) []byte {
	result := make([]byte, len(s)*2)
	for i, r := range s {
		binary.LittleEndian.PutUint16(result[i*2:], uint16(r))
	}
	return result
}

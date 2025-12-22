// Package rpc implements DCE/RPC protocol for SMB named pipes.
//
// DCE/RPC (Distributed Computing Environment / Remote Procedure Calls) is the
// protocol used over SMB named pipes for services like SRVSVC (share enumeration).
//
// Reference: [MS-RPCE] Remote Procedure Call Protocol Extensions
// Reference: [C706] DCE 1.1: Remote Procedure Call
package rpc

import (
	"encoding/binary"
	"fmt"
)

// =============================================================================
// DCE/RPC Constants
// =============================================================================

// PDU Types [C706 Section 12.6.4.14]
const (
	PDURequest       uint8 = 0  // Request PDU
	PDUPing          uint8 = 1  // Ping PDU (connectionless)
	PDUResponse      uint8 = 2  // Response PDU
	PDUFault         uint8 = 3  // Fault PDU
	PDUWorking       uint8 = 4  // Working PDU (connectionless)
	PDUNoCall        uint8 = 5  // Nocall PDU (connectionless)
	PDUReject        uint8 = 6  // Reject PDU (connectionless)
	PDUAck           uint8 = 7  // Ack PDU (connectionless)
	PDUClCancel      uint8 = 8  // Cl_cancel PDU (connectionless)
	PDUFack          uint8 = 9  // Fack PDU (connectionless)
	PDUCancelAck     uint8 = 10 // Cancel_ack PDU (connectionless)
	PDUBind          uint8 = 11 // Bind PDU
	PDUBindAck       uint8 = 12 // Bind_ack PDU
	PDUBindNak       uint8 = 13 // Bind_nak PDU
	PDUAlterContext  uint8 = 14 // Alter_context PDU
	PDUAlterContextR uint8 = 15 // Alter_context_resp PDU
	PDUShutdown      uint8 = 16 // Shutdown PDU
	PDUCoCancel      uint8 = 18 // Co_cancel PDU
	PDUOrphaned      uint8 = 19 // Orphaned PDU
)

// PDU Flags [C706 Section 12.6.3.1]
const (
	FlagFirstFrag  uint8 = 0x01 // First fragment
	FlagLastFrag   uint8 = 0x02 // Last fragment
	FlagPending    uint8 = 0x04 // Cancel pending
	FlagConcMpx    uint8 = 0x10 // Concurrent multiplexing
	FlagDidNotExec uint8 = 0x20 // Did not execute
	FlagMaybe      uint8 = 0x40 // Maybe semantics
	FlagObjectUUID uint8 = 0x80 // Object UUID present
)

// HeaderSize is the size of the common DCE/RPC header
const HeaderSize = 16

// =============================================================================
// DCE/RPC Header
// =============================================================================

// Header represents the common DCE/RPC PDU header [C706 Section 12.6.3.1]
//
// All connection-oriented PDUs begin with this 16-byte header:
//
//	Offset  Size  Field
//	0       1     rpc_vers (5)
//	1       1     rpc_vers_minor (0 or 1)
//	2       1     ptype (PDU type)
//	3       1     pfc_flags (flags)
//	4       4     packed_drep (data representation)
//	8       2     frag_length (total fragment length)
//	10      2     auth_length (auth verifier length)
//	12      4     call_id (call identifier)
type Header struct {
	VersionMajor uint8   // RPC major version (5)
	VersionMinor uint8   // RPC minor version (0 or 1)
	PacketType   uint8   // PDU type
	Flags        uint8   // PDU flags
	DataRep      [4]byte // NDR data representation
	FragLength   uint16  // Total fragment length including header
	AuthLength   uint16  // Authentication verifier length
	CallID       uint32  // Call identifier
}

// ParseHeader parses a DCE/RPC header from bytes
func ParseHeader(data []byte) (*Header, error) {
	if len(data) < HeaderSize {
		return nil, fmt.Errorf("data too short for DCE/RPC header: %d bytes", len(data))
	}

	h := &Header{
		VersionMajor: data[0],
		VersionMinor: data[1],
		PacketType:   data[2],
		Flags:        data[3],
		FragLength:   binary.LittleEndian.Uint16(data[8:10]),
		AuthLength:   binary.LittleEndian.Uint16(data[10:12]),
		CallID:       binary.LittleEndian.Uint32(data[12:16]),
	}
	copy(h.DataRep[:], data[4:8])

	return h, nil
}

// Encode serializes the header to bytes
func (h *Header) Encode() []byte {
	buf := make([]byte, HeaderSize)
	buf[0] = h.VersionMajor
	buf[1] = h.VersionMinor
	buf[2] = h.PacketType
	buf[3] = h.Flags
	copy(buf[4:8], h.DataRep[:])
	binary.LittleEndian.PutUint16(buf[8:10], h.FragLength)
	binary.LittleEndian.PutUint16(buf[10:12], h.AuthLength)
	binary.LittleEndian.PutUint32(buf[12:16], h.CallID)
	return buf
}

// =============================================================================
// Bind PDU
// =============================================================================

// BindRequest represents a DCE/RPC Bind PDU [C706 Section 12.6.4.3]
type BindRequest struct {
	Header       Header
	MaxXmitFrag  uint16 // Max transmit fragment size
	MaxRecvFrag  uint16 // Max receive fragment size
	AssocGroupID uint32 // Association group ID (0 = new)
	NumContexts  uint8  // Number of presentation contexts
	ContextList  []PresentationContext
}

// PresentationContext represents a presentation context in Bind PDU
type PresentationContext struct {
	ContextID         uint16
	NumTransferSyntax uint8
	AbstractSyntax    SyntaxID
	TransferSyntaxes  []SyntaxID
}

// SyntaxID represents a UUID + version
type SyntaxID struct {
	UUID    [16]byte
	Version uint32
}

// ParseBindRequest parses a Bind PDU
func ParseBindRequest(data []byte) (*BindRequest, error) {
	if len(data) < HeaderSize+9 {
		return nil, fmt.Errorf("bind request too short")
	}

	hdr, err := ParseHeader(data)
	if err != nil {
		return nil, err
	}

	if hdr.PacketType != PDUBind {
		return nil, fmt.Errorf("not a bind PDU: type %d", hdr.PacketType)
	}

	req := &BindRequest{
		Header:       *hdr,
		MaxXmitFrag:  binary.LittleEndian.Uint16(data[16:18]),
		MaxRecvFrag:  binary.LittleEndian.Uint16(data[18:20]),
		AssocGroupID: binary.LittleEndian.Uint32(data[20:24]),
		NumContexts:  data[24],
	}

	// Parse presentation contexts (simplified - just get first one)
	if len(data) >= 72 && req.NumContexts > 0 {
		ctx := PresentationContext{
			ContextID:         binary.LittleEndian.Uint16(data[28:30]),
			NumTransferSyntax: data[30],
		}
		// Abstract syntax UUID at offset 32
		copy(ctx.AbstractSyntax.UUID[:], data[32:48])
		ctx.AbstractSyntax.Version = binary.LittleEndian.Uint32(data[48:52])

		// Parse first transfer syntax at offset 52
		if ctx.NumTransferSyntax > 0 {
			var transferSyntax SyntaxID
			copy(transferSyntax.UUID[:], data[52:68])
			transferSyntax.Version = binary.LittleEndian.Uint32(data[68:72])
			ctx.TransferSyntaxes = append(ctx.TransferSyntaxes, transferSyntax)
		}

		req.ContextList = append(req.ContextList, ctx)
	}

	return req, nil
}

// =============================================================================
// Bind Ack PDU
// =============================================================================

// BindAck represents a DCE/RPC Bind Ack PDU [C706 Section 12.6.4.4]
type BindAck struct {
	MaxXmitFrag  uint16
	MaxRecvFrag  uint16
	AssocGroupID uint32
	SecAddr      string // Secondary address (e.g., "\PIPE\srvsvc")
	NumResults   uint8
	Results      []ContextResult
}

// ContextResult represents the result of a presentation context negotiation
type ContextResult struct {
	Result         uint16   // 0 = acceptance
	Reason         uint16   // Rejection reason if Result != 0
	TransferSyntax SyntaxID // Negotiated transfer syntax
}

// Encode serializes a Bind Ack PDU
func (ba *BindAck) Encode(callID uint32) []byte {
	// Calculate sizes
	secAddrLen := len(ba.SecAddr) + 1 // Include null terminator

	// Calculate padding to align to 4-byte boundary
	// After header(16) + max_xmit(2) + max_recv(2) + assoc_group(4) + sec_len(2) + sec_addr(secAddrLen)
	// = 26 + secAddrLen
	offsetAfterSecAddr := 26 + secAddrLen
	secAddrPadding := (4 - (offsetAfterSecAddr % 4)) % 4

	// Results: 24 bytes per result (2 + 2 + 16 + 4)
	resultsLen := len(ba.Results) * 24

	// Total body size after header
	bodySize := 8 + // max_xmit_frag(2) + max_recv_frag(2) + assoc_group_id(4)
		2 + secAddrLen + secAddrPadding + // sec_addr length + data + padding
		4 + resultsLen // num_results(4) + results

	fragLen := HeaderSize + bodySize

	// Build header
	hdr := Header{
		VersionMajor: 5,
		VersionMinor: 0,
		PacketType:   PDUBindAck,
		Flags:        FlagFirstFrag | FlagLastFrag,
		DataRep:      [4]byte{0x10, 0x00, 0x00, 0x00}, // Little-endian, ASCII, IEEE float
		FragLength:   uint16(fragLen),
		AuthLength:   0,
		CallID:       callID,
	}

	buf := make([]byte, fragLen)
	copy(buf[0:16], hdr.Encode())

	offset := 16
	binary.LittleEndian.PutUint16(buf[offset:], ba.MaxXmitFrag)
	offset += 2
	binary.LittleEndian.PutUint16(buf[offset:], ba.MaxRecvFrag)
	offset += 2
	binary.LittleEndian.PutUint32(buf[offset:], ba.AssocGroupID)
	offset += 4

	// Secondary address
	binary.LittleEndian.PutUint16(buf[offset:], uint16(secAddrLen))
	offset += 2
	copy(buf[offset:], ba.SecAddr)
	offset += secAddrLen + secAddrPadding

	// Results
	buf[offset] = ba.NumResults
	offset += 4 // num_results(1) + reserved(3)

	for _, r := range ba.Results {
		binary.LittleEndian.PutUint16(buf[offset:], r.Result)
		offset += 2
		binary.LittleEndian.PutUint16(buf[offset:], r.Reason)
		offset += 2
		copy(buf[offset:], r.TransferSyntax.UUID[:])
		offset += 16
		binary.LittleEndian.PutUint32(buf[offset:], r.TransferSyntax.Version)
		offset += 4
	}

	return buf
}

// =============================================================================
// Request PDU
// =============================================================================

// Request represents a DCE/RPC Request PDU [C706 Section 12.6.4.9]
type Request struct {
	Header    Header
	AllocHint uint32 // Suggested buffer size
	ContextID uint16 // Presentation context ID
	OpNum     uint16 // Operation number
	StubData  []byte // Request body
}

// ParseRequest parses a Request PDU
func ParseRequest(data []byte) (*Request, error) {
	if len(data) < HeaderSize+8 {
		return nil, fmt.Errorf("request PDU too short")
	}

	hdr, err := ParseHeader(data)
	if err != nil {
		return nil, err
	}

	if hdr.PacketType != PDURequest {
		return nil, fmt.Errorf("not a request PDU: type %d", hdr.PacketType)
	}

	req := &Request{
		Header:    *hdr,
		AllocHint: binary.LittleEndian.Uint32(data[16:20]),
		ContextID: binary.LittleEndian.Uint16(data[20:22]),
		OpNum:     binary.LittleEndian.Uint16(data[22:24]),
	}

	// Stub data starts at offset 24 and extends to FragLength - AuthLength
	stubEnd := int(hdr.FragLength) - int(hdr.AuthLength)
	if stubEnd > 24 && stubEnd <= len(data) {
		req.StubData = data[24:stubEnd]
	}

	return req, nil
}

// =============================================================================
// Response PDU
// =============================================================================

// Response represents a DCE/RPC Response PDU [C706 Section 12.6.4.10]
type Response struct {
	AllocHint   uint32
	ContextID   uint16
	CancelCount uint8
	StubData    []byte
}

// Encode serializes a Response PDU
func (r *Response) Encode(callID uint32) []byte {
	// Calculate fragment length
	// Header (16) + alloc_hint(4) + context_id(2) + cancel_count(1) + reserved(1) + stub_data
	fragLen := HeaderSize + 8 + len(r.StubData)

	hdr := Header{
		VersionMajor: 5,
		VersionMinor: 0,
		PacketType:   PDUResponse,
		Flags:        FlagFirstFrag | FlagLastFrag,
		DataRep:      [4]byte{0x10, 0x00, 0x00, 0x00},
		FragLength:   uint16(fragLen),
		AuthLength:   0,
		CallID:       callID,
	}

	buf := make([]byte, fragLen)
	copy(buf[0:16], hdr.Encode())

	binary.LittleEndian.PutUint32(buf[16:20], r.AllocHint)
	binary.LittleEndian.PutUint16(buf[20:22], r.ContextID)
	buf[22] = r.CancelCount
	buf[23] = 0 // Reserved

	copy(buf[24:], r.StubData)

	return buf
}

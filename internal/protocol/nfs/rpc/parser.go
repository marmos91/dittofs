package rpc

import (
	"bytes"
	"encoding/binary"
	"fmt"

	xdr "github.com/rasky/go-xdr/xdr2"
)

// Serializable is an interface for types that can be encoded/decoded using XDR.
//
// This interface is currently unused but provides a standard pattern for
// implementing XDR marshaling/unmarshaling on custom types. Types implementing
// this interface can handle their own serialization logic.
type Serializable interface {
	// Encode serializes the type to XDR byte format.
	Encode() ([]byte, error)

	// Decode deserializes the type from XDR byte format.
	Decode([]byte) error
}

// ReadCall parses an RPC call message from raw bytes.
//
// This function deserializes the RPC call header from the network data,
// extracting all the routing and authentication information needed to
// process the request. It validates that the message is actually a CALL
// (not a REPLY) and properly unmarshals all fields.
//
// The function uses XDR (External Data Representation) encoding as defined
// in RFC 4506. XDR is a standard for describing and encoding data, providing
// platform-independent data representation.
//
// Parameters:
//   - data: Raw bytes from the network containing the RPC call message
//
// Returns:
//   - *RPCCallMessage: Parsed RPC call header with all fields populated
//   - error: Parse error if the data is malformed or not a valid CALL message
//
// The parsed RPCCallMessage contains:
//   - XID: Transaction identifier for matching replies
//   - Program/Version/Procedure: What operation to perform
//   - Authentication credentials and verifier
//
// After calling ReadCall, use ReadData to extract the procedure-specific
// parameters that follow the RPC header.
//
// Example usage:
//
//	call, err := rpc.ReadCall(receivedData)
//	if err != nil {
//	    return fmt.Errorf("parse RPC call: %w", err)
//	}
//
//	// Extract procedure parameters
//	params, err := rpc.ReadData(receivedData, call)
//	if err != nil {
//	    return fmt.Errorf("read parameters: %w", err)
//	}
func ReadCall(data []byte) (*RPCCallMessage, error) {
	call := &RPCCallMessage{}

	// Unmarshal the RPC call header using XDR
	// This decodes all fields: XID, MsgType, RPCVersion, Program,
	// Version, Procedure, Cred, and Verf
	_, err := xdr.Unmarshal(bytes.NewReader(data), call)
	if err != nil {
		return nil, fmt.Errorf("unmarshal RPC call: %w", err)
	}

	// Validate this is a CALL message (not a REPLY)
	// MsgType must be 0 for calls, 1 for replies
	if call.MsgType != RPCCall {
		return nil, fmt.Errorf("expected CALL (0), got %d", call.MsgType)
	}

	return call, nil
}

// ReadData extracts the procedure-specific parameters from an RPC message.
//
// After parsing the RPC call header with ReadCall, this function extracts
// the remaining bytes that contain the procedure-specific parameters. It
// skips over the RPC header fields and authentication data to find where
// the actual procedure arguments begin.
//
// This function manually calculates offsets instead of using XDR unmarshaling
// because we don't know the structure of the procedure parameters at this point -
// that's determined by the specific program/version/procedure being called.
//
// The calculation accounts for:
//   - RPC header: 6 fields × 4 bytes = 24 bytes (XID through Procedure)
//   - Credentials: 4 bytes (flavor) + 4 bytes (length) + data + padding
//   - Verifier: 4 bytes (flavor) + 4 bytes (length) + data + padding
//
// XDR Padding Rules:
// All variable-length data (like the credential and verifier bodies) must be
// padded to 4-byte boundaries. This ensures proper alignment for subsequent
// fields in the XDR stream.
//
// Parameters:
//   - message: Complete raw RPC message bytes
//   - call: Parsed RPC call header (from ReadCall) - used to validate parsing
//
// Returns:
//   - []byte: Procedure-specific parameter bytes (empty if none)
//   - error: Always returns nil (kept for interface compatibility)
//
// Example usage:
//
//	call, err := rpc.ReadCall(data)
//	if err != nil {
//	    return err
//	}
//
//	params, err := rpc.ReadData(data, call)
//	if err != nil {
//	    return err
//	}
//
//	// Now unmarshal params based on the procedure
//	switch call.Procedure {
//	case nfs.ProcGetAttr:
//	    var req nfs.GetAttrRequest
//	    xdr.Unmarshal(bytes.NewReader(params), &req)
//	}
func ReadData(message []byte, call *RPCCallMessage) ([]byte, error) {
	// Start after the fixed RPC header
	// XID, MsgType, RPCVersion, Program, Version, Procedure = 6 × 4 bytes = 24 bytes
	offset := 24

	// Skip credentials (flavor + length + data + padding)
	// Read the credential flavor and length to know how much to skip
	offset += 4 // Skip flavor field
	credLen := binary.BigEndian.Uint32(message[offset : offset+4])
	offset += 4            // Skip length field
	offset += int(credLen) // Skip credential body

	// Add XDR padding for credential body (align to 4-byte boundary)
	// Formula: padding = (4 - (length % 4)) % 4
	// Examples: length=5 -> padding=3, length=8 -> padding=0
	padding := (4 - (credLen % 4)) % 4
	offset += int(padding)

	// Skip verifier (flavor + length + data + padding)
	// Same structure as credentials
	offset += 4 // Skip flavor field
	verfLen := binary.BigEndian.Uint32(message[offset : offset+4])
	offset += 4            // Skip length field
	offset += int(verfLen) // Skip verifier body

	// Add XDR padding for verifier body
	padding = (4 - (verfLen % 4)) % 4
	offset += int(padding)

	// If we've consumed the entire message, return empty data
	// This can happen for procedures that take no parameters (like NULL)
	if offset >= len(message) {
		return []byte{}, nil
	}

	// Return the remaining bytes, which contain procedure-specific parameters
	remaining := message[offset:]
	return remaining, nil
}

// MakeSuccessReply constructs an RPC success reply message.
//
// This function builds a complete RPC reply indicating successful execution
// of a procedure. It creates the RPC reply header with appropriate fields
// and prepends the RPC fragment header required by the RPC record marking
// standard (RFC 5531 Section 11).
//
// The reply structure is:
//  1. RPC Fragment Header (4 bytes):
//     - High bit set to 1 (indicates last fragment)
//     - Lower 31 bits contain the fragment length
//  2. RPC Reply Header (variable, XDR encoded):
//     - XID (matching the call)
//     - MsgType = 1 (REPLY)
//     - ReplyState = 0 (MSG_ACCEPTED)
//     - Verifier (typically AUTH_NULL for responses)
//     - AcceptStat = 0 (SUCCESS)
//  3. Procedure Results (variable, XDR encoded):
//     - Data parameter contains the procedure-specific results
//
// RPC Fragment Header Format:
// The high bit (bit 31) indicates whether this is the last fragment.
// For single-fragment messages (the common case), this bit is set to 1.
// The remaining 31 bits contain the fragment length in bytes.
//
// Example: 0x80000064 means:
//   - 1 in bit 31: last fragment
//   - 0x64 (100) in bits 0-30: fragment is 100 bytes long
//
// Parameters:
//   - xid: Transaction ID from the original RPC call (must match)
//   - data: XDR-encoded procedure results (may be empty)
//
// Returns:
//   - []byte: Complete RPC reply ready to send on the wire
//   - error: Encoding error if reply marshaling fails
//
// Example usage:
//
//	// Marshal procedure results
//	var result nfs.GetAttrResult
//	result.Status = nfs.NFS3_OK
//	result.Attributes = attrs
//
//	var buf bytes.Buffer
//	xdr.Marshal(&buf, &result)
//
//	// Create reply
//	reply, err := rpc.MakeSuccessReply(call.XID, buf.Bytes())
//	if err != nil {
//	    return err
//	}
//
//	// Send reply
//	conn.Write(reply)
func MakeSuccessReply(xid uint32, data []byte) ([]byte, error) {
	// Construct the RPC reply header
	reply := RPCReplyMessage{
		XID:        xid,            // Echo back the transaction ID from the call
		MsgType:    RPCReply,       // 1 = REPLY
		ReplyState: RPCMsgAccepted, // 0 = MSG_ACCEPTED (call was accepted)

		// AUTH_NULL verifier (no authentication in reply)
		// Most NFS implementations use AUTH_NULL for reply verifiers
		Verf: OpaqueAuth{
			Flavor: AuthNull, // 0 = AUTH_NULL
			Body:   []byte{},
		},

		AcceptStat: RPCSuccess, // 0 = SUCCESS (procedure executed successfully)
	}

	// PERFORMANCE OPTIMIZATION: Pre-allocate buffer for reply header
	// Size: RPC reply header (~28 bytes) + procedure data
	replyHeaderSize := 28
	estimatedSize := replyHeaderSize + len(data)
	buf := bytes.NewBuffer(make([]byte, 0, estimatedSize))

	// Marshal the reply header using XDR encoding
	// This converts the struct to network byte order
	_, err := xdr.Marshal(buf, &reply)
	if err != nil {
		return nil, fmt.Errorf("marshal reply: %w", err)
	}

	// Append the procedure-specific result data
	// This data should already be XDR-encoded by the caller
	buf.Write(data)

	// Prepend RPC fragment header (4 bytes)
	// Format: [last_fragment_bit (1 bit)][fragment_length (31 bits)]
	replyData := buf.Bytes()
	fragmentHeader := make([]byte, 4)

	// Set high bit (0x80000000) to indicate last fragment
	// OR with length to combine both fields
	// 0x80000000 = binary 10000000_00000000_00000000_00000000
	binary.BigEndian.PutUint32(fragmentHeader, 0x80000000|uint32(len(replyData)))

	// PERFORMANCE OPTIMIZATION: Pre-allocate final buffer to avoid append reallocation
	result := make([]byte, 0, 4+len(replyData))
	result = append(result, fragmentHeader...)
	result = append(result, replyData...)
	return result, nil
}

// MakeErrorReply creates an RPC error reply message.
//
// This function constructs a complete RPC reply that indicates an error
// occurred on the server side. It's used when the server accepts the RPC call
// but cannot successfully execute it due to system errors, resource exhaustion,
// or rate limiting.
//
// The reply indicates MSG_ACCEPTED (the call was syntactically valid) but
// sets the AcceptStat to indicate the specific error type (e.g., SYSTEM_ERR).
//
// Parameters:
//   - xid: Transaction ID from the original RPC call (must match)
//   - acceptStat: The error status code (RPCSystemErr, RPCProcUnavail, etc.)
//
// Returns:
//   - []byte: Complete RPC error reply ready to send on the wire
//   - error: Encoding error if reply marshaling fails
//
// Example usage:
//
//	// Rate limit exceeded - send system error
//	reply, err := rpc.MakeErrorReply(call.XID, rpc.RPCSystemErr)
//	if err != nil {
//	    return err
//	}
//	conn.Write(reply)
func MakeErrorReply(xid uint32, acceptStat uint32) ([]byte, error) {
	// Construct the RPC reply header
	reply := RPCReplyMessage{
		XID:        xid,            // Echo back the transaction ID from the call
		MsgType:    RPCReply,       // 1 = REPLY
		ReplyState: RPCMsgAccepted, // 0 = MSG_ACCEPTED (call was accepted)

		// AUTH_NULL verifier (no authentication in reply)
		Verf: OpaqueAuth{
			Flavor: AuthNull, // 0 = AUTH_NULL
			Body:   []byte{},
		},

		AcceptStat: acceptStat, // Error status (e.g., SYSTEM_ERR)
	}

	// Pre-allocate buffer for reply header
	replyHeaderSize := 28
	buf := bytes.NewBuffer(make([]byte, 0, replyHeaderSize))

	// Marshal the reply header using XDR encoding
	_, err := xdr.Marshal(buf, &reply)
	if err != nil {
		return nil, fmt.Errorf("marshal error reply: %w", err)
	}

	// Prepend RPC fragment header (4 bytes)
	replyData := buf.Bytes()
	fragmentHeader := make([]byte, 4)

	// Set high bit to indicate last fragment
	binary.BigEndian.PutUint32(fragmentHeader, 0x80000000|uint32(len(replyData)))

	// Combine fragment header and reply data
	result := make([]byte, 0, 4+len(replyData))
	result = append(result, fragmentHeader...)
	result = append(result, replyData...)
	return result, nil
}

// XdrPadding calculates the number of padding bytes needed for XDR alignment.
//
// XDR (External Data Representation) requires all data to be aligned on
// 4-byte boundaries. Variable-length data (strings, opaque data, arrays)
// must be padded with zeros to reach the next 4-byte boundary.
//
// Padding Formula: (4 - (length % 4)) % 4
//
// Examples:
//   - length=1: padding=3 (pad to 4 bytes)
//   - length=2: padding=2 (pad to 4 bytes)
//   - length=3: padding=1 (pad to 4 bytes)
//   - length=4: padding=0 (already aligned)
//   - length=5: padding=3 (pad to 8 bytes)
//
// This function is useful when manually encoding XDR data or calculating
// sizes for buffer allocation.
//
// Parameters:
//   - length: The current data length in bytes
//
// Returns:
//   - uint32: Number of padding bytes needed (0-3)
//
// Example usage:
//
//	nameBytes := []byte("example.txt")
//	nameLen := uint32(len(nameBytes))
//	padding := rpc.XdrPadding(nameLen)
//
//	// Total size: 4 (length) + nameLen + padding
//	totalSize := 4 + nameLen + padding
func XdrPadding(length uint32) uint32 {
	// Formula: (4 - (length % 4)) % 4
	// The outer modulo handles the case where length is already aligned (% 4 == 0)
	// In that case: (4 - 0) % 4 = 0 (no padding needed)
	return (4 - (length % 4)) % 4
}

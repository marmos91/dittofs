// Package callback provides the NLM callback client for GRANTED notifications.
//
// When a blocking lock request (block=true) is queued and the lock later
// becomes available, the server sends an NLM_GRANTED callback to the client.
// This package implements the callback sending mechanism.
package callback

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/marmos91/dittofs/internal/protocol/nfs/rpc"
	"github.com/marmos91/dittofs/internal/protocol/nlm/types"
	nlm_xdr "github.com/marmos91/dittofs/internal/protocol/nlm/xdr"
)

const (
	// CallbackTimeout is the TOTAL timeout for NLM_GRANTED callbacks.
	//
	// This is a LOCKED DECISION from CONTEXT.md: 5 second total timeout.
	// The deadline applies to both dial and I/O combined.
	// Per CONTEXT.md: fresh TCP connection for each callback (no caching).
	CallbackTimeout = 5 * time.Second

	// RPC version 2 per RFC 5531
	rpcVersion = 2
)

// SendGrantedCallback sends an NLM_GRANTED callback to a client.
//
// This function establishes a new TCP connection to the client, sends the
// NLM_GRANTED RPC call, and waits for the reply to confirm delivery.
//
// Per CONTEXT.md locked decision:
//   - Fresh TCP connection for each callback (no connection caching)
//   - 5 second TOTAL timeout (applies to dial + I/O combined)
//   - On failure, caller should release the lock (no hold period)
//
// Parameters:
//   - ctx: Parent context for cancellation
//   - addr: Client callback address (IP:port)
//   - prog: Callback program number (NLM program number)
//   - vers: Callback program version (NLM version 4)
//   - args: NLM_GRANTED arguments to send
//
// Returns:
//   - error: nil on success, error if callback failed
func SendGrantedCallback(
	ctx context.Context,
	addr string,
	prog uint32,
	vers uint32,
	args *types.NLM4GrantedArgs,
) error {
	// Create a context with 5s total deadline for the entire operation
	// This is a LOCKED DECISION from CONTEXT.md: 5 second timeout total
	callbackCtx, cancel := context.WithTimeout(ctx, CallbackTimeout)
	defer cancel()

	// Create TCP connection using the context deadline for dial
	// The DialContext respects the context deadline
	var dialer net.Dialer
	conn, err := dialer.DialContext(callbackCtx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial callback address %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()

	// Get the absolute deadline from context and set on connection for I/O
	// This ensures remaining time after dial is used for I/O
	if deadline, ok := callbackCtx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return fmt.Errorf("set deadline: %w", err)
		}
	}

	// Encode NLM_GRANTED args
	var argsBuf bytes.Buffer
	if err := nlm_xdr.EncodeNLM4GrantedArgs(&argsBuf, args); err != nil {
		return fmt.Errorf("encode granted args: %w", err)
	}

	// Build RPC call message
	// XID can be any unique value - use current time nanoseconds
	xid := uint32(time.Now().UnixNano() & 0xFFFFFFFF)

	callMsg, err := buildRPCCallMessage(xid, prog, vers, types.NLMProcGranted, argsBuf.Bytes())
	if err != nil {
		return fmt.Errorf("build call message: %w", err)
	}

	// Send the call with RPC record marking (fragment header)
	framedMsg := addRecordMark(callMsg, true)
	if _, err := conn.Write(framedMsg); err != nil {
		return fmt.Errorf("write call: %w", err)
	}

	// Read and validate response
	// We need to wait for it to confirm delivery (per NLM callback semantics)
	if err := readAndDiscardReply(conn); err != nil {
		return fmt.Errorf("read reply: %w", err)
	}

	return nil
}

// buildRPCCallMessage builds an RPC CALL message with AUTH_NULL credentials.
//
// Wire format per RFC 5531:
//
//	XID:        [uint32]
//	MsgType:    [uint32] = 0 (CALL)
//	RPCVersion: [uint32] = 2
//	Program:    [uint32]
//	Version:    [uint32]
//	Procedure:  [uint32]
//	Cred:       AUTH_NULL (flavor=0, length=0)
//	Verf:       AUTH_NULL (flavor=0, length=0)
//	Args:       [procedure args]
func buildRPCCallMessage(xid, prog, vers, proc uint32, args []byte) ([]byte, error) {
	var buf bytes.Buffer

	// XID
	if err := binary.Write(&buf, binary.BigEndian, xid); err != nil {
		return nil, fmt.Errorf("write xid: %w", err)
	}

	// Message type: CALL (0)
	if err := binary.Write(&buf, binary.BigEndian, uint32(rpc.RPCCall)); err != nil {
		return nil, fmt.Errorf("write msg type: %w", err)
	}

	// RPC version: 2
	if err := binary.Write(&buf, binary.BigEndian, uint32(rpcVersion)); err != nil {
		return nil, fmt.Errorf("write rpc version: %w", err)
	}

	// Program number
	if err := binary.Write(&buf, binary.BigEndian, prog); err != nil {
		return nil, fmt.Errorf("write program: %w", err)
	}

	// Program version
	if err := binary.Write(&buf, binary.BigEndian, vers); err != nil {
		return nil, fmt.Errorf("write version: %w", err)
	}

	// Procedure number
	if err := binary.Write(&buf, binary.BigEndian, proc); err != nil {
		return nil, fmt.Errorf("write procedure: %w", err)
	}

	// Auth credentials: AUTH_NULL (flavor=0, length=0)
	if err := binary.Write(&buf, binary.BigEndian, uint32(rpc.AuthNull)); err != nil {
		return nil, fmt.Errorf("write cred flavor: %w", err)
	}
	if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
		return nil, fmt.Errorf("write cred length: %w", err)
	}

	// Auth verifier: AUTH_NULL (flavor=0, length=0)
	if err := binary.Write(&buf, binary.BigEndian, uint32(rpc.AuthNull)); err != nil {
		return nil, fmt.Errorf("write verf flavor: %w", err)
	}
	if err := binary.Write(&buf, binary.BigEndian, uint32(0)); err != nil {
		return nil, fmt.Errorf("write verf length: %w", err)
	}

	// Procedure arguments
	if _, err := buf.Write(args); err != nil {
		return nil, fmt.Errorf("write args: %w", err)
	}

	return buf.Bytes(), nil
}

// addRecordMark adds RPC record marking (fragment header) to a message.
//
// Per RFC 5531 Section 11 (Record Marking):
// For TCP, RPC messages are preceded by a 4-byte header:
//   - bit 31: Last fragment flag (1 = last fragment)
//   - bits 0-30: Fragment length
//
// Since we send the entire message as a single fragment, we set the
// last fragment bit.
func addRecordMark(msg []byte, lastFragment bool) []byte {
	header := uint32(len(msg))
	if lastFragment {
		header |= 0x80000000
	}

	result := make([]byte, 4+len(msg))
	binary.BigEndian.PutUint32(result[0:4], header)
	copy(result[4:], msg)

	return result
}

// readAndDiscardReply reads and discards the RPC reply.
//
// Per NLM callback semantics, we need to wait for the reply to confirm
// delivery, but we don't care about the actual result (per CONTEXT.md).
// We just need to ensure the callback was received.
func readAndDiscardReply(conn net.Conn) error {
	// Read fragment header (4 bytes)
	var headerBuf [4]byte
	if _, err := io.ReadFull(conn, headerBuf[:]); err != nil {
		return fmt.Errorf("read reply header: %w", err)
	}

	// Parse fragment length
	header := binary.BigEndian.Uint32(headerBuf[:])
	fragLen := header & 0x7FFFFFFF

	// Sanity check on fragment length (max 1MB should be plenty)
	if fragLen > 1*1024*1024 {
		return fmt.Errorf("reply fragment too large: %d", fragLen)
	}

	// Read reply body (just discard - we don't care about status per CONTEXT.md)
	replyBuf := make([]byte, fragLen)
	if _, err := io.ReadFull(conn, replyBuf); err != nil {
		return fmt.Errorf("read reply body: %w", err)
	}

	return nil
}

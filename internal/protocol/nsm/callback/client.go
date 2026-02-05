// Package callback provides the NSM callback client for SM_NOTIFY notifications.
//
// When a server restarts, it sends SM_NOTIFY callbacks to all registered clients
// to inform them of the server's new state. This package implements the callback
// sending mechanism.
package callback

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/rpc"
	"github.com/marmos91/dittofs/internal/protocol/nsm/types"
)

const (
	// CallbackTimeout is the TOTAL timeout for SM_NOTIFY callbacks.
	//
	// This is a LOCKED DECISION from CONTEXT.md: 5 second total timeout.
	// The deadline applies to both dial and I/O combined.
	// Per CONTEXT.md: fresh TCP connection for each callback (no caching).
	CallbackTimeout = 5 * time.Second

	// DefaultNSMPort is the standard NSM/NLM port.
	DefaultNSMPort = 12049

	// RPC version 2 per RFC 5531
	rpcVersion = 2
)

// Client sends SM_NOTIFY callbacks to registered monitors.
//
// Client is stateless and thread-safe. Each callback creates a fresh TCP
// connection per CONTEXT.md decision (no connection caching).
type Client struct {
	timeout time.Duration
}

// NewClient creates a new SM_NOTIFY callback client.
//
// Parameters:
//   - timeout: Total timeout for dial + I/O. If 0, uses CallbackTimeout (5s).
//
// Returns a configured Client ready to send callbacks.
func NewClient(timeout time.Duration) *Client {
	if timeout == 0 {
		timeout = CallbackTimeout
	}
	return &Client{timeout: timeout}
}

// Send sends an SM_NOTIFY callback to the specified address.
//
// Per CONTEXT.md locked decisions:
//   - Fresh TCP connection for each callback (no connection caching)
//   - 5 second TOTAL timeout (applies to dial + I/O combined)
//   - On failure, caller should treat client as crashed
//
// Parameters:
//   - ctx: Parent context for cancellation
//   - addr: Client callback address (IP:port or just IP)
//   - status: The NSM status message containing mon_name, state, and priv
//   - proc: Callback procedure number (e.g., NLM_FREE_ALL)
//   - prog: RPC program number for the callback
//   - vers: RPC program version for the callback
//
// Returns:
//   - error: nil on success, error if callback failed
func (c *Client) Send(
	ctx context.Context,
	addr string,
	status *types.Status,
	proc uint32,
	prog uint32,
	vers uint32,
) error {
	// Create a context with total deadline for the entire operation
	callbackCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	// Resolve address - if no port, use default NSM port
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// No port specified, use default
		host = addr
		port = fmt.Sprintf("%d", DefaultNSMPort)
	}
	addr = net.JoinHostPort(host, port)

	// Create TCP connection using the context deadline for dial
	var dialer net.Dialer
	conn, err := dialer.DialContext(callbackCtx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial callback address %s: %w", addr, err)
	}
	defer conn.Close()

	// Get the absolute deadline from context and set on connection for I/O
	if deadline, ok := callbackCtx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return fmt.Errorf("set deadline: %w", err)
		}
	}

	// Encode SM_NOTIFY status as procedure arguments
	var argsBuf bytes.Buffer
	if err := encodeStatus(&argsBuf, status); err != nil {
		return fmt.Errorf("encode status: %w", err)
	}

	// Build RPC call message
	xid := uint32(time.Now().UnixNano() & 0xFFFFFFFF)
	callMsg, err := buildRPCCallMessage(xid, prog, vers, proc, argsBuf.Bytes())
	if err != nil {
		return fmt.Errorf("build call message: %w", err)
	}

	// Send the call with RPC record marking (fragment header)
	framedMsg := addRecordMark(callMsg, true)
	if _, err := conn.Write(framedMsg); err != nil {
		return fmt.Errorf("write call: %w", err)
	}

	// Read and validate response
	// We wait for it to confirm delivery (per NSM callback semantics)
	if err := readAndDiscardReply(conn); err != nil {
		// Don't fail on response read error - the notification was sent
		// The callback may have been processed even if we timeout on response
		logger.Debug("SM_NOTIFY callback response read failed", "addr", addr, "error", err)
	}

	return nil
}

// encodeStatus encodes an NSM Status structure to XDR format.
//
// Wire format:
//
//	mon_name: string (length + data + padding)
//	state:    int32
//	priv:     opaque[16]
func encodeStatus(buf *bytes.Buffer, status *types.Status) error {
	// mon_name (string)
	nameBytes := []byte(status.MonName)
	nameLenPadded := (len(nameBytes) + 3) & ^3
	if err := binary.Write(buf, binary.BigEndian, uint32(len(nameBytes))); err != nil {
		return err
	}
	buf.Write(nameBytes)
	for i := len(nameBytes); i < nameLenPadded; i++ {
		buf.WriteByte(0) // Padding
	}

	// state (int32)
	if err := binary.Write(buf, binary.BigEndian, status.State); err != nil {
		return err
	}

	// priv (opaque[16])
	buf.Write(status.Priv[:])

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
// Per NSM callback semantics, we need to wait for the reply to confirm
// delivery, but we don't care about the actual result.
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

	// Read reply body (just discard - we don't care about status)
	replyBuf := make([]byte, fragLen)
	if _, err := io.ReadFull(conn, replyBuf); err != nil {
		return fmt.Errorf("read reply body: %w", err)
	}

	return nil
}

// Package state implements NFSv4 state management including delegation
// callback support for CB_COMPOUND messages per RFC 7530 Section 16.

package state

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/rpc"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/protocol/xdr"
)

const (
	// CBCallbackTimeout is the total timeout for callback operations.
	// This covers both the dial and I/O combined, matching the NLM pattern.
	CBCallbackTimeout = 5 * time.Second

	// cbRPCVersion is the RPC version per RFC 5531.
	cbRPCVersion = 2
)

// ============================================================================
// Universal Address Parsing
// ============================================================================

// ParseUniversalAddr parses an NFSv4 universal address (uaddr) into host and port.
//
// Per RFC 5665 Section 5.2.3.3:
//   - For netid "tcp" (IPv4): uaddr format is "h1.h2.h3.h4.p1.p2"
//     where p1*256 + p2 = port, host = "h1.h2.h3.h4"
//   - For netid "tcp6" (IPv6): uaddr format is "h1:h2::h3.p1.p2"
//     where the last two dot-separated components are port bytes
//
// Returns the host string and port number, or an error for malformed addresses.
func ParseUniversalAddr(netid, uaddr string) (string, int, error) {
	// Find the last two dots (port components)
	lastDot := strings.LastIndex(uaddr, ".")
	if lastDot < 0 {
		return "", 0, fmt.Errorf("malformed universal address %q: no dots found", uaddr)
	}

	p2Str := uaddr[lastDot+1:]
	rest := uaddr[:lastDot]

	secondLastDot := strings.LastIndex(rest, ".")
	if secondLastDot < 0 {
		return "", 0, fmt.Errorf("malformed universal address %q: need at least host.p1.p2", uaddr)
	}

	p1Str := rest[secondLastDot+1:]
	host := rest[:secondLastDot]

	if host == "" {
		return "", 0, fmt.Errorf("malformed universal address %q: empty host", uaddr)
	}

	// Parse p1 and p2
	p1, err := strconv.Atoi(p1Str)
	if err != nil {
		return "", 0, fmt.Errorf("malformed universal address %q: invalid p1 %q: %w", uaddr, p1Str, err)
	}
	p2, err := strconv.Atoi(p2Str)
	if err != nil {
		return "", 0, fmt.Errorf("malformed universal address %q: invalid p2 %q: %w", uaddr, p2Str, err)
	}

	// Validate range 0-255
	if p1 < 0 || p1 > 255 {
		return "", 0, fmt.Errorf("malformed universal address %q: p1=%d out of range 0-255", uaddr, p1)
	}
	if p2 < 0 || p2 > 255 {
		return "", 0, fmt.Errorf("malformed universal address %q: p2=%d out of range 0-255", uaddr, p2)
	}

	port := p1*256 + p2
	return host, port, nil
}

// ============================================================================
// CB_COMPOUND Encoding
// ============================================================================

// encodeCBCompound encodes CB_COMPOUND4args per RFC 7530 Section 16.2.3.
//
// Wire format:
//
//	utf8str_cs  tag;           -- empty tag
//	uint32      minorversion;  -- 0 for NFSv4.0
//	uint32      callback_ident;-- from SETCLIENTID
//	nfs_cb_argop4 argarray<>;  -- the pre-encoded operations
func encodeCBCompound(callbackIdent uint32, ops []byte) []byte {
	var buf bytes.Buffer

	// tag: empty utf8str_cs (XDR opaque with length 0)
	_ = xdr.WriteXDROpaque(&buf, nil)

	// minorversion: 0
	_ = xdr.WriteUint32(&buf, 0)

	// callback_ident: from SETCLIENTID
	_ = xdr.WriteUint32(&buf, callbackIdent)

	// argarray: count=1 + the operation
	_ = xdr.WriteUint32(&buf, 1)
	_, _ = buf.Write(ops)

	return buf.Bytes()
}

// encodeCBRecallOp encodes one nfs_cb_argop4 for CB_RECALL.
//
// Wire format per RFC 7530 Section 16.2.2:
//
//	uint32    argop = OP_CB_RECALL (4)
//	stateid4  stateid
//	bool      truncate
//	nfs_fh4   fh
func encodeCBRecallOp(stateid *types.Stateid4, truncate bool, fh []byte) []byte {
	var buf bytes.Buffer

	// argop: OP_CB_RECALL = 4
	_ = xdr.WriteUint32(&buf, types.OP_CB_RECALL)

	// stateid4
	types.EncodeStateid4(&buf, stateid)

	// truncate: bool as uint32
	_ = xdr.WriteBool(&buf, truncate)

	// fh: nfs_fh4 as XDR opaque
	_ = xdr.WriteXDROpaque(&buf, fh)

	return buf.Bytes()
}

// ============================================================================
// RPC Message Building
// ============================================================================

// buildCBRPCCallMessage builds an RPC CALL message with AUTH_NULL credentials.
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
func buildCBRPCCallMessage(xid, prog, vers, proc uint32, args []byte) []byte {
	var buf bytes.Buffer

	// RPC header fields (writes to bytes.Buffer never fail)
	_ = xdr.WriteUint32(&buf, xid)
	_ = xdr.WriteUint32(&buf, uint32(rpc.RPCCall))
	_ = xdr.WriteUint32(&buf, uint32(cbRPCVersion))
	_ = xdr.WriteUint32(&buf, prog)
	_ = xdr.WriteUint32(&buf, vers)
	_ = xdr.WriteUint32(&buf, proc)

	// Auth credentials: AUTH_NULL (flavor=0, length=0)
	_ = xdr.WriteUint32(&buf, uint32(rpc.AuthNull))
	_ = xdr.WriteUint32(&buf, 0)

	// Auth verifier: AUTH_NULL (flavor=0, length=0)
	_ = xdr.WriteUint32(&buf, uint32(rpc.AuthNull))
	_ = xdr.WriteUint32(&buf, 0)

	// Procedure arguments
	_, _ = buf.Write(args)

	return buf.Bytes()
}

// addCBRecordMark adds RPC record marking (fragment header) to a message.
//
// Per RFC 5531 Section 11 (Record Marking):
// For TCP, RPC messages are preceded by a 4-byte header:
//   - bit 31: Last fragment flag (1 = last fragment)
//   - bits 0-30: Fragment length
//
// Since we send the entire message as a single fragment, we set the
// last fragment bit.
func addCBRecordMark(msg []byte, lastFragment bool) []byte {
	header := uint32(len(msg))
	if lastFragment {
		header |= 0x80000000
	}

	result := make([]byte, 4+len(msg))
	binary.BigEndian.PutUint32(result[0:4], header)
	copy(result[4:], msg)

	return result
}

// ============================================================================
// Connection Helpers
// ============================================================================

// dialCallback establishes a TCP connection to a client's callback address.
// It parses the universal address, dials with the context deadline, and sets
// I/O deadlines on the connection. The caller must close the returned connection.
func dialCallback(ctx context.Context, callback CallbackInfo) (net.Conn, error) {
	host, port, err := ParseUniversalAddr(callback.NetID, callback.Addr)
	if err != nil {
		return nil, fmt.Errorf("parse callback address: %w", err)
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial callback address %s: %w", addr, err)
	}

	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("set deadline: %w", err)
		}
	}

	return conn, nil
}

// ============================================================================
// Reply Parsing
// ============================================================================

// maxFragmentSize is the maximum allowed RPC fragment size (1MB).
const maxFragmentSize = 1 * 1024 * 1024

// readFragment reads an RPC record-marked fragment from a connection.
// It reads the 4-byte fragment header, validates the length, and returns
// the fragment body.
func readFragment(conn net.Conn) ([]byte, error) {
	var headerBuf [4]byte
	if _, err := io.ReadFull(conn, headerBuf[:]); err != nil {
		return nil, fmt.Errorf("read reply fragment header: %w", err)
	}

	header := binary.BigEndian.Uint32(headerBuf[:])
	fragLen := header & 0x7FFFFFFF

	if fragLen > maxFragmentSize {
		return nil, fmt.Errorf("reply fragment too large: %d", fragLen)
	}

	body := make([]byte, fragLen)
	if _, err := io.ReadFull(conn, body); err != nil {
		return nil, fmt.Errorf("read reply body: %w", err)
	}

	return body, nil
}

// readAndValidateCBReply reads an RPC reply and validates it succeeded.
//
// It reads the fragment header, parses the RPC reply status,
// and for CB_COMPOUND responses, checks the nfsstat4 status code.
//
// Wire format of RPC reply:
//
//	[4-byte fragment header]
//	XID:         [uint32]
//	MsgType:     [uint32] = 1 (REPLY)
//	reply_stat:  [uint32] = 0 (MSG_ACCEPTED)
//	verf_flavor: [uint32]
//	verf_body:   [opaque, typically 0 length]
//	accept_stat: [uint32] = 0 (SUCCESS)
//	... procedure-specific results ...
func readAndValidateCBReply(conn net.Conn) error {
	replyBuf, err := readFragment(conn)
	if err != nil {
		return err
	}

	if len(replyBuf) < 12 {
		return fmt.Errorf("reply fragment too small: %d", len(replyBuf))
	}

	reader := bytes.NewReader(replyBuf)

	// Skip XID (4 bytes)
	var xid uint32
	if err := binary.Read(reader, binary.BigEndian, &xid); err != nil {
		return fmt.Errorf("read reply xid: %w", err)
	}

	// MsgType (should be REPLY = 1)
	var msgType uint32
	if err := binary.Read(reader, binary.BigEndian, &msgType); err != nil {
		return fmt.Errorf("read reply msg type: %w", err)
	}
	if msgType != rpc.RPCReply {
		return fmt.Errorf("expected RPC reply (1), got %d", msgType)
	}

	// reply_stat (should be MSG_ACCEPTED = 0)
	var replyStat uint32
	if err := binary.Read(reader, binary.BigEndian, &replyStat); err != nil {
		return fmt.Errorf("read reply stat: %w", err)
	}
	if replyStat != rpc.RPCMsgAccepted {
		return fmt.Errorf("RPC call denied: reply_stat=%d", replyStat)
	}

	// Verifier: flavor + body length + body
	var verfFlavor uint32
	if err := binary.Read(reader, binary.BigEndian, &verfFlavor); err != nil {
		return fmt.Errorf("read verf flavor: %w", err)
	}
	var verfLen uint32
	if err := binary.Read(reader, binary.BigEndian, &verfLen); err != nil {
		return fmt.Errorf("read verf length: %w", err)
	}
	if verfLen > 0 {
		verfBody := make([]byte, verfLen)
		if _, err := io.ReadFull(reader, verfBody); err != nil {
			return fmt.Errorf("read verf body: %w", err)
		}
	}

	// accept_stat (should be SUCCESS = 0)
	var acceptStat uint32
	if err := binary.Read(reader, binary.BigEndian, &acceptStat); err != nil {
		return fmt.Errorf("read accept stat: %w", err)
	}
	if acceptStat != rpc.RPCSuccess {
		return fmt.Errorf("RPC call not successful: accept_stat=%d", acceptStat)
	}

	// For CB_COMPOUND: parse nfsstat4 from the response body
	// CB_COMPOUND4res: { nfsstat4 status; utf8str_cs tag; nfs_cb_resop4 resarray<>; }
	// We only check the top-level status.
	if reader.Len() >= 4 {
		var nfsStatus uint32
		if err := binary.Read(reader, binary.BigEndian, &nfsStatus); err != nil {
			return fmt.Errorf("read nfsstat4: %w", err)
		}
		if nfsStatus != types.NFS4_OK {
			return fmt.Errorf("CB_COMPOUND failed: nfsstat4=%d", nfsStatus)
		}
	}

	return nil
}

// ============================================================================
// SendCBRecall - Delegation Recall
// ============================================================================

// SendCBRecall sends a CB_RECALL to a client to recall a delegation.
//
// It creates a TCP connection to the client's callback address, sends
// a CB_COMPOUND containing CB_RECALL, and validates the response.
//
// Per RFC 7530 Section 16.2:
//   - Uses CB_PROC_COMPOUND (procedure 1) for CB_RECALL
//   - program = client-specified from SETCLIENTID
//   - version = NFS4_CALLBACK_VERSION (1)
//
// Parameters:
//   - ctx: Parent context for cancellation
//   - callback: Client callback info (Program, NetID, Addr)
//   - stateid: Delegation stateid to recall
//   - truncate: Whether to truncate the file (write delegation recall)
//   - fh: File handle of the delegated file
func SendCBRecall(ctx context.Context, callback CallbackInfo, stateid *types.Stateid4, truncate bool, fh []byte) error {
	callbackCtx, cancel := context.WithTimeout(ctx, CBCallbackTimeout)
	defer cancel()

	conn, err := dialCallback(callbackCtx, callback)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	// Encode CB_RECALL wrapped in CB_COMPOUND
	// callback_ident=0: client identifies via the program number
	recallOp := encodeCBRecallOp(stateid, truncate, fh)
	compoundArgs := encodeCBCompound(0, recallOp)

	// Build and send RPC CALL message
	xid := uint32(time.Now().UnixNano() & 0xFFFFFFFF)
	callMsg := buildCBRPCCallMessage(xid, callback.Program, types.NFS4_CALLBACK_VERSION, types.CB_PROC_COMPOUND, compoundArgs)

	framedMsg := addCBRecordMark(callMsg, true)
	if _, err := conn.Write(framedMsg); err != nil {
		return fmt.Errorf("write call: %w", err)
	}

	if err := readAndValidateCBReply(conn); err != nil {
		return fmt.Errorf("callback reply: %w", err)
	}

	logger.Debug("CB_RECALL sent successfully",
		"addr", conn.RemoteAddr().String(),
		"program", callback.Program)

	return nil
}

// ============================================================================
// SendCBNull - Callback Path Verification
// ============================================================================

// SendCBNull sends a CB_NULL to verify the callback path is operational.
//
// CB_NULL is RPC procedure 0 (not a CB_COMPOUND operation). It takes
// no arguments and returns no results. Used to verify the callback
// TCP connection before relying on it for delegation recall.
//
// Parameters:
//   - ctx: Parent context for cancellation
//   - callback: Client callback info (Program, NetID, Addr)
func SendCBNull(ctx context.Context, callback CallbackInfo) error {
	callbackCtx, cancel := context.WithTimeout(ctx, CBCallbackTimeout)
	defer cancel()

	conn, err := dialCallback(callbackCtx, callback)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	// Build and send RPC CALL message for CB_NULL (procedure 0, no args)
	xid := uint32(time.Now().UnixNano() & 0xFFFFFFFF)
	callMsg := buildCBRPCCallMessage(xid, callback.Program, types.NFS4_CALLBACK_VERSION, types.CB_PROC_NULL, nil)

	framedMsg := addCBRecordMark(callMsg, true)
	if _, err := conn.Write(framedMsg); err != nil {
		return fmt.Errorf("write call: %w", err)
	}

	// Read and discard reply -- just confirm receipt
	if err := readAndDiscardCBReply(conn); err != nil {
		return fmt.Errorf("read reply: %w", err)
	}

	logger.Debug("CB_NULL sent successfully",
		"addr", conn.RemoteAddr().String(),
		"program", callback.Program)

	return nil
}

// readAndDiscardCBReply reads and discards the RPC reply for CB_NULL.
// We just need to confirm the callback was received.
func readAndDiscardCBReply(conn net.Conn) error {
	_, err := readFragment(conn)
	return err
}

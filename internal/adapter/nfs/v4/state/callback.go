// Package state implements NFSv4 state management including delegation
// callback support for CB_COMPOUND messages per RFC 7530 Section 16.
//
// This file contains the v4.0 dial-out callback path. Shared wire-format
// helpers have been extracted to callback_common.go.

package state

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
)

const (
	// CBCallbackTimeout is the total timeout for callback operations.
	// This covers both the dial and I/O combined, matching the NLM pattern.
	CBCallbackTimeout = 5 * time.Second
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
// CB_COMPOUND Encoding (v4.0)
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
// Reply Parsing (v4.0 dial-out)
// ============================================================================

// readAndValidateCBReply reads an RPC reply from a dial-out connection and validates it.
// Uses the shared ReadFragment and ValidateCBReply helpers.
func readAndValidateCBReply(conn net.Conn) error {
	replyBuf, err := ReadFragment(conn)
	if err != nil {
		return err
	}
	return ValidateCBReply(replyBuf)
}

// ============================================================================
// SendCBRecall - Delegation Recall (v4.0 dial-out)
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
	recallOp := EncodeCBRecallOp(stateid, truncate, fh)
	compoundArgs := encodeCBCompound(0, recallOp)

	// Build and send RPC CALL message
	xid := uint32(time.Now().UnixNano() & 0xFFFFFFFF)
	callMsg := BuildCBRPCCallMessage(xid, callback.Program, types.NFS4_CALLBACK_VERSION, types.CB_PROC_COMPOUND, compoundArgs)

	framedMsg := AddCBRecordMark(callMsg, true)
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
	callMsg := BuildCBRPCCallMessage(xid, callback.Program, types.NFS4_CALLBACK_VERSION, types.CB_PROC_NULL, nil)

	framedMsg := AddCBRecordMark(callMsg, true)
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
	_, err := ReadFragment(conn)
	return err
}

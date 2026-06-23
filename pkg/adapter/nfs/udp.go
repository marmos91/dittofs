package nfs

import (
	"context"
	"errors"
	"fmt"
	"net"
	"runtime/debug"

	mount_handlers "github.com/marmos91/dittofs/internal/adapter/nfs/mount/handlers"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	"github.com/marmos91/dittofs/internal/logger"
)

// maxUDPDatagram bounds a single RPC-over-UDP message. The protocols served
// over UDP (NLM, NSM, MOUNT) exchange only small fixed-size messages, so the
// practical 64 KiB UDP datagram ceiling is far more than enough.
const maxUDPDatagram = 65535

// rpcRecordMarkLen is the length of the RFC 5531 record-marking header that the
// shared reply builders prepend for the stream (TCP) transport. UDP delivers
// one RPC message per datagram with no record marking, so this prefix is
// stripped before sending.
const rpcRecordMarkLen = 4

// defaultUDPHandlerLimit caps concurrent UDP datagram handlers when the adapter
// config does not set MaxRequestsPerConnection (mirrors its default).
const defaultUDPHandlerLimit = 100

// isUDPEnabled reports whether the UDP transport for NLM/NSM/MOUNT should be
// started. Disabled unless adapters.nfs.udp.enabled is explicitly true.
func (s *NFSAdapter) isUDPEnabled() bool {
	return s.config.UDP.Enabled != nil && *s.config.UDP.Enabled
}

// startUDP binds the UDP transport on the NFS port and serves the lock-manager
// auxiliary protocols (NLM, NSM) and MOUNT. NFS itself is never served over UDP
// because READ/WRITE payloads do not fit a single datagram; only the small
// lock/status/mount messages are. The listener is closed when ctx is cancelled.
func (s *NFSAdapter) startUDP(ctx context.Context) error {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: s.config.Port})
	if err != nil {
		return fmt.Errorf("listen udp :%d: %w", s.config.Port, err)
	}
	s.udpConn = conn

	logger.Info("NFS UDP transport listening (NLM/NSM/MOUNT)", "port", s.config.Port)

	// Close the socket on shutdown so the read loop unblocks and exits.
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	go s.serveUDP(ctx, conn)
	return nil
}

// serveUDP reads datagrams and dispatches each one. A single stateless
// pseudo-connection is reused for every datagram: the NLM/NSM/MOUNT handlers
// read only the adapter (s) and emit metrics — they never touch a net.Conn —
// and UDP carries no per-connection state (no record marking, TLS, or DRC).
func (s *NFSAdapter) serveUDP(ctx context.Context, conn *net.UDPConn) {
	pc := &NFSConnection{server: s}
	buf := make([]byte, maxUDPDatagram)

	// Bound in-flight datagram handlers so a flood cannot spawn unbounded
	// goroutines (the TCP path is bounded per-connection by requestSem). UDP is
	// lossy by nature, so once the limit is reached we drop the datagram rather
	// than block the read loop — the client retransmits.
	limit := s.config.MaxRequestsPerConnection
	if limit <= 0 {
		limit = defaultUDPHandlerLimit
	}
	sem := make(chan struct{}, limit)

	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			// A closed socket (shutdown) or a cancelled context ends the loop.
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return
			}
			logger.Debug("NFS UDP read error", "error", err)
			continue
		}

		select {
		case sem <- struct{}{}:
		default:
			logger.Debug("NFS UDP: handler limit reached, dropping datagram", "client", src.String(), "limit", limit)
			continue
		}

		// Copy out of the shared read buffer before handing to a goroutine.
		msg := make([]byte, n)
		copy(msg, buf[:n])
		go func(src *net.UDPAddr, msg []byte) {
			defer func() { <-sem }()
			pc.handleUDPDatagram(ctx, conn, src, msg)
		}(src, msg)
	}
}

// handleUDPDatagram parses a single RPC-over-UDP message and routes it to the
// NLM, NSM, or MOUNT handler, then sends the reply back to the source address.
// NFS and any other program are answered with PROG_UNAVAIL: they are not served
// over UDP. A panic in a handler is contained so one bad datagram cannot crash
// the listener.
func (c *NFSConnection) handleUDPDatagram(ctx context.Context, conn *net.UDPConn, src *net.UDPAddr, msg []byte) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("Panic in NFS UDP handler", "client", src.String(), "error", r, "stack", string(debug.Stack()))
		}
	}()

	call, err := rpc.ReadCall(msg)
	if err != nil {
		logger.Debug("NFS UDP: parse RPC call failed", "client", src.String(), "error", err)
		return
	}

	procedureData, err := rpc.ReadData(msg, call)
	if err != nil {
		logger.Debug("NFS UDP: extract procedure data failed", "client", src.String(), "xid", fmt.Sprintf("0x%x", call.XID), "error", err)
		return
	}

	clientAddr := src.String()

	var body []byte
	switch call.Program {
	case rpc.ProgramNLM:
		// NLM v1/v3 (32-bit offsets) and v4 (64-bit) — macOS uses v1/v3.
		switch call.Version {
		case rpc.NLMVersion1, rpc.NLMVersion3, rpc.NLMVersion4:
			body, err = c.handleNLMProcedure(ctx, call, procedureData, clientAddr)
		default:
			c.writeUDPVersionMismatch(conn, src, call.XID, rpc.NLMVersion1, rpc.NLMVersion4)
			return
		}

	case rpc.ProgramNSM:
		if call.Version != rpc.NSMVersion1 {
			c.writeUDPVersionMismatch(conn, src, call.XID, rpc.NSMVersion1, rpc.NSMVersion1)
			return
		}
		body, err = c.handleNSMProcedure(ctx, call, procedureData, clientAddr)

	case rpc.ProgramMount:
		// MNT returns a v3-format file handle, so it requires MOUNT v3 — same
		// gate the TCP dispatcher applies. Other MOUNT procedures (NULL, UMNT,
		// EXPORT, …) are version-agnostic and accepted on v1/v2/v3.
		if call.Procedure == mount_handlers.MountProcMnt && call.Version != rpc.MountVersion3 {
			c.writeUDPVersionMismatch(conn, src, call.XID, rpc.MountVersion3, rpc.MountVersion3)
			return
		}
		body, err = c.handleMountProcedure(ctx, call, procedureData, clientAddr)

	default:
		// NFS itself and anything else are not served over UDP.
		c.writeUDPAcceptError(conn, src, call.XID, rpc.RPCProgUnavail)
		return
	}

	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		logger.Debug("NFS UDP handler error", "program", call.Program, "procedure", call.Procedure, "xid", fmt.Sprintf("0x%x", call.XID), "error", err)
		c.writeUDPAcceptError(conn, src, call.XID, rpc.RPCSystemErr)
		return
	}

	c.writeUDPReply(conn, src, call.XID, body)
}

// writeUDPReply wraps a procedure result in an RPC success reply and sends it as
// a single datagram (without the stream record-marking header).
func (c *NFSConnection) writeUDPReply(conn *net.UDPConn, src *net.UDPAddr, xid uint32, body []byte) {
	reply, err := rpc.MakeSuccessReply(xid, body)
	if err != nil {
		logger.Debug("NFS UDP: build success reply failed", "xid", fmt.Sprintf("0x%x", xid), "error", err)
		return
	}
	sendUDP(conn, src, reply)
}

// writeUDPAcceptError sends an RPC reply carrying an accept-status error
// (e.g. PROG_UNAVAIL, SYSTEM_ERR) as a UDP datagram.
func (c *NFSConnection) writeUDPAcceptError(conn *net.UDPConn, src *net.UDPAddr, xid, acceptStat uint32) {
	reply, err := rpc.MakeErrorReply(xid, acceptStat)
	if err != nil {
		logger.Debug("NFS UDP: build error reply failed", "xid", fmt.Sprintf("0x%x", xid), "error", err)
		return
	}
	sendUDP(conn, src, reply)
}

// writeUDPVersionMismatch sends an RFC 5531 PROG_MISMATCH reply over UDP.
func (c *NFSConnection) writeUDPVersionMismatch(conn *net.UDPConn, src *net.UDPAddr, xid, low, high uint32) {
	reply, err := rpc.MakeProgMismatchReply(xid, low, high)
	if err != nil {
		logger.Debug("NFS UDP: build mismatch reply failed", "xid", fmt.Sprintf("0x%x", xid), "error", err)
		return
	}
	sendUDP(conn, src, reply)
}

// sendUDP strips the leading record-marking header that the shared reply
// builders add for the stream transport and writes the bare RPC message as one
// datagram to the client's source address.
func sendUDP(conn *net.UDPConn, src *net.UDPAddr, framed []byte) {
	payload := framed
	if len(framed) >= rpcRecordMarkLen {
		payload = framed[rpcRecordMarkLen:]
	}
	if _, err := conn.WriteToUDP(payload, src); err != nil {
		logger.Debug("NFS UDP: write reply failed", "client", src.String(), "error", err)
	}
}

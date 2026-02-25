package portmap

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/portmap/handlers"
	"github.com/marmos91/dittofs/internal/adapter/nfs/portmap/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	"github.com/marmos91/dittofs/internal/logger"
)

// maxTCPConns is the maximum number of concurrent TCP connections the
// portmapper will accept. Portmapper is lightweight so 64 is generous.
const maxTCPConns = 64

// ServerConfig holds configuration for the portmapper server.
type ServerConfig struct {
	// Port is the port to listen on (default 10111).
	Port int

	// EnableTCP controls whether the TCP listener is started.
	// Default: true.
	EnableTCP bool

	// EnableUDP controls whether the UDP listener is started.
	// Default: true.
	EnableUDP bool

	// Registry is the service registry used by procedure handlers.
	Registry *Registry
}

// Server implements an RFC 1057 portmapper that listens on both TCP and UDP.
//
// TCP uses RPC record marking (4-byte fragment header) and supports
// connection reuse (multiple requests per connection).
// UDP treats each packet as one complete RPC message (no record marking).
type Server struct {
	config        ServerConfig
	handler       *handlers.Handler
	tcpListener   net.Listener
	udpConn       *net.UDPConn
	shutdown      chan struct{}
	shutdownOnce  sync.Once
	wg            sync.WaitGroup
	listenerReady chan struct{}
	connSemaphore chan struct{}
}

// NewServer creates a new portmapper server with the given configuration.
func NewServer(cfg ServerConfig) *Server {
	return &Server{
		config:        cfg,
		handler:       handlers.NewHandler(cfg.Registry),
		shutdown:      make(chan struct{}),
		listenerReady: make(chan struct{}),
		connSemaphore: make(chan struct{}, maxTCPConns),
	}
}

// Serve starts the portmapper server on both TCP and UDP.
// It blocks until the context is cancelled or Stop is called.
// After both TCP and UDP listeners are bound, WaitReady() unblocks.
func (s *Server) Serve(ctx context.Context) error {
	addr := fmt.Sprintf(":%d", s.config.Port)

	// Start TCP listener (unless disabled)
	if s.config.EnableTCP {
		tcpListener, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("listen TCP %s: %w", addr, err)
		}
		s.tcpListener = tcpListener
	}

	// Start UDP listener (unless disabled)
	if s.config.EnableUDP {
		udpAddr, err := net.ResolveUDPAddr("udp", addr)
		if err != nil {
			if s.tcpListener != nil {
				_ = s.tcpListener.Close()
			}
			return fmt.Errorf("resolve UDP %s: %w", addr, err)
		}
		udpConn, err := net.ListenUDP("udp", udpAddr)
		if err != nil {
			if s.tcpListener != nil {
				_ = s.tcpListener.Close()
			}
			return fmt.Errorf("listen UDP %s: %w", addr, err)
		}
		s.udpConn = udpConn
	}

	// Signal that listeners are ready
	close(s.listenerReady)

	logger.Info("Portmapper server started", "address", addr, "tcp", s.config.EnableTCP, "udp", s.config.EnableUDP)

	// Launch TCP and UDP goroutines
	if s.config.EnableTCP {
		s.wg.Add(1)
		go s.serveTCP(ctx)
	}
	if s.config.EnableUDP {
		s.wg.Add(1)
		go s.serveUDP(ctx)
	}

	// Monitor context cancellation
	go func() {
		select {
		case <-ctx.Done():
			s.Stop()
		case <-s.shutdown:
		}
	}()

	// Block until both goroutines complete
	s.wg.Wait()
	return nil
}

// WaitReady returns a channel that is closed when both TCP and UDP
// listeners are successfully bound and ready to accept requests.
// Callers should select on this channel with a timeout to detect
// startup failures.
func (s *Server) WaitReady() <-chan struct{} {
	return s.listenerReady
}

// serveTCP accepts TCP connections and handles them.
func (s *Server) serveTCP(ctx context.Context) {
	defer s.wg.Done()

	for {
		conn, err := s.tcpListener.Accept()
		if err != nil {
			select {
			case <-s.shutdown:
				return
			default:
				logger.Debug("Portmap TCP accept error", "error", err)
				return
			}
		}

		// Enforce connection limit via semaphore
		select {
		case s.connSemaphore <- struct{}{}:
			// Acquired slot
		default:
			// At limit, reject connection
			logger.Debug("Portmap: TCP connection limit reached, rejecting", "client", conn.RemoteAddr())
			_ = conn.Close()
			continue
		}

		s.wg.Add(1)
		go func(c net.Conn) {
			defer s.wg.Done()
			defer func() { <-s.connSemaphore }() // Release semaphore slot
			s.handleTCPConn(ctx, c)
		}(conn)
	}
}

// handleTCPConn handles a single TCP connection with support for connection reuse.
// TCP uses RPC record marking: a 4-byte fragment header (last-fragment bit + length)
// precedes each RPC message. The handler loops to support multiple requests per
// connection, breaking on EOF, error, or shutdown.
func (s *Server) handleTCPConn(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()

	clientAddr := conn.RemoteAddr().String()

	for {
		// Check for shutdown via context
		select {
		case <-ctx.Done():
			return
		case <-s.shutdown:
			return
		default:
		}

		// Set idle timeout per iteration (5s between requests)
		if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			logger.Debug("Portmap: failed to set deadline", "client", clientAddr, "error", err)
			return
		}

		// Read fragment header (4 bytes)
		var headerBuf [4]byte
		if _, err := io.ReadFull(conn, headerBuf[:]); err != nil {
			if err != io.EOF {
				// Timeout is expected for idle connections -- don't log
				var netErr net.Error
				if errors.As(err, &netErr) && netErr.Timeout() {
					return
				}
				logger.Debug("Portmap: read fragment header error", "client", clientAddr, "error", err)
			}
			return
		}

		headerVal := binary.BigEndian.Uint32(headerBuf[:])
		length := headerVal & 0x7FFFFFFF

		// Validate fragment size
		const maxFragmentSize = 1 << 16 // 64KB -- portmap messages are tiny
		if length > maxFragmentSize {
			logger.Warn("Portmap: fragment too large", "size", length, "client", clientAddr)
			return
		}

		// Read the RPC message body
		msgBuf := make([]byte, length)
		if _, err := io.ReadFull(conn, msgBuf); err != nil {
			logger.Debug("Portmap: read RPC message error", "client", clientAddr, "error", err)
			return
		}

		// Process and get reply body (without record marking)
		replyBody := s.processRPCMessage(msgBuf, clientAddr)
		if replyBody == nil {
			continue
		}

		// Write reply with record marking header
		reply := make([]byte, 4+len(replyBody))
		binary.BigEndian.PutUint32(reply[0:4], 0x80000000|uint32(len(replyBody)))
		copy(reply[4:], replyBody)

		if _, err := conn.Write(reply); err != nil {
			logger.Debug("Portmap: write TCP reply error", "client", clientAddr, "error", err)
			return
		}
	}
}

// serveUDP reads UDP packets and handles them.
// UDP has NO record marking -- each packet is one complete RPC message.
//
// Note: While CALLIT (procedure 5) is intentionally omitted to prevent DDoS
// amplification, other procedures (especially DUMP) can still produce responses
// larger than the request. In production deployments, consider firewalling the
// portmapper UDP port from external networks.
func (s *Server) serveUDP(_ context.Context) {
	defer s.wg.Done()

	buf := make([]byte, 65535) // Max UDP packet size

	for {
		select {
		case <-s.shutdown:
			return
		default:
		}

		// Set a short deadline so we can check for shutdown periodically
		if err := s.udpConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
			select {
			case <-s.shutdown:
				return
			default:
				logger.Debug("Portmap: set UDP deadline error", "error", err)
				continue
			}
		}

		n, clientAddr, err := s.udpConn.ReadFromUDP(buf)
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				continue // Normal timeout, check shutdown and retry
			}
			select {
			case <-s.shutdown:
				return
			default:
				logger.Debug("Portmap: UDP read error", "error", err)
				continue
			}
		}

		// Copy the data since buf will be reused
		msgBuf := make([]byte, n)
		copy(msgBuf, buf[:n])

		clientStr := clientAddr.String()

		// Process and get reply body (no record marking for UDP)
		replyBody := s.processRPCMessage(msgBuf, clientStr)
		if replyBody == nil {
			continue
		}

		// Send reply directly (no record marking header for UDP)
		if _, err := s.udpConn.WriteToUDP(replyBody, clientAddr); err != nil {
			logger.Debug("Portmap: write UDP reply error", "client", clientStr, "error", err)
		}
	}
}

// processRPCMessage parses an RPC message, dispatches to the appropriate handler,
// and returns the reply body (without record marking header).
// Returns nil if the message cannot be processed.
func (s *Server) processRPCMessage(data []byte, clientAddr string) []byte {
	// Parse RPC call
	call, err := rpc.ReadCall(data)
	if err != nil {
		logger.Debug("Portmap: parse RPC call error", "client", clientAddr, "error", err)
		return nil
	}

	// Validate program number
	if call.Program != types.ProgramPortmap {
		logger.Debug("Portmap: wrong program", "program", call.Program, "client", clientAddr)
		return makeErrorReplyBody(call.XID, rpc.RPCProgUnavail)
	}

	// Validate version
	if call.Version != types.PortmapVersion2 {
		logger.Debug("Portmap: version mismatch", "version", call.Version, "client", clientAddr)
		return makeProgMismatchReplyBody(call.XID, types.PortmapVersion2, types.PortmapVersion2)
	}

	// Look up procedure in dispatch table
	proc, ok := DispatchTable[call.Procedure]
	if !ok {
		logger.Debug("Portmap: procedure unavailable", "procedure", call.Procedure, "client", clientAddr)
		return makeErrorReplyBody(call.XID, rpc.RPCProcUnavail)
	}

	// Extract procedure data
	procData, err := rpc.ReadData(data, call)
	if err != nil {
		logger.Debug("Portmap: read procedure data error", "client", clientAddr, "error", err)
		return nil
	}

	logger.Debug("Portmap RPC", "procedure", proc.Name, "client", clientAddr)

	// Call handler
	respData, err := proc.Handler(s.handler, procData, clientAddr)
	if err != nil {
		logger.Debug("Portmap: handler error", "procedure", proc.Name, "client", clientAddr, "error", err)
		// Still return the response data if available (e.g., false for SET with bad decode)
		if respData != nil {
			return makeSuccessReplyBody(call.XID, respData)
		}
		return makeErrorReplyBody(call.XID, rpc.RPCSystemErr)
	}

	return makeSuccessReplyBody(call.XID, respData)
}

// Stop gracefully shuts down the portmapper server and waits for all
// goroutines (TCP accept loop, UDP loop, and active connection handlers)
// to complete before returning.
func (s *Server) Stop() {
	s.shutdownOnce.Do(func() {
		close(s.shutdown)
		if s.tcpListener != nil {
			_ = s.tcpListener.Close()
		}
		if s.udpConn != nil {
			_ = s.udpConn.Close()
		}
	})
	// Wait for all goroutines to finish
	s.wg.Wait()
}

// Addr returns the TCP listener address (for tests).
// Returns empty string if the server is not listening.
func (s *Server) Addr() string {
	if s.tcpListener != nil {
		return s.tcpListener.Addr().String()
	}
	return ""
}

// UDPAddr returns the UDP listener address (for tests).
// Returns empty string if the server is not listening.
func (s *Server) UDPAddr() string {
	if s.udpConn != nil {
		return s.udpConn.LocalAddr().String()
	}
	return ""
}

// ============================================================================
// Reply construction helpers (without record marking -- transport adds it)
// ============================================================================

// makeSuccessReplyBody builds an RPC success reply body (no record marking).
//
// Wire format:
//
//	xid(4) + msg_type=1(4) + reply_state=0(4) + verf_flavor=0(4) + verf_len=0(4) + accept_stat=0(4) + data
func makeSuccessReplyBody(xid uint32, data []byte) []byte {
	// Header: 6 uint32 fields = 24 bytes
	buf := make([]byte, 24+len(data))

	binary.BigEndian.PutUint32(buf[0:4], xid)
	binary.BigEndian.PutUint32(buf[4:8], rpc.RPCReply)        // msg_type = REPLY
	binary.BigEndian.PutUint32(buf[8:12], rpc.RPCMsgAccepted) // reply_state = MSG_ACCEPTED
	binary.BigEndian.PutUint32(buf[12:16], 0)                 // verf_flavor = AUTH_NULL
	binary.BigEndian.PutUint32(buf[16:20], 0)                 // verf_len = 0
	binary.BigEndian.PutUint32(buf[20:24], rpc.RPCSuccess)    // accept_stat = SUCCESS

	copy(buf[24:], data)
	return buf
}

// makeErrorReplyBody builds an RPC error reply body (no record marking).
//
// Wire format:
//
//	xid(4) + msg_type=1(4) + reply_state=0(4) + verf_flavor=0(4) + verf_len=0(4) + accept_stat(4)
func makeErrorReplyBody(xid uint32, acceptStat uint32) []byte {
	buf := make([]byte, 24)

	binary.BigEndian.PutUint32(buf[0:4], xid)
	binary.BigEndian.PutUint32(buf[4:8], rpc.RPCReply)
	binary.BigEndian.PutUint32(buf[8:12], rpc.RPCMsgAccepted)
	binary.BigEndian.PutUint32(buf[12:16], 0) // verf_flavor = AUTH_NULL
	binary.BigEndian.PutUint32(buf[16:20], 0) // verf_len = 0
	binary.BigEndian.PutUint32(buf[20:24], acceptStat)

	return buf
}

// makeProgMismatchReplyBody builds an RPC PROG_MISMATCH reply body (no record marking).
//
// Wire format:
//
//	xid(4) + msg_type=1(4) + reply_state=0(4) + verf_flavor=0(4) + verf_len=0(4) + accept_stat=2(4) + low(4) + high(4)
func makeProgMismatchReplyBody(xid uint32, low, high uint32) []byte {
	buf := make([]byte, 32)

	binary.BigEndian.PutUint32(buf[0:4], xid)
	binary.BigEndian.PutUint32(buf[4:8], rpc.RPCReply)
	binary.BigEndian.PutUint32(buf[8:12], rpc.RPCMsgAccepted)
	binary.BigEndian.PutUint32(buf[12:16], 0)                   // verf_flavor = AUTH_NULL
	binary.BigEndian.PutUint32(buf[16:20], 0)                   // verf_len = 0
	binary.BigEndian.PutUint32(buf[20:24], rpc.RPCProgMismatch) // accept_stat = PROG_MISMATCH
	binary.BigEndian.PutUint32(buf[24:28], low)
	binary.BigEndian.PutUint32(buf[28:32], high)

	return buf
}

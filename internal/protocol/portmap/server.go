package portmap

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/rpc"
	"github.com/marmos91/dittofs/internal/protocol/portmap/handlers"
	"github.com/marmos91/dittofs/internal/protocol/portmap/types"
)

// ServerConfig holds configuration for the portmapper server.
type ServerConfig struct {
	// Port is the port to listen on (default 111 per RFC 1057).
	Port int

	// Registry is the service registry used by procedure handlers.
	Registry *Registry
}

// Server implements an RFC 1057 portmapper that listens on both TCP and UDP.
//
// TCP uses RPC record marking (4-byte fragment header).
// UDP treats each packet as one complete RPC message (no record marking).
type Server struct {
	config       ServerConfig
	handler      *handlers.Handler
	tcpListener  net.Listener
	udpConn      *net.UDPConn
	shutdown     chan struct{}
	shutdownOnce sync.Once
	wg           sync.WaitGroup
}

// NewServer creates a new portmapper server with the given configuration.
func NewServer(cfg ServerConfig) *Server {
	return &Server{
		config:   cfg,
		handler:  handlers.NewHandler(cfg.Registry),
		shutdown: make(chan struct{}),
	}
}

// Serve starts the portmapper server on both TCP and UDP.
// It blocks until the context is cancelled or Stop is called.
func (s *Server) Serve(ctx context.Context) error {
	addr := fmt.Sprintf(":%d", s.config.Port)

	// Start TCP listener
	tcpListener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen TCP %s: %w", addr, err)
	}
	s.tcpListener = tcpListener

	// Start UDP listener
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		_ = s.tcpListener.Close()
		return fmt.Errorf("resolve UDP %s: %w", addr, err)
	}
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		_ = s.tcpListener.Close()
		return fmt.Errorf("listen UDP %s: %w", addr, err)
	}
	s.udpConn = udpConn

	logger.Info("Portmapper server started", "address", addr)

	// Launch TCP and UDP goroutines
	s.wg.Add(2)
	go s.serveTCP(ctx)
	go s.serveUDP(ctx)

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

		s.wg.Add(1)
		go func(c net.Conn) {
			defer s.wg.Done()
			s.handleTCPConn(ctx, c)
		}(conn)
	}
}

// handleTCPConn handles a single TCP connection.
// TCP uses RPC record marking: a 4-byte fragment header (last-fragment bit + length)
// precedes each RPC message.
func (s *Server) handleTCPConn(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()

	clientAddr := conn.RemoteAddr().String()

	// Set reasonable read/write timeout
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		logger.Debug("Portmap: failed to set deadline", "client", clientAddr, "error", err)
		return
	}

	// Read fragment header (4 bytes)
	var headerBuf [4]byte
	if _, err := io.ReadFull(conn, headerBuf[:]); err != nil {
		if err != io.EOF {
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
		return
	}

	// Write reply with record marking header
	reply := make([]byte, 4+len(replyBody))
	binary.BigEndian.PutUint32(reply[0:4], 0x80000000|uint32(len(replyBody)))
	copy(reply[4:], replyBody)

	if _, err := conn.Write(reply); err != nil {
		logger.Debug("Portmap: write TCP reply error", "client", clientAddr, "error", err)
	}
}

// serveUDP reads UDP packets and handles them.
// UDP has NO record marking -- each packet is one complete RPC message.
func (s *Server) serveUDP(ctx context.Context) {
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
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
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
		reply := makeErrorReplyBody(call.XID, rpc.RPCProgMismatch)
		return reply
	}

	// Validate version
	if call.Version != types.PortmapVersion2 {
		logger.Debug("Portmap: version mismatch", "version", call.Version, "client", clientAddr)
		reply := makeProgMismatchReplyBody(call.XID, types.PortmapVersion2, types.PortmapVersion2)
		return reply
	}

	// Look up procedure in dispatch table
	proc, ok := DispatchTable[call.Procedure]
	if !ok {
		logger.Debug("Portmap: procedure unavailable", "procedure", call.Procedure, "client", clientAddr)
		reply := makeErrorReplyBody(call.XID, rpc.RPCProcUnavail)
		return reply
	}

	// Extract procedure data
	procData, err := rpc.ReadData(data, call)
	if err != nil {
		logger.Debug("Portmap: read procedure data error", "client", clientAddr, "error", err)
		return nil
	}

	logger.Debug("Portmap RPC", "procedure", proc.Name, "client", clientAddr)

	// Call handler
	result, err := proc.Handler(s.handler, procData)
	if err != nil {
		logger.Debug("Portmap: handler error", "procedure", proc.Name, "client", clientAddr, "error", err)
		// Still return the result data if available (e.g., false for SET with bad decode)
		if result != nil && result.Data != nil {
			return makeSuccessReplyBody(call.XID, result.Data)
		}
		reply := makeErrorReplyBody(call.XID, rpc.RPCSystemErr)
		return reply
	}

	return makeSuccessReplyBody(call.XID, result.Data)
}

// Stop gracefully shuts down the portmapper server.
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

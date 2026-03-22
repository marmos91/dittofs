package nfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime/debug"
	"sync"
	"time"

	nfs_internal "github.com/marmos91/dittofs/internal/adapter/nfs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/adapter/pool"
	"github.com/marmos91/dittofs/internal/bytesize"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/clients"
)

// errBackchannelReply is a sentinel error returned by readRequest when the
// incoming message is a backchannel REPLY (msg_type=1) rather than a CALL.
var errBackchannelReply = errors.New("backchannel reply routed")

// NFSConnection handles a single NFS client TCP connection.
// Requests are read sequentially from the wire but dispatched concurrently
// via goroutines, bounded by requestSem. Replies are serialized by writeMu.
type NFSConnection struct {
	server *NFSAdapter
	conn   net.Conn

	connectionID uint64
	clientID     string // registry key for ClientRegistry

	requestSem chan struct{}
	wg         sync.WaitGroup
	writeMu    sync.Mutex

	// pendingCBReplies routes NFSv4.1 backchannel REPLY messages.
	// nil unless the connection is bound for back-channel.
	pendingCBReplies *state.PendingCBReplies
}

func NewNFSConnection(server *NFSAdapter, conn net.Conn, connectionID uint64) *NFSConnection {
	return &NFSConnection{
		server:       server,
		conn:         conn,
		connectionID: connectionID,
		requestSem:   make(chan struct{}, server.config.MaxRequestsPerConnection),
	}
}

// SetPendingCBReplies enables backchannel REPLY demuxing on this connection.
func (c *NFSConnection) SetPendingCBReplies(p *state.PendingCBReplies) {
	c.pendingCBReplies = p
}

// Serve runs the read loop for this connection. It reads RPC requests
// sequentially from the wire and dispatches each concurrently via a goroutine.
// The loop exits on shutdown, timeout, EOF, or unrecoverable error.
func (c *NFSConnection) Serve(ctx context.Context) {
	defer c.handleConnectionClose()

	clientAddr := c.conn.RemoteAddr().String()
	logger.Debug("New connection", "address", clientAddr)

	// Register with the client registry for operational visibility.
	c.clientID = fmt.Sprintf("nfs-%d", c.connectionID)
	if rt := c.server.Registry; rt != nil {
		rt.Clients().Register(&clients.ClientRecord{
			ClientID: c.clientID,
			Protocol: "nfs",
			Address:  clientAddr,
			NFS:      &clients.NfsDetails{Version: "3"},
		})
	}

	c.resetIdleTimeout(clientAddr)

	for {
		if c.isShuttingDown(ctx, clientAddr) {
			return
		}

		call, rawMessage, err := c.readRequest(ctx)
		if err != nil {
			if errors.Is(err, errBackchannelReply) {
				continue
			}
			c.logReadError(err, clientAddr)
			return
		}

		c.dispatchRequest(ctx, clientAddr, call, rawMessage)
		c.resetIdleTimeout(clientAddr)
	}
}

// isShuttingDown checks if the connection should close due to context
// cancellation or server shutdown.
func (c *NFSConnection) isShuttingDown(ctx context.Context, clientAddr string) bool {
	select {
	case <-ctx.Done():
		logger.Debug("Connection closed due to context cancellation", "address", clientAddr)
		return true
	case <-c.server.Shutdown:
		logger.Debug("Connection closed due to server shutdown", "address", clientAddr)
		return true
	default:
		return false
	}
}

// logReadError logs a connection read error at the appropriate level.
func (c *NFSConnection) logReadError(err error, clientAddr string) {
	switch {
	case err == io.EOF:
		logger.Debug("Connection closed by client", "address", clientAddr)
	case isNetTimeout(err):
		logger.Debug("Connection timed out", "address", clientAddr, "error", err)
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		logger.Debug("Connection cancelled", "address", clientAddr, "error", err)
	default:
		logger.Debug("Error reading request", "address", clientAddr, "error", err)
	}
}

// isNetTimeout returns true if err is a network timeout.
func isNetTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// dispatchRequest launches an RPC call handler in a goroutine for concurrent
// processing. Bounded by requestSem to limit memory usage. NFS clients use XIDs
// for request/response matching, so out-of-order replies are safe. Replies are
// serialized on the wire by writeMu. This mirrors kernel nfsd's thread pool model
// and allows WRITE+COMMIT to overlap on the same TCP connection.
func (c *NFSConnection) dispatchRequest(ctx context.Context, clientAddr string, call *rpc.RPCCallMessage, rawMessage []byte) {
	// Update activity timestamp for the client registry.
	if rt := c.server.Registry; rt != nil && c.clientID != "" {
		rt.Clients().UpdateActivity(c.clientID)
	}

	c.requestSem <- struct{}{}
	c.wg.Add(1)

	go func(call *rpc.RPCCallMessage, rawMessage []byte) {
		defer c.handleRequestPanic(clientAddr, call.XID)
		defer pool.Put(rawMessage)

		if err := c.processRequest(ctx, call, rawMessage); err != nil {
			logger.Debug("Error processing request",
				"address", clientAddr,
				"xid", fmt.Sprintf("0x%x", call.XID),
				"error", err)
		}
	}(call, rawMessage)
}

// resetIdleTimeout resets the connection deadline if an idle timeout is configured.
func (c *NFSConnection) resetIdleTimeout(clientAddr string) {
	if c.server.config.Timeouts.Idle > 0 {
		if err := c.conn.SetDeadline(time.Now().Add(c.server.config.Timeouts.Idle)); err != nil {
			logger.Warn("Failed to set deadline", "address", clientAddr, "error", err)
		}
	}
}

// readRequest reads and parses an RPC request from the connection.
// The returned rawMessage is a pooled buffer — the caller must return it
// via pool.Put() after processing.
func (c *NFSConnection) readRequest(ctx context.Context) (*rpc.RPCCallMessage, []byte, error) {
	select {
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	default:
	}

	if c.server.config.Timeouts.Read > 0 {
		if err := c.conn.SetReadDeadline(time.Now().Add(c.server.config.Timeouts.Read)); err != nil {
			return nil, nil, fmt.Errorf("set read deadline: %w", err)
		}
	}

	header, err := nfs_internal.ReadFragmentHeader(c.conn)
	if err != nil {
		if err != io.EOF {
			logger.Debug("Error reading fragment header", "address", c.conn.RemoteAddr().String(), "error", err)
		}
		return nil, nil, err
	}
	logger.Debug("Read fragment header", "address", c.conn.RemoteAddr().String(), "last", header.IsLast, "length", bytesize.ByteSize(header.Length))

	if err := nfs_internal.ValidateFragmentSize(header.Length, c.conn.RemoteAddr().String()); err != nil {
		return nil, nil, err
	}

	select {
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	default:
	}

	message, err := nfs_internal.ReadRPCMessage(c.conn, header.Length)
	if err != nil {
		return nil, nil, fmt.Errorf("read RPC message: %w", err)
	}

	if nfs_internal.DemuxBackchannelReply(message, c.connectionID, c.pendingCBReplies) {
		return nil, nil, errBackchannelReply
	}

	call, err := rpc.ReadCall(message)
	if err != nil {
		pool.Put(message)
		logger.Debug("Error parsing RPC call", "error", err)
		return nil, nil, err
	}

	logger.Debug("RPC Call", "xid", fmt.Sprintf("0x%x", call.XID), "program", call.Program, "version", call.Version, "procedure", call.Procedure)
	return call, message, nil
}

// processRequest extracts procedure data from the raw message and dispatches
// to the appropriate RPC handler. Designed to run in a goroutine.
func (c *NFSConnection) processRequest(ctx context.Context, call *rpc.RPCCallMessage, rawMessage []byte) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	procedureData, err := rpc.ReadData(rawMessage, call)
	if err != nil {
		return fmt.Errorf("extract procedure data: %w", err)
	}

	return c.handleRPCCall(ctx, call, procedureData)
}

// handleUnsupportedVersion sends an RFC 5531 PROG_MISMATCH reply for
// unrecognized NFS/Mount protocol versions.
func (c *NFSConnection) handleUnsupportedVersion(call *rpc.RPCCallMessage, lowVersion, highVersion uint32, programName string, clientAddr string) error {
	logger.Warn("Unsupported "+programName+" version",
		"requested", call.Version,
		"supported_low", lowVersion,
		"supported_high", highVersion,
		"xid", fmt.Sprintf("0x%x", call.XID),
		"client", clientAddr)

	mismatchReply, err := rpc.MakeProgMismatchReply(call.XID, lowVersion, highVersion)
	if err != nil {
		return fmt.Errorf("make version mismatch reply: %w", err)
	}
	return c.writeReply(call.XID, mismatchReply)
}

// handleConnectionClose recovers from panics, waits for in-flight requests,
// and closes the TCP connection. Called via defer in Serve.
func (c *NFSConnection) handleConnectionClose() {
	if r := recover(); r != nil {
		stack := string(debug.Stack())
		logger.Error("Panic in connection handler",
			"address", c.conn.RemoteAddr().String(),
			"error", r,
			"stack", stack)
	}

	c.wg.Wait()

	// Deregister from the client registry.
	if rt := c.server.Registry; rt != nil && c.clientID != "" {
		rt.Clients().Deregister(c.clientID)
	}

	_ = c.conn.Close()
}

// handleRequestPanic releases the semaphore slot, decrements the WaitGroup,
// and recovers from panics in individual request handlers.
func (c *NFSConnection) handleRequestPanic(clientAddr string, xid uint32) {
	<-c.requestSem
	c.wg.Done()

	if r := recover(); r != nil {
		stack := string(debug.Stack())
		logger.Error("Panic in request handler",
			"address", clientAddr,
			"xid", fmt.Sprintf("0x%x", xid),
			"error", r,
			"stack", stack)
	}
}

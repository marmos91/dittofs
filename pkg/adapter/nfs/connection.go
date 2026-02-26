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
)

// errBackchannelReply is a sentinel error returned by readRequest when the
// incoming message is a backchannel REPLY (msg_type=1) rather than a CALL.
// The Serve loop checks for this and continues without treating it as an error.
var errBackchannelReply = errors.New("backchannel reply routed")

type NFSConnection struct {
	server *NFSAdapter
	conn   net.Conn

	// connectionID is the unique identifier for this connection, assigned at accept() time.
	connectionID uint64

	// Concurrent request handling
	requestSem chan struct{}  // Semaphore to limit concurrent requests
	wg         sync.WaitGroup // Track active requests for graceful shutdown
	writeMu    sync.Mutex     // Protects connection writes (replies must be serialized)

	// pendingCBReplies routes backchannel REPLY messages to the sender goroutine.
	// nil by default; set when the connection is bound for back-channel via
	// SetPendingCBReplies. When non-nil, the read loop demuxes incoming messages:
	// CALL (msg_type=0) dispatches to handlers, REPLY (msg_type=1) routes here.
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

// SetPendingCBReplies sets the PendingCBReplies instance for this connection,
// enabling the read-loop demux to route backchannel REPLY messages to the
// BackchannelSender goroutine. Called when a connection is bound for back-channel.
func (c *NFSConnection) SetPendingCBReplies(p *state.PendingCBReplies) {
	c.pendingCBReplies = p
}

// serve handles all RPC requests for this connection.
// It implements panic recovery to prevent a single misbehaving connection
// from crashing the entire server.
//
// The connection is automatically closed when:
// - The context is cancelled (server shutdown)
// - An idle timeout occurs
// - A read or write timeout occurs
// - An unrecoverable error occurs
// - The client closes the connection
//
// Context cancellation is checked at the beginning of each request loop,
// ensuring graceful shutdown and proper cleanup of resources.
func (c *NFSConnection) Serve(ctx context.Context) {
	defer c.handleConnectionClose()

	clientAddr := c.conn.RemoteAddr().String()
	logger.Debug("New connection", "address", clientAddr)

	// Set initial idle timeout
	if c.server.config.Timeouts.Idle > 0 {
		if err := c.conn.SetDeadline(time.Now().Add(c.server.config.Timeouts.Idle)); err != nil {
			logger.Warn("Failed to set deadline", "address", clientAddr, "error", err)
		}
	}

	for {
		// Check for context cancellation before processing next request
		// This provides graceful shutdown capability
		select {
		case <-ctx.Done():
			logger.Debug("Connection closed due to context cancellation", "address", clientAddr)
			return
		case <-c.server.Shutdown:
			logger.Debug("Connection closed due to server shutdown", "address", clientAddr)
			return
		default:
		}

		// Read the request (blocks until data available)
		// This is done synchronously to maintain request order on the wire
		call, rawMessage, err := c.readRequest(ctx)
		if err != nil {
			// Backchannel REPLY messages are routed internally; continue loop
			if errors.Is(err, errBackchannelReply) {
				continue
			}
			if err == io.EOF {
				logger.Debug("Connection closed by client", "address", clientAddr)
			} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				logger.Debug("Connection timed out", "address", clientAddr, "error", err)
			} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				logger.Debug("Connection cancelled", "address", clientAddr, "error", err)
			} else {
				logger.Debug("Error reading request", "address", clientAddr, "error", err)
			}
			return
		}

		// Acquire semaphore slot (blocks if at limit)
		c.requestSem <- struct{}{}

		// Process request synchronously to maintain POSIX ordering semantics.
		// NFS clients send requests sequentially for dependent operations (e.g., chown
		// followed by rename). Processing them in parallel can cause TOCTOU races where
		// a later operation checks stale metadata (e.g., sticky bit check sees old UID).
		// NOTE: rawMessage is a pooled buffer - must be returned via pool.Put()
		c.wg.Add(1)
		func(call *rpc.RPCCallMessage, rawMessage []byte) {
			defer c.handleRequestPanic(clientAddr, call.XID)
			defer pool.Put(rawMessage) // Return pooled buffer after processing

			// Process and send reply
			if err := c.processRequest(ctx, call, rawMessage); err != nil {
				logger.Debug("Error processing request", "address", clientAddr, "xid", fmt.Sprintf("0x%x", call.XID), "error", err)
			}
		}(call, rawMessage)

		// Reset idle timeout after reading request
		if c.server.config.Timeouts.Idle > 0 {
			if err := c.conn.SetDeadline(time.Now().Add(c.server.config.Timeouts.Idle)); err != nil {
				logger.Warn("Failed to reset deadline", "address", clientAddr, "error", err)
			}
		}
	}
}

// readRequest reads and parses an RPC request from the connection.
//
// This reads the fragment header, validates the message size, reads the RPC message,
// and parses the RPC header. The pooled buffer is NOT returned to the pool here -
// the caller is responsible for returning it via pool.Put() after processing.
//
// Returns:
//   - call: The parsed RPC call message (for routing and XID)
//   - rawMessage: The complete raw RPC message (pooled buffer - caller must Put)
//   - error: Any error that occurred during reading
func (c *NFSConnection) readRequest(ctx context.Context) (*rpc.RPCCallMessage, []byte, error) {
	// Check context before starting request processing
	select {
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	default:
	}

	// Apply read timeout if configured
	if c.server.config.Timeouts.Read > 0 {
		deadline := time.Now().Add(c.server.config.Timeouts.Read)
		if err := c.conn.SetReadDeadline(deadline); err != nil {
			return nil, nil, fmt.Errorf("set read deadline: %w", err)
		}
	}

	// Read fragment header using shared RPC framing utility
	header, err := nfs_internal.ReadFragmentHeader(c.conn)
	if err != nil {
		// Don't log EOF as an error - it's a normal client disconnect
		if err != io.EOF {
			logger.Debug("Error reading fragment header", "address", c.conn.RemoteAddr().String(), "error", err)
		}
		return nil, nil, err
	}
	logger.Debug("Read fragment header", "address", c.conn.RemoteAddr().String(), "last", header.IsLast, "length", bytesize.ByteSize(header.Length))

	// Validate fragment size to prevent memory exhaustion
	if err := nfs_internal.ValidateFragmentSize(header.Length, c.conn.RemoteAddr().String()); err != nil {
		return nil, nil, err
	}

	// Check context before reading potentially large message
	select {
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	default:
	}

	// Read RPC message using shared utility (uses buffer pool)
	// NOTE: Caller is responsible for returning buffer via pool.Put()
	message, err := nfs_internal.ReadRPCMessage(c.conn, header.Length)
	if err != nil {
		return nil, nil, fmt.Errorf("read RPC message: %w", err)
	}

	// Demux: check if this is a backchannel REPLY (NFSv4.1 multiplexing).
	// The shared connection helper handles the backchannel reply routing.
	if nfs_internal.DemuxBackchannelReply(message, c.connectionID, c.pendingCBReplies) {
		return nil, nil, errBackchannelReply
	}

	// Parse RPC call header
	call, err := rpc.ReadCall(message)
	if err != nil {
		pool.Put(message) // Return buffer on error
		logger.Debug("Error parsing RPC call", "error", err)
		return nil, nil, err
	}

	logger.Debug("RPC Call", "xid", fmt.Sprintf("0x%x", call.XID), "program", call.Program, "version", call.Version, "procedure", call.Procedure)

	// Return pooled buffer directly - no copy needed
	// Caller must return buffer to pool via pool.Put() after processing
	return call, message, nil
}

// processRequest processes an RPC request and sends the reply.
//
// This takes a pre-parsed RPC call and raw message, extracts procedure data,
// dispatches to the appropriate handler, and sends the reply.
//
// This method is designed to be called in a goroutine for parallel processing.
// The RPC header has already been parsed by readRequest to avoid double parsing.
func (c *NFSConnection) processRequest(ctx context.Context, call *rpc.RPCCallMessage, rawMessage []byte) error {
	// Check context before processing
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// Extract procedure data (RPC header already parsed)
	procedureData, err := rpc.ReadData(rawMessage, call)
	if err != nil {
		return fmt.Errorf("extract procedure data: %w", err)
	}

	// Dispatch to handler (this is where the real work happens - COMMIT, etc.)
	return c.handleRPCCall(ctx, call, procedureData)
}

// handleUnsupportedVersion handles version mismatch for NFS/Mount protocols.
//
// This method logs a warning about the unsupported version and sends an
// RFC 5531-compliant PROG_MISMATCH reply indicating the supported version range.
//
// Parameters:
//   - call: The RPC call with the unsupported version
//   - lowVersion: The lowest supported version
//   - highVersion: The highest supported version
//   - programName: Name for logging ("NFS" or "Mount")
//   - clientAddr: Client address for logging
//
// Returns:
//   - error: Always returns an error after sending PROG_MISMATCH
func (c *NFSConnection) handleUnsupportedVersion(call *rpc.RPCCallMessage, lowVersion, highVersion uint32, programName string, clientAddr string) error {
	logger.Warn("Unsupported "+programName+" version",
		"requested", call.Version,
		"supported_low", lowVersion,
		"supported_high", highVersion,
		"xid", fmt.Sprintf("0x%x", call.XID),
		"client", clientAddr)

	// Per RFC 5531, respond with PROG_MISMATCH for unsupported versions
	mismatchReply, err := rpc.MakeProgMismatchReply(call.XID, lowVersion, highVersion)
	if err != nil {
		return fmt.Errorf("make version mismatch reply: %w", err)
	}
	return c.writeReply(call.XID, mismatchReply)
}

// handleConnectionClose handles cleanup and panic recovery for the connection.
// This is called as a deferred function in Serve to ensure proper cleanup
// even if a panic occurs. It:
//  1. Recovers from any panics in the connection handler
//  2. Waits for all in-flight requests to complete
//  3. Closes the connection
func (c *NFSConnection) handleConnectionClose() {
	// Panic recovery - prevents a single connection from crashing the server
	if r := recover(); r != nil {
		stack := string(debug.Stack())
		logger.Error("Panic in connection handler",
			"address", c.conn.RemoteAddr().String(),
			"error", r,
			"stack", stack)
	}

	// Wait for all in-flight requests to complete before closing connection
	c.wg.Wait()
	_ = c.conn.Close()
}

// handleRequestPanic handles cleanup and panic recovery for individual requests.
// This is called as a deferred function in the request processing to:
//  1. Release the semaphore slot
//  2. Decrement the wait group counter
//  3. Recover from any panics in the request handler
func (c *NFSConnection) handleRequestPanic(clientAddr string, xid uint32) {
	<-c.requestSem // Release semaphore slot
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

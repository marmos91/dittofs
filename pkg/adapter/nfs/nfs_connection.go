package nfs

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	nfs "github.com/marmos91/dittofs/internal/protocol/nfs"
	mount_handlers "github.com/marmos91/dittofs/internal/protocol/nfs/mount/handlers"
	"github.com/marmos91/dittofs/internal/protocol/nfs/rpc"
	nfs_types "github.com/marmos91/dittofs/internal/protocol/nfs/types"
	handlers "github.com/marmos91/dittofs/internal/protocol/nfs/v3/handlers"
	"github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
)

type NFSConnection struct {
	server *NFSAdapter
	conn   net.Conn
}

type fragmentHeader struct {
	IsLast bool
	Length uint32
}

func NewNFSConnection(server *NFSAdapter, conn net.Conn) *NFSConnection {
	return &NFSConnection{
		server,
		conn,
	}
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
	defer func() {
		// Panic recovery - prevents a single connection from crashing the server
		if r := recover(); r != nil {
			logger.Error("Panic in connection handler from %s: %v",
				c.conn.RemoteAddr().String(), r)
		}
		_ = c.conn.Close()
	}()

	clientAddr := c.conn.RemoteAddr().String()
	logger.Debug("New connection from %s", clientAddr)

	// Set initial idle timeout
	if c.server.config.Timeouts.Idle > 0 {
		if err := c.conn.SetDeadline(time.Now().Add(c.server.config.Timeouts.Idle)); err != nil {
			logger.Warn("Failed to set deadline for %s: %v", clientAddr, err)
		}
	}

	for {
		// Check for context cancellation before processing next request
		// This provides graceful shutdown capability
		select {
		case <-ctx.Done():
			logger.Debug("Connection from %s closed due to context cancellation", clientAddr)
			return
		case <-c.server.shutdown:
			logger.Debug("Connection from %s closed due to server shutdown", clientAddr)
			return
		default:
		}

		err := c.handleRequest(ctx)

		if err != nil {
			if err == io.EOF {
				logger.Debug("Connection from %s closed by client", clientAddr)
			} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				logger.Debug("Connection from %s timed out: %v", clientAddr, err)
			} else if err == context.Canceled || err == context.DeadlineExceeded {
				logger.Debug("Connection from %s cancelled: %v", clientAddr, err)
			} else {
				logger.Debug("Error handling request from %s: %v", clientAddr, err)
			}
			return
		}

		// Reset idle timeout after successful request
		if c.server.config.Timeouts.Idle > 0 {
			if err := c.conn.SetDeadline(time.Now().Add(c.server.config.Timeouts.Idle)); err != nil {
				logger.Warn("Failed to reset deadline for %s: %v", clientAddr, err)
			}
		}
	}
}

// handleRequest processes a single RPC request.
//
// It reads the fragment header, validates the message size, reads the RPC message,
// parses it, and dispatches it to the appropriate handler.
//
// The context is passed through to handlers to enable cancellation of long-running
// operations.
//
// Returns an error if:
// - Context is cancelled
// - Network error occurs
// - Message is malformed or too large
// - Handler returns an error
func (c *NFSConnection) handleRequest(ctx context.Context) error {
	// Check context before starting request processing
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// Apply read timeout if configured
	if c.server.config.Timeouts.Read > 0 {
		deadline := time.Now().Add(c.server.config.Timeouts.Read)
		if err := c.conn.SetReadDeadline(deadline); err != nil {
			return fmt.Errorf("set read deadline: %w", err)
		}
	}

	// Read fragment header
	header, err := c.readFragmentHeader()
	if err != nil {
		// Don't log EOF as an error - it's a normal client disconnect
		if err != io.EOF {
			logger.Debug("Error reading fragment header from %s: %v", c.conn.RemoteAddr().String(), err)
		}
		return err
	}
	logger.Debug("Read fragment header from %s: last=%v length=%d", c.conn.RemoteAddr().String(), header.IsLast, header.Length)

	// Validate fragment size to prevent memory exhaustion
	const maxFragmentSize = 1 << 20 // 1MB - NFS messages are typically much smaller
	if header.Length > maxFragmentSize {
		logger.Warn("Fragment size %d exceeds maximum %d from %s",
			header.Length, maxFragmentSize, c.conn.RemoteAddr().String())
		return fmt.Errorf("fragment too large: %d bytes", header.Length)
	}

	// Check context before reading potentially large message
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// Read RPC message (now uses buffer pool)
	message, err := c.readRPCMessage(header.Length)
	if err != nil {
		return fmt.Errorf("read RPC message: %w", err)
	}
	// NOTE: Buffer is returned AFTER handler completes to allow zero-copy
	// operations where procedureData is a slice into the original buffer
	defer nfs.PutBuffer(message)

	// Parse RPC call
	call, err := rpc.ReadCall(message)
	if err != nil {
		logger.Debug("Error parsing RPC call: %v", err)
		return nil
	}

	logger.Debug("RPC Call: XID=0x%x Program=%d Version=%d Procedure=%d",
		call.XID, call.Program, call.Version, call.Procedure)

	// Extract procedure data (returns slice into message buffer - zero-copy)
	procedureData, err := rpc.ReadData(message, call)
	if err != nil {
		return fmt.Errorf("extract procedure data: %w", err)
	}

	// Check context before dispatching to handler
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// Handle the call with context
	// IMPORTANT: procedureData is a slice into the pooled message buffer
	// The buffer will be returned to the pool when this function exits
	return c.handleRPCCall(ctx, call, procedureData)
}

// readFragmentHeader reads the 4-byte RPC fragment header.
//
// The fragment header contains:
// - Bit 31: Last fragment flag (1 = last, 0 = more fragments)
// - Bits 0-30: Fragment length in bytes
//
// Returns the parsed header or an error if reading fails.
func (c *NFSConnection) readFragmentHeader() (*fragmentHeader, error) {
	var buf [4]byte
	_, err := io.ReadFull(c.conn, buf[:])
	if err != nil {
		return nil, err
	}

	header := binary.BigEndian.Uint32(buf[:])
	return &fragmentHeader{
		IsLast: (header & 0x80000000) != 0,
		Length: header & 0x7FFFFFFF,
	}, nil
}

// readRPCMessage reads an RPC message of the specified length.
//
// It uses a buffer pool to reduce allocations for frequently sized messages.
// The caller is responsible for returning the buffer to the pool via PutBuffer.
//
// Returns the message buffer or an error if reading fails.
func (c *NFSConnection) readRPCMessage(length uint32) ([]byte, error) {
	// Get buffer from pool
	message := nfs.GetBuffer(length)

	// Read directly into pooled buffer
	_, err := io.ReadFull(c.conn, message)
	if err != nil {
		// Return buffer to pool on error
		nfs.PutBuffer(message)
		return nil, fmt.Errorf("read message: %w", err)
	}

	return message, nil
}

// handleRPCCall dispatches an RPC call to the appropriate handler.
//
// It routes calls to either NFS or MOUNT handlers based on the program number,
// records metrics, and sends the reply back to the client.
//
// The context is passed through to handlers to enable cancellation of
// long-running operations like large file reads/writes or directory scans.
//
// Returns an error if:
// - Context is cancelled during processing
// - Handler returns an error
// - Reply cannot be sent
func (c *NFSConnection) handleRPCCall(ctx context.Context, call *rpc.RPCCallMessage, procedureData []byte) error {
	var replyData []byte
	var err error

	clientAddr := c.conn.RemoteAddr().String()

	logger.Debug("RPC Call Details: Program=%d Version=%d Procedure=%d",
		call.Program, call.Version, call.Procedure)

	// Check context before dispatching to handler
	select {
	case <-ctx.Done():
		logger.Debug("RPC call cancelled before handler dispatch: XID=0x%x client=%s error=%v",
			call.XID, clientAddr, ctx.Err())
		return ctx.Err()
	default:
	}

	switch call.Program {
	case rpc.ProgramNFS:
		replyData, err = c.handleNFSProcedure(ctx, call, procedureData, clientAddr)
	case rpc.ProgramMount:
		replyData, err = c.handleMountProcedure(ctx, call, procedureData, clientAddr)
	default:
		logger.Debug("Unknown program: %d", call.Program)
		// Send PROC_UNAVAIL error reply for unknown programs
		errorReply, err := rpc.MakeErrorReply(call.XID, rpc.RPCProcUnavail)
		if err != nil {
			return fmt.Errorf("make error reply: %w", err)
		}
		return c.sendReply(call.XID, errorReply)
	}

	if err != nil {
		// Check if error was due to context cancellation
		if err == context.Canceled || err == context.DeadlineExceeded {
			logger.Debug("Handler cancelled: program=%d procedure=%d xid=0x%x client=%s error=%v",
				call.Program, call.Procedure, call.XID, clientAddr, err)
			return err
		}

		// Handler returned an error - send RPC SYSTEM_ERR reply to client
		// Per RFC 5531, every RPC call should receive a reply, even on failure
		logger.Debug("Handler error: program=%d procedure=%d xid=0x%x error=%v",
			call.Program, call.Procedure, call.XID, err)

		errorReply, makeErr := rpc.MakeErrorReply(call.XID, rpc.RPCSystemErr)
		if makeErr != nil {
			// Failed to create error reply - return error to close connection
			return fmt.Errorf("make error reply: %w", makeErr)
		}

		// Send the error reply to client
		if sendErr := c.sendReply(call.XID, errorReply); sendErr != nil {
			return fmt.Errorf("send error reply: %w", sendErr)
		}

		// Return original error for logging/metrics but reply was sent
		return fmt.Errorf("handle program %d: %w", call.Program, err)
	}

	return c.sendReply(call.XID, replyData)
}

// extractShareName attempts to extract the share name from NFS request data.
//
// Most NFS procedures include a file handle at the beginning of the request.
// This function decodes the file handle using XDR and resolves it to a share
// name using the registry.
//
// Parameters:
//   - ctx: Context for cancellation
//   - data: Raw procedure data (XDR-encoded, file handle as first field)
//
// Returns:
//   - string: Share name, or empty string if no handle present (e.g., NULL procedure)
//   - error: Decoding or resolution error
func (c *NFSConnection) extractShareName(ctx context.Context, data []byte) (string, error) {
	// Decode file handle from XDR request data
	handle, err := xdr.DecodeFileHandleFromRequest(data)
	if err != nil {
		return "", fmt.Errorf("decode file handle: %w", err)
	}

	// No handle present (procedures like NULL, FSINFO don't have handles)
	if handle == nil {
		return "", nil
	}

	// Resolve share name from handle using registry
	shareName, err := c.server.registry.GetShareNameForHandle(ctx, handle)
	if err != nil {
		return "", fmt.Errorf("resolve share from handle: %w", err)
	}

	return shareName, nil
}

// logNFSRequest logs an NFS request with procedure, share, and auth information.
//
// This consolidates all the conditional logging logic in one place for cleaner code.
func (c *NFSConnection) logNFSRequest(procedure, share string, ctx *handlers.NFSHandlerContext) {
	var parts []string

	parts = append(parts, fmt.Sprintf("NFS %s", procedure))

	if share != "" {
		parts = append(parts, fmt.Sprintf("share=%s", share))
	}

	if ctx.UID != nil {
		parts = append(parts, fmt.Sprintf("uid=%d gid=%d ngids=%d",
			*ctx.UID, *ctx.GID, len(ctx.GIDs)))
	} else {
		parts = append(parts, fmt.Sprintf("auth_flavor=%d", ctx.AuthFlavor))
	}

	logger.Debug("%s", parts[0])
	for i := 1; i < len(parts); i++ {
		logger.Debug("  %s", parts[i])
	}
}

// handleNFSProcedure dispatches an NFS procedure call to the appropriate handler.
//
// It looks up the procedure in the dispatch table, extracts authentication
// context from the RPC call, and invokes the handler with the context.
//
// The context enables handlers to:
// - Respect cancellation during long operations (READ, WRITE, READDIR)
// - Implement request timeouts
// - Support graceful server shutdown
//
// Returns the reply data or an error if the handler fails.
func (c *NFSConnection) handleNFSProcedure(ctx context.Context, call *rpc.RPCCallMessage, data []byte, clientAddr string) ([]byte, error) {
	// Look up procedure in dispatch table
	procedure, ok := nfs.NfsDispatchTable[call.Procedure]
	if !ok {
		logger.Debug("Unknown NFS procedure: %d", call.Procedure)
		return []byte{}, nil
	}

	// Extract share name from file handle (best effort for metrics)
	share, extractErr := c.extractShareName(ctx, data)
	if extractErr != nil {
		logger.Warn("NFS %s: failed to extract share (handle may be invalid): %v", procedure.Name, extractErr)
		// Continue anyway - handler will validate and return proper NFS error
		share = ""
	}

	// Extract handler context (includes share and authentication for handlers)
	handlerCtx := nfs.ExtractHandlerContext(ctx, call, clientAddr, share, procedure.Name)

	// Log request with clean helper
	c.logNFSRequest(procedure.Name, share, handlerCtx)

	// Check context before dispatching to handler
	select {
	case <-ctx.Done():
		logger.Debug("NFS %s cancelled before handler: xid=0x%x", procedure.Name, call.XID)
		return nil, ctx.Err()
	default:
	}

	// ============================================================================
	// Metrics Instrumentation (Transparent to Handlers)
	// ============================================================================
	//
	// Three metrics are recorded for each request, following Prometheus best practices:
	//
	// 1. In-Flight Gauge (RecordRequestStart/End):
	//    - Tracks concurrent requests at any moment
	//    - Useful for capacity planning and detecting overload
	//    - Metric: dittofs_nfs_requests_in_flight
	//
	// 2. Request Counter (RecordRequest):
	//    - Counts total requests by procedure, share, status, and error code
	//    - Enables calculation of success/error rates and error type distribution
	//    - Metric: dittofs_nfs_requests_total
	//
	// 3. Duration Histogram (RecordRequest):
	//    - Tracks latency distribution for percentile calculations (p50, p95, p99)
	//    - Same call records both counter and histogram for efficiency
	//    - Metric: dittofs_nfs_request_duration_milliseconds
	//
	// This pattern ensures:
	//  - Handlers remain unaware of metrics (clean separation of concerns)
	//  - All procedures are instrumented consistently
	//  - Metrics include NFS protocol status codes, not Go errors
	//  - Share-level tracking enables per-tenant analysis
	//
	c.server.metrics.RecordRequestStart(procedure.Name, share)
	defer c.server.metrics.RecordRequestEnd(procedure.Name, share)

	// Execute handler and measure duration
	startTime := time.Now()
	result, err := procedure.Handler(
		handlerCtx,
		c.server.nfsHandler,
		c.server.registry,
		data,
	)
	duration := time.Since(startTime)

	// Record completion with NFS status code (e.g., "NFS3_OK", "NFS3ERR_NOENT")
	// This provides RFC-compliant error tracking for observability
	// Note: Pass empty string for NFS3_OK (success) to avoid labeling as error
	var responseStatus string
	if result != nil {
		if result.NFSStatus != nfs_types.NFS3OK {
			responseStatus = nfs.NFSStatusToString(result.NFSStatus)
		}

		// Record bytes transferred for READ/WRITE operations
		// Only successful operations populate these fields
		if result.NFSStatus == nfs_types.NFS3OK {
			if result.BytesRead > 0 {
				c.server.metrics.RecordBytesTransferred(procedure.Name, share, "read", result.BytesRead)
				c.server.metrics.RecordOperationSize("read", share, result.BytesRead)
			}
			if result.BytesWritten > 0 {
				c.server.metrics.RecordBytesTransferred(procedure.Name, share, "write", result.BytesWritten)
				c.server.metrics.RecordOperationSize("write", share, result.BytesWritten)
			}
		}
	} else if err != nil {
		responseStatus = "ERROR_NO_RESULT"
	}
	c.server.metrics.RecordRequest(procedure.Name, share, duration, responseStatus)

	if result == nil {
		return nil, err
	}
	return result.Data, err
}

// handleMountProcedure dispatches a MOUNT procedure call to the appropriate handler.
//
// It looks up the procedure in the dispatch table, extracts authentication
// context from the RPC call, and invokes the handler with the context.
//
// The context enables handlers to respect cancellation and timeouts.
//
// Returns the reply data or an error if the handler fails.
func (c *NFSConnection) handleMountProcedure(ctx context.Context, call *rpc.RPCCallMessage, data []byte, clientAddr string) ([]byte, error) {
	// Look up procedure in dispatch table
	procedure, ok := nfs.MountDispatchTable[call.Procedure]
	if !ok {
		logger.Debug("Unknown Mount procedure: %d", call.Procedure)
		return []byte{}, nil
	}

	// Mount requests don't have file handles, so no share
	share := ""

	// Extract handler context for mount requests
	handlerCtx := &mount_handlers.MountHandlerContext{
		Context:    ctx,
		ClientAddr: clientAddr,
		AuthFlavor: call.GetAuthFlavor(),
	}

	// Parse Unix credentials if AUTH_UNIX
	if handlerCtx.AuthFlavor == rpc.AuthUnix {
		authBody := call.GetAuthBody()
		if len(authBody) > 0 {
			if unixAuth, err := rpc.ParseUnixAuth(authBody); err == nil {
				handlerCtx.UID = &unixAuth.UID
				handlerCtx.GID = &unixAuth.GID
				handlerCtx.GIDs = unixAuth.GIDs
			}
		}
	}

	// Log request (MOUNT_ prefix) - convert to NFSHandlerContext for logging
	procedureName := "MOUNT_" + procedure.Name
	logCtx := &handlers.NFSHandlerContext{
		Context:    handlerCtx.Context,
		ClientAddr: handlerCtx.ClientAddr,
		Share:      share,
		AuthFlavor: handlerCtx.AuthFlavor,
		UID:        handlerCtx.UID,
		GID:        handlerCtx.GID,
		GIDs:       handlerCtx.GIDs,
	}
	c.logNFSRequest(procedureName, share, logCtx)

	// Check context before dispatching to handler
	select {
	case <-ctx.Done():
		logger.Debug("MOUNT %s cancelled before handler: xid=0x%x", procedure.Name, call.XID)
		return nil, ctx.Err()
	default:
	}

	// Record request start in metrics
	c.server.metrics.RecordRequestStart(procedureName, share)
	defer c.server.metrics.RecordRequestEnd(procedureName, share)

	// Dispatch to handler with context and record metrics
	startTime := time.Now()
	result, err := procedure.Handler(
		handlerCtx,
		c.server.mountHandler,
		c.server.registry,
		data,
	)
	duration := time.Since(startTime)

	// Record request completion in metrics with Mount status code
	// Note: Pass empty string for MountOK (success) to avoid labeling as error
	var responseStatus string
	if result != nil {
		if result.NFSStatus != mount_handlers.MountOK {
			responseStatus = nfs.MountStatusToString(result.NFSStatus)
		}
	} else if err != nil {
		responseStatus = "ERROR_NO_RESULT"
	}
	c.server.metrics.RecordRequest(procedureName, share, duration, responseStatus)

	if result == nil {
		return nil, err
	}
	return result.Data, err
}

// sendReply sends an RPC reply to the client.
//
// It applies write timeout if configured, constructs the RPC success reply,
// and writes it to the connection.
//
// Returns an error if:
// - Write timeout cannot be set
// - Reply construction fails
// - Network write fails
func (c *NFSConnection) sendReply(xid uint32, data []byte) error {
	if c.server.config.Timeouts.Write > 0 {
		deadline := time.Now().Add(c.server.config.Timeouts.Write)
		if err := c.conn.SetWriteDeadline(deadline); err != nil {
			return fmt.Errorf("set write deadline: %w", err)
		}
	}

	reply, err := rpc.MakeSuccessReply(xid, data)
	if err != nil {
		return fmt.Errorf("make reply: %w", err)
	}

	_, err = c.conn.Write(reply)
	if err != nil {
		return fmt.Errorf("write reply: %w", err)
	}

	logger.Debug("Sent reply for XID=0x%x (%d bytes)", xid, len(reply))
	return nil
}

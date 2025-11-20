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
	"github.com/marmos91/dittofs/internal/protocol/nfs/rpc"
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

		startTime := time.Now()
		err := c.handleRequest(ctx)
		duration := time.Since(startTime)

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

		// Record successful request (actual success/error determined in handler)
		// This records the request was processed, not necessarily successful
		_ = duration // Will be recorded in handleRPCCall

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
		return nil
	}

	if err != nil {
		// Check if error was due to context cancellation
		if err == context.Canceled || err == context.DeadlineExceeded {
			logger.Debug("Handler cancelled: program=%d procedure=%d xid=0x%x client=%s error=%v",
				call.Program, call.Procedure, call.XID, clientAddr, err)
			return err
		}

		logger.Debug("Handler error: %v", err)
		return fmt.Errorf("handle program %d: %w", call.Program, err)
	}

	return c.sendReply(call.XID, replyData)
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
	procInfo, ok := nfs.NfsDispatchTable[call.Procedure]
	if !ok {
		logger.Debug("Unknown NFS procedure: %d", call.Procedure)
		return []byte{}, nil
	}

	// Extract authentication context
	authCtx := nfs.ExtractAuthContext(ctx, call, clientAddr, procInfo.Name)

	// Log procedure with auth info
	if authCtx.UID != nil {
		logger.Debug("NFS %s: uid=%d gid=%d ngids=%d",
			procInfo.Name, *authCtx.UID, *authCtx.GID, len(authCtx.GIDs))
	} else {
		logger.Debug("NFS %s: auth_flavor=%d (no Unix credentials)",
			procInfo.Name, authCtx.AuthFlavor)
	}

	// Check context before dispatching to handler
	// This prevents starting work on cancelled requests
	select {
	case <-ctx.Done():
		logger.Debug("NFS %s cancelled before handler: xid=0x%x client=%s error=%v",
			procInfo.Name, call.XID, clientAddr, ctx.Err())
		return nil, ctx.Err()
	default:
	}

	// Record request start in metrics
	c.server.metrics.RecordRequestStart(procInfo.Name)
	defer c.server.metrics.RecordRequestEnd(procInfo.Name)

	// Dispatch to handler with context and record metrics
	startTime := time.Now()
	replyData, err := procInfo.Handler(
		authCtx,
		c.server.nfsHandler,
		c.server.registry,
		data,
	)
	duration := time.Since(startTime)

	// Record request completion in metrics
	c.server.metrics.RecordRequest(procInfo.Name, duration, err)

	return replyData, err
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
	procInfo, ok := nfs.MountDispatchTable[call.Procedure]
	if !ok {
		logger.Debug("Unknown Mount procedure: %d", call.Procedure)
		return []byte{}, nil
	}

	// Extract authentication context
	authCtx := nfs.ExtractAuthContext(ctx, call, clientAddr, procInfo.Name)

	// Log procedure with auth info
	if authCtx.UID != nil {
		logger.Debug("MOUNT %s: uid=%d gid=%d ngids=%d",
			procInfo.Name, *authCtx.UID, *authCtx.GID, len(authCtx.GIDs))
	} else {
		logger.Debug("MOUNT %s: auth_flavor=%d",
			procInfo.Name, authCtx.AuthFlavor)
	}

	// Check context before dispatching to handler
	select {
	case <-ctx.Done():
		logger.Debug("MOUNT %s cancelled before handler: xid=0x%x client=%s error=%v",
			procInfo.Name, call.XID, clientAddr, ctx.Err())
		return nil, ctx.Err()
	default:
	}

	// Record request start in metrics (use MOUNT_ prefix to distinguish from NFS)
	procedureName := "MOUNT_" + procInfo.Name
	c.server.metrics.RecordRequestStart(procedureName)
	defer c.server.metrics.RecordRequestEnd(procedureName)

	// Dispatch to handler with context and record metrics
	startTime := time.Now()
	replyData, err := procInfo.Handler(
		authCtx,
		c.server.mountHandler,
		c.server.registry,
		data,
	)
	duration := time.Since(startTime)

	// Record request completion in metrics
	c.server.metrics.RecordRequest(procedureName, duration, err)

	return replyData, err
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

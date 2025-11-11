package server

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/rpc"
)

type conn struct {
	server *NFSServer
	conn   net.Conn
}

type fragmentHeader struct {
	IsLast bool
	Length uint32
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
func (c *conn) serve(ctx context.Context) {
	defer func() {
		// Panic recovery - prevents a single connection from crashing the server
		if r := recover(); r != nil {
			c.server.metrics.RecordPanic()
			logger.Error("Panic in connection handler from %s: %v",
				c.conn.RemoteAddr().String(), r)
		}
		c.conn.Close()
	}()

	clientAddr := c.conn.RemoteAddr().String()
	logger.Debug("New connection from %s", clientAddr)

	// Set initial idle timeout
	if c.server.config.IdleTimeout > 0 {
		if err := c.conn.SetDeadline(time.Now().Add(c.server.config.IdleTimeout)); err != nil {
			logger.Warn("Failed to set deadline for %s: %v", clientAddr, err)
		}
	}

	for {
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
				c.server.metrics.RecordTimeout()
				logger.Debug("Connection from %s timed out: %v", clientAddr, err)
			} else {
				logger.Debug("Error handling request from %s: %v", clientAddr, err)
			}
			return
		}

		// Record successful request (actual success/error determined in handler)
		// This records the request was processed, not necessarily successful
		_ = duration // Will be recorded in handleRPCCall

		// Reset idle timeout after successful request
		if c.server.config.IdleTimeout > 0 {
			if err := c.conn.SetDeadline(time.Now().Add(c.server.config.IdleTimeout)); err != nil {
				logger.Warn("Failed to reset deadline for %s: %v", clientAddr, err)
			}
		}
	}
}

func (c *conn) handleRequest(ctx context.Context) error {
	// Apply read timeout if configured
	if c.server.config.ReadTimeout > 0 {
		deadline := time.Now().Add(c.server.config.ReadTimeout)
		if err := c.conn.SetReadDeadline(deadline); err != nil {
			return fmt.Errorf("set read deadline: %w", err)
		}
	}

	// Read fragment header
	header, err := c.readFragmentHeader()
	if err != nil {
		return err
	}

	// Validate fragment size to prevent memory exhaustion
	const maxFragmentSize = 1 << 20 // 1MB - NFS messages are typically much smaller
	if header.Length > maxFragmentSize {
		c.server.metrics.RecordFragmentError()
		logger.Warn("Fragment size %d exceeds maximum %d from %s",
			header.Length, maxFragmentSize, c.conn.RemoteAddr().String())
		return fmt.Errorf("fragment too large: %d bytes", header.Length)
	}

	// Read RPC message (now uses buffer pool)
	message, err := c.readRPCMessage(header.Length)
	if err != nil {
		return fmt.Errorf("read RPC message: %w", err)
	}
	defer PutBuffer(message) // Always return buffer to pool

	// Parse RPC call
	call, err := rpc.ReadCall(message)
	if err != nil {
		c.server.metrics.RecordParseError()
		logger.Debug("Error parsing RPC call: %v", err)
		return nil
	}

	logger.Debug("RPC Call: XID=0x%x Program=%d Version=%d Procedure=%d",
		call.XID, call.Program, call.Version, call.Procedure)

	// Extract procedure data
	procedureData, err := rpc.ReadData(message, call)
	if err != nil {
		c.server.metrics.RecordParseError()
		return fmt.Errorf("extract procedure data: %w", err)
	}

	// Handle the call
	return c.handleRPCCall(ctx, call, procedureData)
}

func (c *conn) readFragmentHeader() (*fragmentHeader, error) {
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

func (c *conn) readRPCMessage(length uint32) ([]byte, error) {
	// Get buffer from pool
	message := GetBuffer(length)

	// Read directly into pooled buffer
	_, err := io.ReadFull(c.conn, message)
	if err != nil {
		// Return buffer to pool on error
		PutBuffer(message)
		return nil, fmt.Errorf("read message: %w", err)
	}

	return message, nil
}

func (c *conn) handleRPCCall(ctx context.Context, call *rpc.RPCCallMessage, procedureData []byte) error {
	startTime := time.Now()
	var replyData []byte
	var err error
	var isNFS bool

	clientAddr := c.conn.RemoteAddr().String()

	logger.Debug("RPC Call Details: Program=%d Version=%d Procedure=%d",
		call.Program, call.Version, call.Procedure)

	switch call.Program {
	case rpc.ProgramNFS:
		isNFS = true
		replyData, err = c.handleNFSProcedure(ctx, call, procedureData, clientAddr)
	case rpc.ProgramMount:
		isNFS = false
		replyData, err = c.handleMountProcedure(ctx, call, procedureData, clientAddr)
	default:
		logger.Debug("Unknown program: %d", call.Program)
		return nil
	}

	// Record request metrics
	duration := time.Since(startTime)
	success := (err == nil)
	c.server.metrics.RecordRequest(duration, success, isNFS)

	if err != nil {
		logger.Debug("Handler error: %v", err)
		return fmt.Errorf("handle program %d: %w", call.Program, err)
	}

	return c.sendReply(call.XID, replyData)
}

func (c *conn) handleNFSProcedure(ctx context.Context, call *rpc.RPCCallMessage, data []byte, clientAddr string) ([]byte, error) {
	// Look up procedure in dispatch table
	procInfo, ok := nfsDispatchTable[call.Procedure]
	if !ok {
		logger.Debug("Unknown NFS procedure: %d", call.Procedure)
		return []byte{}, nil
	}

	// Extract authentication context
	authCtx := extractAuthContext(ctx, call, clientAddr, procInfo.Name)

	// Log procedure with auth info
	if authCtx.UID != nil {
		logger.Debug("NFS %s: uid=%d gid=%d ngids=%d",
			procInfo.Name, *authCtx.UID, *authCtx.GID, len(authCtx.GIDs))
	} else {
		logger.Debug("NFS %s: auth_flavor=%d (no Unix credentials)",
			procInfo.Name, authCtx.AuthFlavor)
	}

	// Dispatch to handler
	return procInfo.Handler(
		authCtx,
		c.server.nfsHandler,
		c.server.repository,
		c.server.content,
		data,
	)
}

func (c *conn) handleMountProcedure(ctx context.Context, call *rpc.RPCCallMessage, data []byte, clientAddr string) ([]byte, error) {
	// Look up procedure in dispatch table
	procInfo, ok := mountDispatchTable[call.Procedure]
	if !ok {
		logger.Debug("Unknown Mount procedure: %d", call.Procedure)
		return []byte{}, nil
	}

	// Extract authentication context
	authCtx := extractAuthContext(ctx, call, clientAddr, procInfo.Name)

	// Log procedure with auth info
	if authCtx.UID != nil {
		logger.Debug("MOUNT %s: uid=%d gid=%d ngids=%d",
			procInfo.Name, *authCtx.UID, *authCtx.GID, len(authCtx.GIDs))
	} else {
		logger.Debug("MOUNT %s: auth_flavor=%d",
			procInfo.Name, authCtx.AuthFlavor)
	}

	// Dispatch to handler
	return procInfo.Handler(
		authCtx,
		c.server.mountHandler,
		c.server.repository,
		data,
	)
}

func (c *conn) sendReply(xid uint32, data []byte) error {
	if c.server.config.WriteTimeout > 0 {
		deadline := time.Now().Add(c.server.config.WriteTimeout)
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

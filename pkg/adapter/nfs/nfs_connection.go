package nfs

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"runtime/debug"
	"sync"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/marmos91/dittofs/internal/bufpool"
	"github.com/marmos91/dittofs/internal/bytesize"
	"github.com/marmos91/dittofs/internal/logger"
	nfs "github.com/marmos91/dittofs/internal/protocol/nfs"
	mount_handlers "github.com/marmos91/dittofs/internal/protocol/nfs/mount/handlers"
	"github.com/marmos91/dittofs/internal/protocol/nfs/rpc"
	nfs_types "github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
	nlm "github.com/marmos91/dittofs/internal/protocol/nlm"
	nlm_handlers "github.com/marmos91/dittofs/internal/protocol/nlm/handlers"
	nlm_types "github.com/marmos91/dittofs/internal/protocol/nlm/types"
	nsm "github.com/marmos91/dittofs/internal/protocol/nsm"
	nsm_handlers "github.com/marmos91/dittofs/internal/protocol/nsm/handlers"
	nsm_types "github.com/marmos91/dittofs/internal/protocol/nsm/types"
	"github.com/marmos91/dittofs/internal/telemetry"
)

type NFSConnection struct {
	server *NFSAdapter
	conn   net.Conn

	// Concurrent request handling
	requestSem chan struct{}  // Semaphore to limit concurrent requests
	wg         sync.WaitGroup // Track active requests for graceful shutdown
	writeMu    sync.Mutex     // Protects connection writes (replies must be serialized)
}

type fragmentHeader struct {
	IsLast bool
	Length uint32
}

func NewNFSConnection(server *NFSAdapter, conn net.Conn) *NFSConnection {
	return &NFSConnection{
		server:     server,
		conn:       conn,
		requestSem: make(chan struct{}, server.config.MaxRequestsPerConnection),
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
		case <-c.server.shutdown:
			logger.Debug("Connection closed due to server shutdown", "address", clientAddr)
			return
		default:
		}

		// Read the request (blocks until data available)
		// This is done synchronously to maintain request order on the wire
		call, rawMessage, err := c.readRequest(ctx)
		if err != nil {
			if err == io.EOF {
				logger.Debug("Connection closed by client", "address", clientAddr)
			} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				logger.Debug("Connection timed out", "address", clientAddr, "error", err)
			} else if err == context.Canceled || err == context.DeadlineExceeded {
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
		// NOTE: rawMessage is a pooled buffer - must be returned via bufpool.Put()
		c.wg.Add(1)
		func(call *rpc.RPCCallMessage, rawMessage []byte) {
			defer c.handleRequestPanic(clientAddr, call.XID)
			defer bufpool.Put(rawMessage) // Return pooled buffer after processing

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
// the caller is responsible for returning it via bufpool.Put() after processing.
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

	// Read fragment header
	header, err := c.readFragmentHeader()
	if err != nil {
		// Don't log EOF as an error - it's a normal client disconnect
		if err != io.EOF {
			logger.Debug("Error reading fragment header", "address", c.conn.RemoteAddr().String(), "error", err)
		}
		return nil, nil, err
	}
	logger.Debug("Read fragment header", "address", c.conn.RemoteAddr().String(), "last", header.IsLast, "length", bytesize.ByteSize(header.Length))

	// Validate fragment size to prevent memory exhaustion
	const maxFragmentSize = 1 << 20 // 1MB - NFS messages are typically much smaller
	if header.Length > maxFragmentSize {
		logger.Warn("Fragment size exceeds maximum", "size", bytesize.ByteSize(header.Length), "max", bytesize.ByteSize(maxFragmentSize), "address", c.conn.RemoteAddr().String())
		return nil, nil, fmt.Errorf("fragment too large: %d bytes", header.Length)
	}

	// Check context before reading potentially large message
	select {
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	default:
	}

	// Read RPC message (uses buffer pool)
	// NOTE: Caller is responsible for returning buffer via bufpool.Put()
	message, err := c.readRPCMessage(header.Length)
	if err != nil {
		return nil, nil, fmt.Errorf("read RPC message: %w", err)
	}

	// Parse RPC call header
	call, err := rpc.ReadCall(message)
	if err != nil {
		bufpool.Put(message) // Return buffer on error
		logger.Debug("Error parsing RPC call", "error", err)
		return nil, nil, err
	}

	logger.Debug("RPC Call", "xid", fmt.Sprintf("0x%x", call.XID), "program", call.Program, "version", call.Version, "procedure", call.Procedure)

	// Return pooled buffer directly - no copy needed
	// Caller must return buffer to pool via bufpool.Put() after processing
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
	message := bufpool.GetUint32(length)

	// Read directly into pooled buffer
	_, err := io.ReadFull(c.conn, message)
	if err != nil {
		// Return buffer to pool on error
		bufpool.Put(message)
		return nil, fmt.Errorf("read message: %w", err)
	}

	return message, nil
}

// handleUnsupportedVersion handles version mismatch for NFS/Mount protocols.
//
// This method logs a warning about the unsupported version and returns an
// appropriate response. For NFSv4, it closes the connection to work around
// a macOS kernel bug. For other versions, it sends an RFC 5531-compliant
// PROG_MISMATCH reply.
//
// Parameters:
//   - call: The RPC call with the unsupported version
//   - supportedVersion: The version we support (e.g., 3 for NFSv3)
//   - programName: Name for logging ("NFS" or "Mount")
//   - clientAddr: Client address for logging
//
// Returns:
//   - error: Always returns an error (either connection closed or reply sent)
func (c *NFSConnection) handleUnsupportedVersion(call *rpc.RPCCallMessage, supportedVersion uint32, programName string, clientAddr string) error {
	logger.Warn("Unsupported "+programName+" version",
		"requested", call.Version,
		"supported", supportedVersion,
		"xid", fmt.Sprintf("0x%x", call.XID),
		"client", clientAddr)

	// WORKAROUND: macOS's NFS kernel module has a bug that causes a kernel
	// panic (null pointer dereference in com.apple.filesystems.nfs) when
	// receiving PROG_MISMATCH replies for NFSv4 requests. To avoid crashing
	// the client's machine, we close the TCP connection instead of sending
	// the RFC-compliant PROG_MISMATCH reply for NFSv4.
	// For other versions (e.g., NFSv2), we send the proper PROG_MISMATCH.
	if call.Version == rpc.NFSVersion4 {
		_ = c.conn.Close()
		return fmt.Errorf("unsupported %s version %d (only version %d supported) - closed connection to avoid macOS kernel bug",
			programName, call.Version, supportedVersion)
	}

	// Per RFC 5531, respond with PROG_MISMATCH for unsupported versions
	mismatchReply, err := rpc.MakeProgMismatchReply(call.XID, supportedVersion, supportedVersion)
	if err != nil {
		return fmt.Errorf("make version mismatch reply: %w", err)
	}
	return c.writeReply(call.XID, mismatchReply)
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

	logger.Debug("RPC Call Details", "program", call.Program, "version", call.Version, "procedure", call.Procedure)

	// Check context before dispatching to handler
	select {
	case <-ctx.Done():
		logger.Debug("RPC call cancelled before handler dispatch", "xid", fmt.Sprintf("0x%x", call.XID), "client", clientAddr, "error", ctx.Err())
		return ctx.Err()
	default:
	}

	switch call.Program {
	case rpc.ProgramNFS:
		// Validate NFS version (we only support NFSv3)
		if call.Version != rpc.NFSVersion3 {
			return c.handleUnsupportedVersion(call, rpc.NFSVersion3, "NFS", clientAddr)
		}
		replyData, err = c.handleNFSProcedure(ctx, call, procedureData, clientAddr)

	case rpc.ProgramMount:
		// Mount protocol version handling:
		// - MNT requires v3 (returns v3 file handle format)
		// - Other procedures (NULL, DUMP, UMNT, UMNTALL, EXPORT) are version-agnostic
		// macOS umount uses mount v1 for UMNT, so we accept v1/v2/v3 for those procedures
		if call.Procedure == mount_handlers.MountProcMnt && call.Version != rpc.MountVersion3 {
			return c.handleUnsupportedVersion(call, rpc.MountVersion3, "Mount", clientAddr)
		}
		replyData, err = c.handleMountProcedure(ctx, call, procedureData, clientAddr)

	case rpc.ProgramNLM:
		// NLM v4 only per CONTEXT.md decision
		if call.Version != rpc.NLMVersion4 {
			return c.handleUnsupportedVersion(call, rpc.NLMVersion4, "NLM", clientAddr)
		}
		replyData, err = c.handleNLMProcedure(ctx, call, procedureData, clientAddr)

	case rpc.ProgramNSM:
		// NSM v1 only
		if call.Version != rpc.NSMVersion1 {
			return c.handleUnsupportedVersion(call, rpc.NSMVersion1, "NSM", clientAddr)
		}
		replyData, err = c.handleNSMProcedure(ctx, call, procedureData, clientAddr)

	default:
		logger.Debug("Unknown program", "program", call.Program)
		// Send PROC_UNAVAIL error reply for unknown programs
		errorReply, err := rpc.MakeErrorReply(call.XID, rpc.RPCProcUnavail)
		if err != nil {
			return fmt.Errorf("make error reply: %w", err)
		}
		return c.writeReply(call.XID, errorReply)
	}

	if err != nil {
		// Check if error was due to context cancellation
		if err == context.Canceled || err == context.DeadlineExceeded {
			logger.Debug("Handler cancelled", "program", call.Program, "procedure", call.Procedure, "xid", fmt.Sprintf("0x%x", call.XID), "client", clientAddr, "error", err)
			return err
		}

		// Handler returned an error - send RPC SYSTEM_ERR reply to client
		// Per RFC 5531, every RPC call should receive a reply, even on failure
		logger.Debug("Handler error", "program", call.Program, "procedure", call.Procedure, "xid", fmt.Sprintf("0x%x", call.XID), "error", err)

		errorReply, makeErr := rpc.MakeErrorReply(call.XID, rpc.RPCSystemErr)
		if makeErr != nil {
			// Failed to create error reply - return error to close connection
			return fmt.Errorf("make error reply: %w", makeErr)
		}

		// Send the error reply to client
		if sendErr := c.writeReply(call.XID, errorReply); sendErr != nil {
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
		logger.Debug("Unknown NFS procedure", "procedure", call.Procedure)
		return []byte{}, nil
	}

	// Extract share name from file handle (best effort for metrics)
	share, extractErr := c.extractShareName(ctx, data)
	if extractErr != nil {
		logger.Warn("Failed to extract share from handle",
			"procedure", procedure.Name,
			"error", extractErr)
		// Continue anyway - handler will validate and return proper NFS error
		share = ""
	}

	// Start a span for this NFS operation
	// The span will be passed through the context to all downstream operations
	ctx, span := telemetry.StartNFSSpan(ctx, procedure.Name, nil,
		telemetry.ClientAddr(clientAddr),
		telemetry.RPCXID(call.XID),
		telemetry.NFSShare(share))
	defer span.End()

	// Inject trace context into logger context for log-trace correlation
	ctx = telemetry.InjectTraceContext(ctx)

	// Extract handler context (includes share and authentication for handlers)
	handlerCtx := nfs.ExtractHandlerContext(ctx, call, clientAddr, share, procedure.Name)

	// Log request with trace context
	logger.DebugCtx(ctx, "NFS request",
		"procedure", procedure.Name,
		"share", share,
		"client", clientAddr,
		"xid", fmt.Sprintf("0x%x", call.XID))

	// Check context before dispatching to handler
	select {
	case <-ctx.Done():
		telemetry.RecordError(ctx, ctx.Err())
		logger.DebugCtx(ctx, "NFS request cancelled before handler",
			"procedure", procedure.Name,
			"xid", fmt.Sprintf("0x%x", call.XID))
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
	if c.server.metrics != nil {
		c.server.metrics.RecordRequestStart(procedure.Name, share)
		defer c.server.metrics.RecordRequestEnd(procedure.Name, share)
	}

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
	if c.server.metrics != nil {
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
	}

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
		logger.Debug("Unknown Mount procedure", "procedure", call.Procedure)
		return []byte{}, nil
	}

	// Mount requests don't have file handles, so no share
	share := ""
	procedureName := "MOUNT_" + procedure.Name

	// Start a span for this Mount operation
	ctx, span := telemetry.StartSpan(ctx, "mount."+procedure.Name,
		trace.WithAttributes(
			telemetry.ClientAddr(clientAddr),
			telemetry.RPCXID(call.XID),
		))
	defer span.End()

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

	// Log request with trace context
	logger.DebugCtx(ctx, "Mount request",
		"procedure", procedureName,
		"client", clientAddr,
		"xid", fmt.Sprintf("0x%x", call.XID))

	// Check context before dispatching to handler
	select {
	case <-ctx.Done():
		telemetry.RecordError(ctx, ctx.Err())
		logger.DebugCtx(ctx, "Mount request cancelled before handler",
			"procedure", procedure.Name,
			"xid", fmt.Sprintf("0x%x", call.XID))
		return nil, ctx.Err()
	default:
	}

	// Record request start in metrics
	if c.server.metrics != nil {
		c.server.metrics.RecordRequestStart(procedureName, share)
		defer c.server.metrics.RecordRequestEnd(procedureName, share)
	}

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
	if c.server.metrics != nil {
		var responseStatus string
		if result != nil {
			if result.NFSStatus != mount_handlers.MountOK {
				responseStatus = nfs.MountStatusToString(result.NFSStatus)
			}
		} else if err != nil {
			responseStatus = "ERROR_NO_RESULT"
		}
		c.server.metrics.RecordRequest(procedureName, share, duration, responseStatus)
	}

	if result == nil {
		return nil, err
	}
	return result.Data, err
}

// handleNLMProcedure dispatches an NLM procedure call to the appropriate handler.
//
// It looks up the procedure in the NLM dispatch table, extracts authentication
// context from the RPC call, and invokes the handler with the context.
//
// NLM (Network Lock Manager) provides advisory file locking for NFS clients.
// It runs on the same port as NFS and MOUNT protocols.
//
// Returns the reply data or an error if the handler fails.
func (c *NFSConnection) handleNLMProcedure(ctx context.Context, call *rpc.RPCCallMessage, data []byte, clientAddr string) ([]byte, error) {
	// Look up procedure in NLM dispatch table
	procedure, ok := nlm.NLMDispatchTable[call.Procedure]
	if !ok {
		logger.Debug("Unknown NLM procedure", "procedure", call.Procedure)
		return []byte{}, nil
	}

	procedureName := fmt.Sprintf("NLM_%s", procedure.Name)

	// Start a span for this NLM operation
	ctx, span := telemetry.StartSpan(ctx, "nlm."+procedure.Name,
		trace.WithAttributes(
			telemetry.ClientAddr(clientAddr),
			telemetry.RPCXID(call.XID),
		))
	defer span.End()

	// Extract handler context for NLM requests
	handlerCtx := &nlm_handlers.NLMHandlerContext{
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

	// Log request with trace context
	logger.DebugCtx(ctx, "NLM request",
		"procedure", procedureName,
		"client", clientAddr,
		"xid", fmt.Sprintf("0x%x", call.XID))

	// Check context before dispatching to handler
	select {
	case <-ctx.Done():
		telemetry.RecordError(ctx, ctx.Err())
		logger.DebugCtx(ctx, "NLM request cancelled before handler",
			"procedure", procedure.Name,
			"xid", fmt.Sprintf("0x%x", call.XID))
		return nil, ctx.Err()
	default:
	}

	// Record request start in metrics
	if c.server.metrics != nil {
		c.server.metrics.RecordRequestStart(procedureName, "")
		defer c.server.metrics.RecordRequestEnd(procedureName, "")
	}

	// Dispatch to handler with context and record metrics
	startTime := time.Now()
	result, err := procedure.Handler(
		handlerCtx,
		c.server.nlmHandler,
		c.server.registry,
		data,
	)
	duration := time.Since(startTime)

	// Record request completion in metrics with NLM status code
	if c.server.metrics != nil {
		var responseStatus string
		if result != nil {
			if result.NLMStatus != nlm_types.NLM4Granted {
				responseStatus = nlm.NLMStatusToString(result.NLMStatus)
			}
		} else if err != nil {
			responseStatus = "ERROR_NO_RESULT"
		}
		c.server.metrics.RecordRequest(procedureName, "", duration, responseStatus)
	}

	if result == nil {
		return nil, err
	}
	return result.Data, err
}

// handleNSMProcedure dispatches an NSM procedure call to the appropriate handler.
//
// It looks up the procedure in the NSM dispatch table, extracts authentication
// context from the RPC call, and invokes the handler with the context.
//
// NSM (Network Status Monitor) provides crash recovery for NLM clients.
// It enables clients to register for notifications when the server restarts.
//
// Returns the reply data or an error if the handler fails.
func (c *NFSConnection) handleNSMProcedure(ctx context.Context, call *rpc.RPCCallMessage, data []byte, clientAddr string) ([]byte, error) {
	// Look up procedure in NSM dispatch table
	procedure, ok := nsm.NSMDispatchTable[call.Procedure]
	if !ok {
		logger.Debug("Unknown NSM procedure", "procedure", call.Procedure)
		return []byte{}, nil
	}

	procedureName := fmt.Sprintf("NSM_%s", procedure.Name)

	// Start a span for this NSM operation
	ctx, span := telemetry.StartSpan(ctx, "nsm."+procedure.Name,
		trace.WithAttributes(
			telemetry.ClientAddr(clientAddr),
			telemetry.RPCXID(call.XID),
		))
	defer span.End()

	// Extract handler context for NSM requests
	handlerCtx := &nsm_handlers.NSMHandlerContext{
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
				handlerCtx.ClientName = unixAuth.MachineName
			}
		}
	}

	// Log request with trace context
	logger.DebugCtx(ctx, "NSM request",
		"procedure", procedureName,
		"client", clientAddr,
		"xid", fmt.Sprintf("0x%x", call.XID))

	// Check context before dispatching to handler
	select {
	case <-ctx.Done():
		telemetry.RecordError(ctx, ctx.Err())
		logger.DebugCtx(ctx, "NSM request cancelled before handler",
			"procedure", procedure.Name,
			"xid", fmt.Sprintf("0x%x", call.XID))
		return nil, ctx.Err()
	default:
	}

	// Record request start in metrics
	if c.server.metrics != nil {
		c.server.metrics.RecordRequestStart(procedureName, "")
		defer c.server.metrics.RecordRequestEnd(procedureName, "")
	}

	// Dispatch to handler with context and record metrics
	startTime := time.Now()
	result, err := procedure.Handler(
		handlerCtx,
		c.server.nsmHandler,
		data,
	)
	duration := time.Since(startTime)

	// Record request completion in metrics with NSM status code
	if c.server.metrics != nil {
		var responseStatus string
		if result != nil {
			if result.NSMStatus != nsm_types.StatSucc {
				responseStatus = nsm.NSMStatusToString(result.NSMStatus)
			}
		} else if err != nil {
			responseStatus = "ERROR_NO_RESULT"
		}
		c.server.metrics.RecordRequest(procedureName, "", duration, responseStatus)
	}

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
// This method is thread-safe and can be called from multiple goroutines.
// Writes are serialized using writeMu to prevent concurrent writes from
// corrupting the TCP stream.
//
// Returns an error if:
// - Write timeout cannot be set
// - Reply construction fails
// - Network write fails
func (c *NFSConnection) sendReply(xid uint32, data []byte) error {
	reply, err := rpc.MakeSuccessReply(xid, data)
	if err != nil {
		return fmt.Errorf("make reply: %w", err)
	}

	return c.writeReply(xid, reply)
}

// writeReply writes a complete RPC reply to the connection.
//
// This is the core method for sending replies. It handles:
//   - Serializing writes with a mutex to prevent TCP stream corruption
//   - Setting write deadlines for timeout handling
//   - Logging the sent reply
//
// The reply parameter must be a complete RPC message including the fragment
// header. Use this for pre-formatted replies from MakeSuccessReply,
// MakeErrorReply, or MakeProgMismatchReply.
//
// Parameters:
//   - xid: Transaction ID for logging purposes
//   - reply: Complete RPC reply including fragment header
//
// Returns an error if:
//   - Write deadline cannot be set
//   - Network write fails
func (c *NFSConnection) writeReply(xid uint32, reply []byte) error {
	// Serialize all connection writes to prevent corruption
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if c.server.config.Timeouts.Write > 0 {
		deadline := time.Now().Add(c.server.config.Timeouts.Write)
		if err := c.conn.SetWriteDeadline(deadline); err != nil {
			return fmt.Errorf("set write deadline: %w", err)
		}
	}

	_, err := c.conn.Write(reply)
	if err != nil {
		return fmt.Errorf("write reply: %w", err)
	}

	logger.Debug("Sent reply", "xid", fmt.Sprintf("0x%x", xid), "bytes", bytesize.ByteSize(len(reply)))
	return nil
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

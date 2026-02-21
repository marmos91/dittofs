package nfs

import (
	"context"
	"encoding/binary"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/marmos91/dittofs/internal/logger"
	nfs "github.com/marmos91/dittofs/internal/protocol/nfs"
	mount_handlers "github.com/marmos91/dittofs/internal/protocol/nfs/mount/handlers"
	"github.com/marmos91/dittofs/internal/protocol/nfs/rpc"
	nfs_types "github.com/marmos91/dittofs/internal/protocol/nfs/types"
	v4handlers "github.com/marmos91/dittofs/internal/protocol/nfs/v4/handlers"
	v4state "github.com/marmos91/dittofs/internal/protocol/nfs/v4/state"
	v4types "github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
	nlm "github.com/marmos91/dittofs/internal/protocol/nlm"
	nlm_handlers "github.com/marmos91/dittofs/internal/protocol/nlm/handlers"
	nlm_types "github.com/marmos91/dittofs/internal/protocol/nlm/types"
	nsm "github.com/marmos91/dittofs/internal/protocol/nsm"
	nsm_handlers "github.com/marmos91/dittofs/internal/protocol/nsm/handlers"
	nsm_types "github.com/marmos91/dittofs/internal/protocol/nsm/types"
	"github.com/marmos91/dittofs/internal/telemetry"
)

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
	// Log first v3 call per server lifetime
	c.server.logV3FirstUse()

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

	// Check if this operation is blocked via adapter settings.
	if c.isOperationBlocked(procedure.Name) {
		logger.Debug("NFSv3 operation blocked by adapter settings",
			"procedure", procedure.Name,
			"client", clientAddr,
			"xid", fmt.Sprintf("0x%x", call.XID))

		// Return a minimal NFS3ERR_NOTSUPP response
		result := c.makeBlockedOpResponse()
		if c.server.metrics != nil {
			c.server.metrics.RecordRequest(procedure.Name, share, 0, nfs.NFSStatusToString(nfs_types.NFS3ErrNotSupp))
		}
		return result.Data, nil
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
		Context:         ctx,
		ClientAddr:      clientAddr,
		AuthFlavor:      call.GetAuthFlavor(),
		KerberosEnabled: c.server.gssProcessor != nil,
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

// handleNFSv4Procedure dispatches an NFSv4 procedure call to the appropriate handler.
//
// NFSv4 has only two RPC procedures (RFC 7530 Section 16):
//   - NFSPROC4_NULL (0): Ping/keepalive
//   - NFSPROC4_COMPOUND (1): Bundled operations
//
// All other procedure numbers are invalid and receive PROC_UNAVAIL.
func (c *NFSConnection) handleNFSv4Procedure(ctx context.Context, call *rpc.RPCCallMessage, data []byte, clientAddr string) ([]byte, error) {
	// Log first v4 call per server lifetime
	c.server.logV4FirstUse()

	// Start a span for this NFSv4 operation
	ctx, span := telemetry.StartSpan(ctx, "nfs4.procedure",
		trace.WithAttributes(
			telemetry.ClientAddr(clientAddr),
			telemetry.RPCXID(call.XID),
		))
	defer span.End()

	// Record metrics
	if c.server.metrics != nil {
		procedureName := "NFSv4_COMPOUND"
		if call.Procedure == v4types.NFSPROC4_NULL {
			procedureName = "NFSv4_NULL"
		}
		c.server.metrics.RecordRequestStart(procedureName, "")
		defer c.server.metrics.RecordRequestEnd(procedureName, "")
	}

	startTime := time.Now()

	switch call.Procedure {
	case v4types.NFSPROC4_NULL:
		result, err := c.server.v4Handler.HandleNull(data)
		if c.server.metrics != nil {
			duration := time.Since(startTime)
			c.server.metrics.RecordRequest("NFSv4_NULL", "", duration, "")
		}
		return result, err

	case v4types.NFSPROC4_COMPOUND:
		// Extract CompoundContext with auth credentials
		compCtx := v4handlers.ExtractV4HandlerContext(ctx, call, clientAddr)
		compCtx.ConnectionID = c.connectionID

		result, err := c.server.v4Handler.ProcessCompound(compCtx, data)

		// After COMPOUND completes, check if this connection was bound for
		// back-channel. If so, register a ConnWriter and PendingCBReplies
		// so the read loop can demux backchannel replies.
		c.maybeRegisterBackchannel(ctx)

		if c.server.metrics != nil {
			duration := time.Since(startTime)
			var responseStatus string
			if err != nil {
				responseStatus = "ERROR"
			}
			c.server.metrics.RecordRequest("NFSv4_COMPOUND", "", duration, responseStatus)
		}
		return result, err

	default:
		// NFSv4 only has 2 procedures -- anything else is invalid
		logger.Debug("Unknown NFSv4 procedure",
			"procedure", call.Procedure,
			"client", clientAddr)
		errorReply, err := rpc.MakeErrorReply(call.XID, rpc.RPCProcUnavail)
		if err != nil {
			return nil, fmt.Errorf("make NFSv4 proc unavail reply: %w", err)
		}
		return nil, c.writeReply(call.XID, errorReply)
	}
}

// isOperationBlocked checks if the given operation is blocked via adapter
// settings. Reads from the runtime's settings watcher for hot-reload support.
// Used for NFSv3 dispatch; NFSv4 has its own blocked ops mechanism via Handler.SetBlockedOps.
func (c *NFSConnection) isOperationBlocked(opName string) bool {
	if c.server.registry == nil {
		return false
	}

	settings := c.server.registry.GetNFSSettings()
	if settings == nil {
		return false
	}

	blockedOps := settings.GetBlockedOperations()
	for _, blocked := range blockedOps {
		if blocked == opName {
			return true
		}
	}
	return false
}

// maybeRegisterBackchannel checks if this connection has been bound for
// back-channel after a COMPOUND completes. If it has, registers a ConnWriter
// callback (capturing the NFSConnection's writeMu for serialization) and a
// PendingCBReplies instance for demuxing backchannel replies.
//
// This is called after every NFSv4 COMPOUND to detect BIND_CONN_TO_SESSION
// or CREATE_SESSION auto-bind results that include back-channel direction.
// The check is cheap (one map lookup) and idempotent (no-op if already registered).
func (c *NFSConnection) maybeRegisterBackchannel(ctx context.Context) {
	if c.server.v4Handler == nil || c.server.v4Handler.StateManager == nil {
		return
	}

	sm := c.server.v4Handler.StateManager

	// Check if this connection is bound with a back-channel direction
	binding := sm.GetConnectionBinding(c.connectionID)
	if binding == nil {
		return
	}
	if binding.Direction != v4state.ConnDirBack && binding.Direction != v4state.ConnDirBoth {
		return
	}

	// Already registered -- no-op
	if c.pendingCBReplies != nil {
		return
	}

	// Register ConnWriter: captures this NFSConnection's writeMu to prevent
	// interleaving between fore-channel replies and backchannel callbacks.
	writer := v4state.ConnWriter(func(data []byte) error {
		c.writeMu.Lock()
		defer c.writeMu.Unlock()
		_, err := c.conn.Write(data)
		return err
	})
	pending := sm.RegisterConnWriter(c.connectionID, writer)
	c.pendingCBReplies = pending

	// Start the BackchannelSender for this session (idempotent)
	sm.StartBackchannelSender(ctx, binding.SessionID)

	logger.Debug("Backchannel registered for connection",
		"conn_id", c.connectionID,
		"session_id", binding.SessionID.String(),
		"direction", binding.Direction.String())
}

// makeBlockedOpResponse creates an NFS3ERR_NOTSUPP response for a blocked operation.
// The response contains the status code followed by empty WCC data (pre_op=false,
// post_op=false), which clients handle gracefully per RFC 1813.
func (c *NFSConnection) makeBlockedOpResponse() *nfs.HandlerResult {
	response := make([]byte, 12)

	// Write status code as big-endian uint32
	status := uint32(nfs_types.NFS3ErrNotSupp)
	binary.BigEndian.PutUint32(response[0:4], status)
	// bytes 4-7: pre_op_attr present flag = 0 (false)
	// bytes 8-11: post_op_attr present flag = 0 (false)
	// (already zero-initialized)

	return &nfs.HandlerResult{
		Data:      response,
		NFSStatus: nfs_types.NFS3ErrNotSupp,
	}
}

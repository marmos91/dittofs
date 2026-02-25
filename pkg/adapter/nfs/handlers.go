package nfs

import (
	"context"
	"encoding/binary"
	"fmt"
	"slices"

	nfs "github.com/marmos91/dittofs/internal/adapter/nfs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/middleware"
	nlm "github.com/marmos91/dittofs/internal/adapter/nfs/nlm"
	nlm_handlers "github.com/marmos91/dittofs/internal/adapter/nfs/nlm/handlers"
	nsm "github.com/marmos91/dittofs/internal/adapter/nfs/nsm"
	nsm_handlers "github.com/marmos91/dittofs/internal/adapter/nfs/nsm/handlers"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	nfs_types "github.com/marmos91/dittofs/internal/adapter/nfs/types"
	v4handlers "github.com/marmos91/dittofs/internal/adapter/nfs/v4/handlers"
	v4state "github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	v4types "github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/logger"
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
		return result.Data, nil
	}

	// Dispatch to handler
	result, err := procedure.Handler(
		handlerCtx,
		c.server.nfsHandler,
		c.server.registry,
		data,
	)

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

	// Extract handler context using shared middleware
	handlerCtx := middleware.ExtractMountHandlerContext(ctx, call, clientAddr, c.server.gssProcessor != nil)

	// Log request with trace context
	logger.DebugCtx(ctx, "Mount request",
		"procedure", "MOUNT_"+procedure.Name,
		"client", clientAddr,
		"xid", fmt.Sprintf("0x%x", call.XID))

	// Check context before dispatching to handler
	select {
	case <-ctx.Done():
		logger.DebugCtx(ctx, "Mount request cancelled before handler",
			"procedure", procedure.Name,
			"xid", fmt.Sprintf("0x%x", call.XID))
		return nil, ctx.Err()
	default:
	}

	// Dispatch to handler
	result, err := procedure.Handler(
		handlerCtx,
		c.server.mountHandler,
		c.server.registry,
		data,
	)

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
		"procedure", procedure.Name,
		"client", clientAddr,
		"xid", fmt.Sprintf("0x%x", call.XID))

	// Check context before dispatching to handler
	select {
	case <-ctx.Done():
		logger.DebugCtx(ctx, "NLM request cancelled before handler",
			"procedure", procedure.Name,
			"xid", fmt.Sprintf("0x%x", call.XID))
		return nil, ctx.Err()
	default:
	}

	// Dispatch to handler
	result, err := procedure.Handler(
		handlerCtx,
		c.server.nlmHandler,
		c.server.registry,
		data,
	)

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
		"procedure", procedure.Name,
		"client", clientAddr,
		"xid", fmt.Sprintf("0x%x", call.XID))

	// Check context before dispatching to handler
	select {
	case <-ctx.Done():
		logger.DebugCtx(ctx, "NSM request cancelled before handler",
			"procedure", procedure.Name,
			"xid", fmt.Sprintf("0x%x", call.XID))
		return nil, ctx.Err()
	default:
	}

	// Dispatch to handler
	result, err := procedure.Handler(
		handlerCtx,
		c.server.nsmHandler,
		data,
	)

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

	switch call.Procedure {
	case v4types.NFSPROC4_NULL:
		return c.server.v4Handler.HandleNull(data)

	case v4types.NFSPROC4_COMPOUND:
		// Extract CompoundContext with auth credentials
		compCtx := v4handlers.ExtractV4HandlerContext(ctx, call, clientAddr)
		compCtx.ConnectionID = c.connectionID

		result, err := c.server.v4Handler.ProcessCompound(compCtx, data)

		// After COMPOUND completes, check if this connection was bound for
		// back-channel. If so, register a ConnWriter and PendingCBReplies
		// so the read loop can demux backchannel replies.
		c.maybeRegisterBackchannel(ctx)

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

	return slices.Contains(settings.GetBlockedOperations(), opName)
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

	// Already registered -- verify StateManager still has pending replies.
	// If the connection was unbound/rebound, StateManager may have cleared
	// the ConnWriter and PendingCBReplies, so we need to re-register.
	if c.pendingCBReplies != nil {
		if smPending := sm.GetPendingCBReplies(c.connectionID); smPending != nil {
			return
		}
		// Local state is stale: StateManager no longer tracks this connection.
		// Clear the local flag so we can re-register the backchannel below.
		c.pendingCBReplies = nil
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
	binary.BigEndian.PutUint32(response[0:4], uint32(nfs_types.NFS3ErrNotSupp))
	// bytes 4-7: pre_op_attr present flag = 0 (false)
	// bytes 8-11: post_op_attr present flag = 0 (false)
	// (already zero-initialized)

	return &nfs.HandlerResult{
		Data:      response,
		NFSStatus: nfs_types.NFS3ErrNotSupp,
	}
}

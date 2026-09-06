package nfs

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"time"

	nfs "github.com/marmos91/dittofs/internal/adapter/nfs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/middleware"
	nlm "github.com/marmos91/dittofs/internal/adapter/nfs/nlm"
	nlm_callback "github.com/marmos91/dittofs/internal/adapter/nfs/nlm/callback"
	nlm_handlers "github.com/marmos91/dittofs/internal/adapter/nfs/nlm/handlers"
	nsm "github.com/marmos91/dittofs/internal/adapter/nfs/nsm"
	nsm_handlers "github.com/marmos91/dittofs/internal/adapter/nfs/nsm/handlers"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	nfs_types "github.com/marmos91/dittofs/internal/adapter/nfs/types"
	v4handlers "github.com/marmos91/dittofs/internal/adapter/nfs/v4/handlers"
	v4state "github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	v4types "github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	xdr "github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
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
// recordOp records one NFS operation for the RED metrics (rate, errors,
// duration). status is intentionally a bounded ok|error rather than the precise
// NFS status code: labelling by raw status would multiply series by op ×
// ~20 codes. The ok bit is derived accurately from the handler's NFSStatus (v3)
// so it reflects protocol-level failures, not just transport errors.
func (c *NFSConnection) recordOp(op string, start time.Time, ok bool) {
	status := "ok"
	if !ok {
		status = "error"
	}
	c.server.Registry.Metrics().RecordRequest("nfs", op, status, time.Since(start))
}

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

	// Duplicate-request cache (DRC) for non-idempotent procedures.
	//
	// On an RPC-timeout retransmit a client re-sends the same request; for
	// non-idempotent ops (REMOVE/RMDIR/RENAME/CREATE/MKDIR/LINK/SYMLINK/MKNOD/
	// guarded SETATTR) re-executing yields a spurious EEXIST/ENOENT/NOT_SYNC.
	// We replay the recorded reply instead. Idempotent ops bypass the cache.
	useDRC := c.server.drc != nil && isCacheable(call.Procedure)
	if useDRC {
		switch res, reply := c.server.drc.lookup(clientAddr, call.XID, data); res {
		case drcReplay:
			logger.DebugCtx(ctx, "NFS duplicate request replayed from DRC",
				"procedure", procedure.Name,
				"client", clientAddr,
				"xid", fmt.Sprintf("0x%x", call.XID))
			return reply, nil
		case drcInProgressDup:
			// Original is still executing; drop this duplicate and let the
			// in-flight request produce the single authoritative reply. Signal
			// "write nothing" via errDropReply so the dispatcher does not emit a
			// truncated success reply (or a second reply for this XID).
			logger.DebugCtx(ctx, "NFS duplicate of in-flight request dropped",
				"procedure", procedure.Name,
				"client", clientAddr,
				"xid", fmt.Sprintf("0x%x", call.XID))
			return nil, errDropReply
		default:
			// drcMiss: an in-progress slot is now reserved; fall through to run
			// the handler and record the reply below.
		}
	}

	// Dispatch to handler
	start := time.Now()
	result, err := procedure.Handler(
		handlerCtx,
		c.server.nfsHandler,
		c.server.Registry,
		data,
	)
	c.recordOp(procedure.Name, start, err == nil && (result == nil || result.NFSStatus == 0))

	if result == nil {
		if useDRC {
			// No reply to cache (e.g. decode failure); release the slot so a
			// later legitimate retry is not swallowed.
			c.server.drc.abort(clientAddr, call.XID, data)
		}
		return nil, err
	}
	if useDRC {
		c.server.drc.record(clientAddr, call.XID, data, result.Data)
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
	start := time.Now()
	result, err := procedure.Handler(
		handlerCtx,
		c.server.mountHandler,
		c.server.Registry,
		data,
	)
	c.recordOp("MOUNT_"+procedure.Name, start, err == nil)

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
		Version:    call.Version,
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
	start := time.Now()
	result, err := procedure.Handler(
		handlerCtx,
		c.server.nlmHandler,
		c.server.Registry,
		data,
	)
	c.recordOp("NLM_"+procedure.Name, start, err == nil)

	if result == nil {
		return nil, err
	}

	// Asynchronous (_MSG) procedures reply void inline and deliver the result as
	// an NLM *_RES callback to the client (the macOS/BSD lockd path). The *_RES
	// MUST leave the NLM listening socket: lockd talks to nlockmgr over a
	// connected UDP socket and the kernel drops any datagram whose source is not
	// that exact peer, so the callback is sent via the live listener, not a new
	// socket. Async NLM is UDP-only.
	if result.AsyncRes != nil {
		c.sendNLMAsyncResult(ctx, call.Version, clientAddr, procedure.Name, result.AsyncRes)
		// The inline reply to a _MSG call is ALWAYS SUCCESS + void; any decode
		// error is already encoded in the *_RES body delivered above. Returning
		// the error here would make the transport send RPCSystemErr, which lockd
		// would misread as a transport failure rather than a protocol result.
		return []byte{}, nil
	}

	return result.Data, err
}

// sendNLMAsyncResult delivers an NLM *_RES callback for an async (_MSG) request
// from the NLM listening socket (so the source address matches the peer lockd
// is connected to).
func (c *NFSConnection) sendNLMAsyncResult(ctx context.Context, vers uint32, clientAddr, procName string, async *nlm.AsyncResult) {
	udpConn := c.server.udpConn
	if udpConn == nil {
		logger.WarnCtx(ctx, "NLM async result: no UDP listener; cannot deliver *_RES",
			"procedure", procName, "client", clientAddr)
		return
	}
	dst, err := net.ResolveUDPAddr("udp", clientAddr)
	if err != nil {
		logger.WarnCtx(ctx, "NLM async result: bad client address",
			"procedure", procName, "client", clientAddr, "error", err)
		return
	}
	msg, err := nlm_callback.BuildNLMResultMessage(rpc.ProgramNLM, vers, async.Proc, async.Body)
	if err != nil {
		logger.WarnCtx(ctx, "NLM async result: build failed",
			"procedure", procName, "client", clientAddr, "error", err)
		return
	}
	if _, err := udpConn.WriteToUDP(msg, dst); err != nil {
		logger.WarnCtx(ctx, "NLM async result: send failed",
			"procedure", procName, "client", clientAddr, "res_proc", async.Proc, "error", err)
		return
	}
	logger.DebugCtx(ctx, "NLM *_RES sent",
		"procedure", procName, "client", clientAddr, "res_proc", async.Proc,
		"src", udpConn.LocalAddr().String())
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
	start := time.Now()
	result, err := procedure.Handler(
		handlerCtx,
		c.server.nsmHandler,
		data,
	)
	c.recordOp("NSM_"+procedure.Name, start, err == nil)

	if result == nil {
		return nil, err
	}
	return result.Data, err
}

// maxDupReqBytes bounds both the request a v4.0 COMPOUND may present and the
// reply the duplicate request cache will keep for it.
//
// The cache exists for namespace mutations, and a CREATE, REMOVE, RENAME or
// LINK compound is a filehandle and a name or two -- hundreds of bytes, with a
// reply smaller still. Bulk-data compounds are far above this, and paying for
// them is pure cost: keying the cache checksums the whole request body, so a
// 1 MiB WRITE would buy nothing and pay for a checksum of every byte, twice,
// once to reserve the slot and once to release it. Capping the reply likewise
// keeps the cache bounded in bytes and not only in entries.
//
// ponytail: one bound for both directions, chosen to sit far above every
// namespace-mutating compound rather than tuned. A compound above it is simply
// left to re-execute on a retransmission, which is what the server did before
// the cache covered v4.0 at all. Split it in two, or raise it, only if a real
// workload is found whose mutations do not fit.
const maxDupReqBytes = 8 << 10

// drcEligibleV40Compound reports whether a COMPOUND should be tracked in the
// duplicate request cache: it must be v4.0, which has no session slot table of
// its own, and small enough that tracking it is worth what keying it costs.
// The size test comes first because it is the cheap one.
func drcEligibleV40Compound(data []byte) bool {
	return len(data) <= maxDupReqBytes && isV40Compound(data)
}

// drcRecordableReply reports whether a finished COMPOUND's reply should be kept
// for a retransmission to be answered from. The dispatcher decides whether the
// COMPOUND ran anything that must not run twice; this adds the size bound, which
// is what keeps a mutation bundled with a large read from pinning its whole
// reply in the cache.
func drcRecordableReply(cacheReply bool, replyLen int) bool {
	return cacheReply && replyLen <= maxDupReqBytes
}

// isV40Compound reports whether a COMPOUND request body declares minorversion 0.
//
// COMPOUND4args opens with the tag the server echoes back, a utf8str_cs whose
// wire encoding is that of a variable-length opaque, followed by the
// minorversion -- so the dialect is two fields into the body and is readable
// without decoding any operation. A body too malformed to yield them is left to
// ProcessCompound to reject.
func isV40Compound(data []byte) bool {
	reader := bytes.NewReader(data)
	if _, err := xdr.DecodeOpaque(reader); err != nil {
		return false
	}
	minorVersion, err := xdr.DecodeUint32(reader)
	return err == nil && minorVersion == v4types.NFS4_MINOR_VERSION_0
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
		start := time.Now()
		reply, err := c.server.v4Handler.HandleNull(data)
		c.recordOp("NULL", start, err == nil)
		return reply, err

	case v4types.NFSPROC4_COMPOUND:
		// Extract CompoundContext with auth credentials
		compCtx, authStatus := v4handlers.ExtractV4HandlerContext(ctx, call, clientAddr)
		if authStatus != v4types.NFS4_OK {
			// GSS auth flavor claimed but no verified identity — abort before
			// decoding any ops. We cannot echo the COMPOUND tag here because the
			// request body has not been decoded; RFC 7530 §15.1 permits replying
			// with NFS4ERR_WRONGSEC, an empty tag, and zero results.
			logger.Warn("NFSv4 COMPOUND rejected: unauthenticated GSS flavor",
				"status", authStatus, "client", clientAddr)
			reply, encErr := v4handlers.EncodeAbortCompound(authStatus)
			if encErr != nil {
				return nil, fmt.Errorf("encode GSS auth error reply: %w", encErr)
			}
			return reply, nil
		}
		compCtx.ConnectionID = c.connectionID

		// Duplicate-request cache for NFSv4.0, which has no SEQUENCE and so no
		// per-request exactly-once machinery of its own. A retransmitted CREATE,
		// REMOVE, RENAME or LINK carries no seqid to recognise it by, and
		// re-executing it turns a success the client never saw into
		// NFS4ERR_EXIST or NFS4ERR_NOENT. Retransmits of v4.1 and v4.2 compounds
		// are caught by the session slot table instead, so only minorversion 0
		// consults the cache.
		useDRC := c.server.drc != nil && drcEligibleV40Compound(data)
		if useDRC {
			switch res, reply := c.server.drc.lookup(clientAddr, call.XID, data); res {
			case drcReplay:
				logger.DebugCtx(ctx, "NFSv4.0 COMPOUND replayed from DRC",
					"client", clientAddr,
					"xid", fmt.Sprintf("0x%x", call.XID))
				return reply, nil
			case drcInProgressDup:
				// The original is still running and owns the XID; write nothing
				// rather than a second reply on the same XID.
				logger.DebugCtx(ctx, "NFSv4.0 duplicate of in-flight COMPOUND dropped",
					"client", clientAddr,
					"xid", fmt.Sprintf("0x%x", call.XID))
				return nil, errDropReply
			default:
				// drcMiss: an in-progress slot is now reserved. Release it
				// however this returns -- lookup matches an in-progress entry
				// before it considers age, so one left behind answers every
				// later retransmission of this exact request with a silent
				// drop, for as long as the connection lives. A panic in
				// ProcessCompound is recovered per request and leaves the
				// connection open, so only a deferred release covers it.
				// abort removes the entry only while it is still in-progress,
				// which makes this a no-op once the reply below is recorded.
				defer c.server.drc.abort(clientAddr, call.XID, data)
			}
		}

		// COMPOUND status here is coarse: per-op NFS4ERR codes are encoded inside
		// the XDR result and the RPC always succeeds, so err only reflects
		// wire/decode failures. Per-op v4 RED needs ProcessCompound to surface a
		// status (follow-up); this records traffic + latency + transport errors.
		start := time.Now()
		result, err := c.server.v4Handler.ProcessCompound(compCtx, data)
		c.recordOp("COMPOUND", start, err == nil)

		// Record only what a retransmission must not re-run. Anything else
		// leaves the slot to the deferred release above, so a later legitimate
		// retry is not swallowed by it.
		if useDRC && err == nil && drcRecordableReply(compCtx.CacheReply, len(result)) {
			c.server.drc.record(clientAddr, call.XID, data, result)
		}

		// The COMPOUND carries the minorversion, so the registry's "4" can now
		// be refined to the exact dialect. A refused minorversion is not
		// reported: it is answered with NFS4ERR_MINOR_VERS_MISMATCH and a nil
		// error, and the server never served that dialect.
		if err == nil && compCtx.MinorVersionAccepted {
			c.noteNFSVersion("4." + strconv.FormatUint(uint64(compCtx.MinorVersion), 10))
		}

		// After COMPOUND completes, check if this connection was bound for
		// back-channel. If so, register a ConnWriter and PendingCBReplies
		// so the read loop can demux backchannel replies.
		c.maybeRegisterBackchannel(ctx)

		return result, err

	default:
		// NFSv4 only has 2 procedures -- anything else is invalid. Write the
		// single authoritative PROC_UNAVAIL reply here, then signal the
		// dispatcher to emit nothing further via errReplyAlreadySent. Returning
		// (nil, nil) instead would fall through to handleRPCCall's sendReply,
		// writing a SECOND (empty MSG_ACCEPTED) reply on the same XID and
		// corrupting the TCP stream for all subsequent requests.
		logger.Debug("Unknown NFSv4 procedure",
			"procedure", call.Procedure,
			"client", clientAddr)
		errorReply, err := rpc.MakeErrorReply(call.XID, rpc.RPCProcUnavail)
		if err != nil {
			return nil, fmt.Errorf("make NFSv4 proc unavail reply: %w", err)
		}
		if writeErr := c.writeReply(call.XID, errorReply); writeErr != nil {
			return nil, writeErr
		}
		return nil, errReplyAlreadySent
	}
}

// setV3BlockedOps replaces the cached set of NFSv3 procedure names blocked at
// the adapter level. Called from applyNFSSettings on startup and on each
// settings-change event, so the hot RPC dispatch path (isOperationBlocked)
// consults a pre-parsed name-keyed set instead of unmarshalling the stored
// blocklist on every request. A nil/empty slice clears the set.
func (s *NFSAdapter) setV3BlockedOps(opNames []string) {
	var blocked map[string]bool
	if len(opNames) > 0 {
		blocked = make(map[string]bool, len(opNames))
		for _, name := range opNames {
			blocked[name] = true
		}
	}
	s.blockedOpsMu.Lock()
	s.v3BlockedOps = blocked
	s.blockedOpsMu.Unlock()
}

// isOperationBlocked checks if the given NFSv3 procedure is blocked via adapter
// settings. It consults the pre-parsed v3BlockedOps set (populated from the
// SettingsWatcher in applyNFSSettings, with hot-reload support) so the hot
// dispatch path does a single map lookup rather than a per-RPC JSON unmarshal.
// NFSv4 has its own blocked ops mechanism via Handler.SetBlockedOps.
func (c *NFSConnection) isOperationBlocked(opName string) bool {
	c.server.blockedOpsMu.RLock()
	blocked := c.server.v3BlockedOps[opName]
	c.server.blockedOpsMu.RUnlock()
	return blocked
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

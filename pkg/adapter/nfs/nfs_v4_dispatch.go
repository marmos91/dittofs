package nfs

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	v4handlers "github.com/marmos91/dittofs/internal/adapter/nfs/v4/handlers"
	v4state "github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	v4types "github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/logger"
)

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
		return c.withDRC(ctx, call, data, clientAddr,
			drcEligibleV40Compound(data), "NFSv4.0 COMPOUND",
			func() ([]byte, bool, error) {
				// COMPOUND status here is coarse: per-op NFS4ERR codes are encoded
				// inside the XDR result and the RPC always succeeds, so err only
				// reflects wire/decode failures. Per-op v4 RED needs ProcessCompound
				// to surface a status (follow-up); this records traffic + latency +
				// transport errors.
				start := time.Now()
				result, err := c.server.v4Handler.ProcessCompound(compCtx, data)
				c.recordOp("COMPOUND", start, err == nil)

				// The COMPOUND carries the minorversion, so the registry's "4" can
				// now be refined to the exact dialect. A refused minorversion is not
				// reported: it is answered with NFS4ERR_MINOR_VERS_MISMATCH and a nil
				// error, and the server never served that dialect.
				if err == nil && compCtx.MinorVersionAccepted {
					c.noteNFSVersion("4." + strconv.FormatUint(uint64(compCtx.MinorVersion), 10))
				}

				// After COMPOUND completes, check if this connection was bound for
				// back-channel. If so, register a ConnWriter and PendingCBReplies
				// so the read loop can demux backchannel replies.
				c.maybeRegisterBackchannel(ctx)

				// Record only what a retransmission must not re-run. Anything else
				// leaves the reservation to the deferred release, so a later
				// legitimate retry is not swallowed by it.
				return result, err == nil && drcRecordableReply(compCtx.CacheReply, len(result)), err
			})

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

// maybeRegisterBackchannel checks if this connection has been bound for
// back-channel after a COMPOUND completes. If it has, registers a ConnWriter
// callback (capturing the NFSConnection's writeMu for serialization) and a
// PendingCBReplies instance for demuxing backchannel replies.
//
// This is called after every NFSv4 COMPOUND to detect BIND_CONN_TO_SESSION
// or CREATE_SESSION auto-bind results that include back-channel direction.
// The check is cheap (one map lookup) and idempotent (no-op if already registered).
//
// The ConnWriter and its reply demultiplexer are per connection while the
// sender is per session, and one connection may carry several sessions, so the
// writer is registered once and every back-bound session on the connection
// gets its own sender started.
//
// The demultiplexer this installs is not copied onto the connection: the read
// loop reads it back out of the StateManager. Two concurrent COMPOUNDs on one
// connection can both reach the registration below, and a DESTROY_SESSION
// between one of them registering and publishing its copy would leave the read
// loop demuxing into a table no sender registers with any more. One owner, so
// there is nothing to reconcile.
func (c *NFSConnection) maybeRegisterBackchannel(ctx context.Context) {
	if c.server.v4Handler == nil || c.server.v4Handler.StateManager == nil {
		return
	}

	sm := c.server.v4Handler.StateManager

	// Collect the sessions this connection carries a back channel for.
	var backBound []*v4state.BoundConnection
	for _, b := range sm.GetConnectionBindingsForConn(c.connectionID) {
		if b.Direction == v4state.ConnDirBack || b.Direction == v4state.ConnDirBoth {
			backBound = append(backBound, b)
		}
	}
	if len(backBound) == 0 {
		return
	}

	// Register the ConnWriter once per connection. When StateManager no longer
	// tracks the connection (unbind/rebind cleared it), re-register rather than
	// leaving the sender writerless.
	if sm.GetPendingCBReplies(c.connectionID) == nil {
		// Serialized against fore-channel replies on the same writeMu, and
		// bounded, because a callback that never returns from the socket holds
		// that lock against every reply behind it.
		writer := v4state.ConnWriter(func(data []byte) error {
			return c.write(data, c.callbackWriteTimeout())
		})
		sm.RegisterConnWriter(c.connectionID, writer)
	}

	for _, b := range backBound {
		// Idempotent: returns immediately when the session already has a sender.
		sm.StartBackchannelSender(ctx, b.SessionID)

		logger.Debug("Backchannel registered for connection",
			"conn_id", c.connectionID,
			"session_id", b.SessionID.String(),
			"direction", b.Direction.String())
	}
}

// callbackWriteTimeout bounds one back-channel write. It is the configured
// fore-channel write timeout, capped at the budget the backchannel sender
// charges every attempt for: the recall watchdog is derived from that budget,
// and a write allowed to outlast it has the recall give up and start revoking
// while its own callback is still on the socket.
func (c *NFSConnection) callbackWriteTimeout() time.Duration {
	if w := c.server.config.Timeouts.Write; w > 0 && w < v4state.CallbackWriteTimeout {
		return w
	}
	return v4state.CallbackWriteTimeout
}

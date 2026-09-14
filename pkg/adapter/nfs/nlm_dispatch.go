package nfs

import (
	"context"
	"fmt"
	"net"
	"time"

	nlm "github.com/marmos91/dittofs/internal/adapter/nfs/nlm"
	nlm_callback "github.com/marmos91/dittofs/internal/adapter/nfs/nlm/callback"
	nlm_handlers "github.com/marmos91/dittofs/internal/adapter/nfs/nlm/handlers"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	"github.com/marmos91/dittofs/internal/logger"
)

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

	if err := cancelledBeforeHandler(ctx, "NLM", procedure.Name, call.XID); err != nil {
		return nil, err
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
	// Snapshot under sidecarMu so the read does not race the transport's
	// publish (startUDP) or its shutdown snapshot (udpSidecar.Stop).
	c.server.sidecarMu.Lock()
	udpConn := c.server.udpConn
	c.server.sidecarMu.Unlock()
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

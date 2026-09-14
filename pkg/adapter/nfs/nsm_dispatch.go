package nfs

import (
	"context"
	"fmt"
	"time"

	nsm "github.com/marmos91/dittofs/internal/adapter/nfs/nsm"
	nsm_handlers "github.com/marmos91/dittofs/internal/adapter/nfs/nsm/handlers"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	"github.com/marmos91/dittofs/internal/logger"
)

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

	if err := cancelledBeforeHandler(ctx, "NSM", procedure.Name, call.XID); err != nil {
		return nil, err
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

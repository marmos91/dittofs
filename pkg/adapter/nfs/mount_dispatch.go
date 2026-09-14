package nfs

import (
	"context"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/middleware"
	mount_dispatch "github.com/marmos91/dittofs/internal/adapter/nfs/mount"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	"github.com/marmos91/dittofs/internal/logger"
)

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
	procedure, ok := mount_dispatch.MountDispatchTable[call.Procedure]
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

	if err := cancelledBeforeHandler(ctx, "Mount", procedure.Name, call.XID); err != nil {
		return nil, err
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

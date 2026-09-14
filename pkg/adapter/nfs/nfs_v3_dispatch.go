package nfs

import (
	"context"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/middleware"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	v3 "github.com/marmos91/dittofs/internal/adapter/nfs/v3"
	"github.com/marmos91/dittofs/internal/logger"
)

func (c *NFSConnection) handleNFSProcedure(ctx context.Context, call *rpc.RPCCallMessage, data []byte, clientAddr string) ([]byte, error) {
	// Log first v3 call per server lifetime
	c.server.logV3FirstUse()

	// Look up procedure in dispatch table
	procedure, ok := v3.NfsDispatchTable[call.Procedure]
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
	handlerCtx := middleware.ExtractHandlerContext(ctx, call, clientAddr, share, procedure.Name)

	// Log request with trace context
	logger.DebugCtx(ctx, "NFS request",
		"procedure", procedure.Name,
		"share", share,
		"client", clientAddr,
		"xid", fmt.Sprintf("0x%x", call.XID))

	if err := cancelledBeforeHandler(ctx, "NFS", procedure.Name, call.XID); err != nil {
		return nil, err
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
	return c.withDRC(ctx, call, data, clientAddr,
		isCacheable(call.Procedure), "NFS "+procedure.Name,
		func() ([]byte, bool, error) {
			start := time.Now()
			result, err := procedure.Handler(
				handlerCtx,
				c.server.nfsHandler,
				c.server.Registry,
				data,
			)
			c.recordOp(procedure.Name, start, err == nil && (result == nil || result.NFSStatus == 0))

			if result == nil {
				// Nothing to cache (e.g. a decode failure); the deferred
				// release leaves a later legitimate retry free to run.
				return nil, false, err
			}
			return result.Data, true, err
		})
}

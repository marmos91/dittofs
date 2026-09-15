package nfs

import (
	"context"
	"fmt"
	"time"

	nfsauth "github.com/marmos91/dittofs/internal/adapter/nfs/auth"
	"github.com/marmos91/dittofs/internal/adapter/nfs/middleware"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	nfs_types "github.com/marmos91/dittofs/internal/adapter/nfs/types"
	v3 "github.com/marmos91/dittofs/internal/adapter/nfs/v3"
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

	// Enforce the share's export auth-flavor policy on every operation, not
	// only at MOUNT. A file handle stays valid across restarts, so a client
	// that mounted while the share still accepted its flavor would otherwise
	// keep full read/write access after an administrator tightened the policy,
	// which is only enforced against new mounts.
	//
	// The gate sits here rather than in the v3 auth-context builder for two
	// reasons: it must be outside the handler's auth-context cache, whose
	// entries survive a policy change, and several procedures (GETATTR, FSINFO,
	// PATHCONF) resolve no auth context at all. Tightening a share therefore
	// takes effect on the next operation of an already-mounted client.
	//
	// An empty share name means no handle was carried (NULL) or the handle did
	// not resolve; the handler answers those.
	//
	// ponytail: one GetShare snapshot copy per RPC. Narrow it to a
	// flavor-policy accessor only if this shows up in a profile.
	if share != "" {
		if shareRef, shareErr := c.server.Registry.GetShare(share); shareErr == nil {
			if accessErr := nfsauth.CheckExportAccess(ctx, shareRef, handlerCtx.AuthFlavor, nil, nil); accessErr != nil {
				logger.Warn("NFSv3 operation denied by export auth policy",
					"procedure", procedure.Name,
					"share", share,
					"client", clientAddr,
					"reason", accessErr)
				return c.makeStatusOnlyResponse(nfs_types.NFS3ErrAccess).Data, nil
			}
		}
	}

	// Check if this operation is blocked via adapter settings.
	if c.isOperationBlocked(procedure.Name) {
		logger.Debug("NFSv3 operation blocked by adapter settings",
			"procedure", procedure.Name,
			"client", clientAddr,
			"xid", fmt.Sprintf("0x%x", call.XID))

		// Return a minimal NFS3ERR_NOTSUPP response
		return c.makeStatusOnlyResponse(nfs_types.NFS3ErrNotSupp).Data, nil
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

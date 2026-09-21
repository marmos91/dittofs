package nfs

import (
	"context"
	"encoding/binary"
	"errors"
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
	// An empty share name means the request carried no handle (NULL) or the
	// handle did not decode, so there is no export to apply a policy to. A name
	// that no longer resolves is different: the handle named a share the
	// registry has since dropped, and serving it would run the operation with
	// the policy unread, so it is refused as stale rather than let through.
	//
	// ponytail: one GetShare snapshot copy per RPC. Narrow it to a
	// flavor-policy accessor only if this shows up in a profile.
	if share != "" {
		shareRef, shareErr := c.server.Registry.GetShare(share)
		if shareErr != nil {
			logger.Warn("NFSv3 operation refused: share no longer resolves",
				"procedure", procedure.Name,
				"share", share,
				"client", clientAddr,
				"error", shareErr)
			return v3StatusOnlyReply(call.Procedure, nfs_types.NFS3ErrStale), nil
		}
		// A disabled share admits nobody, and on this path nothing else says
		// so. The adapters that decide access when a client establishes
		// something -- SMB at TREE_CONNECT, NFS at MOUNT -- re-read the flag by
		// dropping what they established. NFSv3 establishes nothing per
		// operation: the handle is the credential and it outlives restarts, so
		// an already-mounted client reaches the share through this function
		// alone. Invalidating the auth cache on disable does not cover it
		// either, because the rebuilt context consults the user record and the
		// share's permissions, neither of which carries the enabled flag.
		//
		// STALE rather than ACCES, matching what a handle into a share that no
		// longer resolves answers just above, and what NFSv4 answers for the
		// same condition: the share is gone as far as this client is concerned,
		// and a client that is told ACCES may keep the handle and retry.
		if !shareRef.Enabled {
			logger.Warn("NFSv3 operation refused: share is disabled",
				"procedure", procedure.Name,
				"share", share,
				"client", clientAddr)
			return v3StatusOnlyReply(call.Procedure, nfs_types.NFS3ErrStale), nil
		}
		if accessErr := nfsauth.CheckExportAccess(ctx, shareRef, handlerCtx.AuthFlavor, nil, nil); accessErr != nil {
			logger.Warn("NFSv3 operation denied by export auth policy",
				"procedure", procedure.Name,
				"share", share,
				"client", clientAddr,
				"reason", accessErr)
			return v3StatusOnlyReply(call.Procedure, nfs_types.NFS3ErrAccess), nil
		}
	}

	// Check if this operation is blocked via adapter settings.
	if c.isOperationBlocked(procedure.Name) {
		logger.Debug("NFSv3 operation blocked by adapter settings",
			"procedure", procedure.Name,
			"client", clientAddr,
			"xid", fmt.Sprintf("0x%x", call.XID))

		return v3StatusOnlyReply(call.Procedure, nfs_types.NFS3ErrNotSupp), nil
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
			// A cancelled request's reply is not a decision the server made, it
			// is what the cancellation was encoded as: the handlers answer one
			// with a non-nil response carrying NFS3ErrIO plus the context error.
			// Caching that poisons the client's retransmission — the mutation
			// never ran, and the retry is answered from the cache with the
			// fabricated failure instead of being executed. The deferred release
			// leaves the reservation, so the retransmit runs for real.
			//
			// A non-cancellation error is different and must still be cached: a
			// handler reports a real status (NFS3ErrStale, NFS3ErrAccess) by
			// returning it alongside a non-nil error, and replaying that is
			// exactly what the cache is for.
			cancelled := errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
			return result.Data, !cancelled, err
		})
}

// v3FailureArmBytes is the encoded size, in bytes after the 4-byte status word,
// of each NFSv3 procedure's failure reply. RFC 1813 makes every result a union
// discriminated on the status, and the failure arm still carries attributes for
// most procedures: a post_op_attr contributes its 4-byte FALSE discriminant and
// a wcc_data two of them. The sizes here are what this package's own response
// codecs emit for a non-OK status, so a refusal answered before any handler runs
// is indistinguishable on the wire from one the handler produced.
//
// A procedure missing from the table gets the status alone, which is what NULL
// and GETATTR encode anyway.
var v3FailureArmBytes = map[uint32]int{
	nfs_types.NFSProcSetAttr:     8,  // wcc_data
	nfs_types.NFSProcLookup:      4,  // post_op_attr (dir)
	nfs_types.NFSProcAccess:      4,  // post_op_attr
	nfs_types.NFSProcReadLink:    4,  // post_op_attr
	nfs_types.NFSProcRead:        4,  // post_op_attr
	nfs_types.NFSProcWrite:       8,  // wcc_data
	nfs_types.NFSProcCreate:      8,  // wcc_data (dir)
	nfs_types.NFSProcMkdir:       8,  // wcc_data (dir)
	nfs_types.NFSProcSymlink:     8,  // wcc_data (dir)
	nfs_types.NFSProcMknod:       8,  // wcc_data (dir)
	nfs_types.NFSProcRemove:      8,  // wcc_data (dir)
	nfs_types.NFSProcRmdir:       8,  // wcc_data (dir)
	nfs_types.NFSProcRename:      16, // wcc_data for each of the two directories
	nfs_types.NFSProcLink:        12, // post_op_attr (file) + wcc_data (dir)
	nfs_types.NFSProcReadDir:     4,  // post_op_attr (dir)
	nfs_types.NFSProcReadDirPlus: 4,  // post_op_attr (dir)
	nfs_types.NFSProcPathConf:    4,  // post_op_attr
	nfs_types.NFSProcCommit:      8,  // wcc_data
}

// v3StatusOnlyReply encodes an NFSv3 error reply for one procedure: the status
// followed by that procedure's failure arm with every attribute marked absent.
// It is what the dispatch layer answers with when a request is refused before
// any procedure handler runs, so no response type is available to encode. A
// reply shorter than the procedure's union arm is a decode error at the client,
// so the length has to follow the procedure rather than be one fixed shape.
func v3StatusOnlyReply(procedure, status uint32) []byte {
	reply := make([]byte, 4+v3FailureArmBytes[procedure])
	binary.BigEndian.PutUint32(reply[0:4], status)
	// The remaining bytes are the arm's attribute-present discriminants, all
	// already zero (FALSE).
	return reply
}

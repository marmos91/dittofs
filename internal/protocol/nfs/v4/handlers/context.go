package handlers

import (
	"context"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/rpc"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
)

// ExtractV4HandlerContext creates a CompoundContext from an RPC call message.
//
// This extracts AUTH_UNIX credentials (UID, GID, supplementary GIDs) from the
// RPC call and populates the CompoundContext. CurrentFH and SavedFH start as nil
// and are set by filehandle operations within the COMPOUND (PUTFH, PUTROOTFH, etc.).
//
// Parameters:
//   - ctx: Go context for cancellation and timeout control
//   - call: The RPC call message containing authentication data
//   - clientAddr: The remote address of the client connection
//
// Returns a CompoundContext with extracted authentication information.
func ExtractV4HandlerContext(
	ctx context.Context,
	call *rpc.RPCCallMessage,
	clientAddr string,
) *types.CompoundContext {
	compCtx := &types.CompoundContext{
		Context:    ctx,
		ClientAddr: clientAddr,
		AuthFlavor: call.GetAuthFlavor(),
	}

	// Only attempt to parse Unix credentials if AUTH_UNIX is specified
	if compCtx.AuthFlavor != rpc.AuthUnix {
		return compCtx
	}

	authBody := call.GetAuthBody()
	if len(authBody) == 0 {
		logger.Warn("NFSv4 AUTH_UNIX specified but auth body is empty", "client", clientAddr)
		return compCtx
	}

	unixAuth, err := rpc.ParseUnixAuth(authBody)
	if err != nil {
		logger.Warn("NFSv4 failed to parse AUTH_UNIX credentials",
			"client", clientAddr,
			"error", err)
		return compCtx
	}

	logger.Debug("NFSv4 parsed Unix auth",
		"client", clientAddr,
		"uid", unixAuth.UID,
		"gid", unixAuth.GID,
		"ngids", len(unixAuth.GIDs))

	compCtx.UID = &unixAuth.UID
	compCtx.GID = &unixAuth.GID
	compCtx.GIDs = unixAuth.GIDs

	return compCtx
}

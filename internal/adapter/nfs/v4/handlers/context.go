package handlers

import (
	"context"

	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc/gss"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/logger"
)

// authCall is the minimal view of an RPC call message needed to extract
// authentication credentials. *rpc.RPCCallMessage satisfies it. Keeping the
// surface narrow lets this protocol-concern helper stay decoupled from the
// full concrete message type (and makes it testable with a lightweight stub).
type authCall interface {
	GetAuthFlavor() uint32
	GetAuthBody() []byte
}

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
// Returns a CompoundContext with extracted authentication information and an
// NFSv4 status. The status is NFS4_OK on success. When the call claims the
// RPCSEC_GSS auth flavor but no verified GSS identity is present in the context
// (i.e. the GSS unwrap path never resolved an identity), the context is rejected
// with (nil, NFS4ERR_WRONGSEC) so the COMPOUND is not executed as an
// unauthenticated/anonymous request.
func ExtractV4HandlerContext(
	ctx context.Context,
	call authCall,
	clientAddr string,
) (*types.CompoundContext, uint32) {
	compCtx := &types.CompoundContext{
		Context:    ctx,
		ClientAddr: clientAddr,
		AuthFlavor: call.GetAuthFlavor(),
	}

	// Check for GSS identity from context.Value (set by handleRPCCall GSS interception)
	if compCtx.AuthFlavor == rpc.AuthRPCSECGSS {
		if gssIdentity := gss.IdentityFromContext(ctx); gssIdentity != nil {
			compCtx.UID = gssIdentity.UID
			compCtx.GID = gssIdentity.GID
			compCtx.GIDs = gssIdentity.GIDs

			logger.Debug("NFSv4 using GSS identity",
				"client", clientAddr,
				"uid", gssIdentity.UID,
				"gid", gssIdentity.GID,
				"ngids", len(gssIdentity.GIDs))

			return compCtx, types.NFS4_OK
		}
		// GSS auth flavor claimed but no verified identity in context: reject the
		// COMPOUND rather than letting it proceed with a nil-UID (anonymous) context.
		logger.Warn("NFSv4 RPCSEC_GSS auth flavor but no GSS identity in context — rejecting with NFS4ERR_WRONGSEC",
			"client", clientAddr)
		return nil, types.NFS4ERR_WRONGSEC
	}

	// Only attempt to parse Unix credentials if AUTH_UNIX is specified
	if compCtx.AuthFlavor != rpc.AuthUnix {
		return compCtx, types.NFS4_OK
	}

	authBody := call.GetAuthBody()
	if len(authBody) == 0 {
		logger.Warn("NFSv4 AUTH_UNIX specified but auth body is empty", "client", clientAddr)
		return compCtx, types.NFS4_OK
	}

	unixAuth, err := rpc.ParseUnixAuth(authBody)
	if err != nil {
		logger.Warn("NFSv4 failed to parse AUTH_UNIX credentials",
			"client", clientAddr,
			"error", err)
		return compCtx, types.NFS4_OK
	}

	logger.Debug("NFSv4 parsed Unix auth",
		"client", clientAddr,
		"uid", unixAuth.UID,
		"gid", unixAuth.GID,
		"ngids", len(unixAuth.GIDs))

	compCtx.UID = &unixAuth.UID
	compCtx.GID = &unixAuth.GID
	compCtx.GIDs = unixAuth.GIDs

	return compCtx, types.NFS4_OK
}

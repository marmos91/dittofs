// Package middleware provides authentication extraction and future middleware
// components for the NFS adapter dispatch pipeline.
package middleware

import (
	"context"

	mount "github.com/marmos91/dittofs/internal/adapter/nfs/mount/handlers"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc/gss"
	nfs "github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers"
	"github.com/marmos91/dittofs/internal/logger"
)

// ExtractHandlerContext creates an NFSHandlerContext from an RPC call message.
// This centralizes authentication extraction logic and ensures consistent
// handling across all procedures.
//
// For AUTH_UNIX credentials, this parses the Unix auth body and extracts
// the UID, GID, and supplementary GIDs. For other auth flavors (like AUTH_NULL),
// the Unix credential fields are left as nil.
//
// Parsing failures are logged but do not cause the procedure to fail -
// the procedure receives a context with nil credentials and can decide
// how to handle unauthenticated requests.
//
// **Context Propagation:**
//
// The Go context passed to this function is embedded in the returned NFSHandlerContext.
// This context will be passed through to all procedure handlers, enabling them
// to respect cancellation signals from the server or client disconnect events.
//
// Parameters:
//   - ctx: The Go context for cancellation and timeout control
//   - call: The RPC call message containing authentication data
//   - clientAddr: The remote address of the client connection
//   - share: The share name extracted from file handle (empty if not available)
//   - procedure: Name of the procedure (for logging purposes)
//
// Returns:
//   - *nfs.NFSHandlerContext with extracted authentication information and propagated context
func ExtractHandlerContext(
	ctx context.Context,
	call *rpc.RPCCallMessage,
	clientAddr string,
	share string,
	procedure string,
) *nfs.NFSHandlerContext {
	handlerCtx := &nfs.NFSHandlerContext{
		Context:    ctx,
		ClientAddr: clientAddr,
		Share:      share,
		AuthFlavor: call.GetAuthFlavor(),
	}

	// Check for GSS identity from context.Value (set by handleRPCCall GSS interception)
	if handlerCtx.AuthFlavor == rpc.AuthRPCSECGSS {
		if gssIdentity := gss.IdentityFromContext(ctx); gssIdentity != nil {
			handlerCtx.UID = gssIdentity.UID
			handlerCtx.GID = gssIdentity.GID
			handlerCtx.GIDs = gssIdentity.GIDs

			logger.Debug("Using GSS identity",
				"procedure", procedure,
				"uid", gssIdentity.UID,
				"gid", gssIdentity.GID,
				"ngids", len(gssIdentity.GIDs))

			return handlerCtx
		}
		// GSS auth flavor but no identity in context - this should not happen
		// for DATA requests, but can happen if GSS interception was bypassed
		logger.Warn("RPCSEC_GSS auth flavor but no GSS identity in context",
			"procedure", procedure)
		return handlerCtx
	}

	// Only attempt to parse Unix credentials if AUTH_UNIX is specified
	if handlerCtx.AuthFlavor != rpc.AuthUnix {
		return handlerCtx
	}

	// Get auth body
	authBody := call.GetAuthBody()
	if len(authBody) == 0 {
		logger.Warn("AUTH_UNIX specified but auth body is empty", "procedure", procedure)
		return handlerCtx
	}

	// Parse Unix auth credentials
	unixAuth, err := rpc.ParseUnixAuth(authBody)
	if err != nil {
		// Log the parsing failure - this is unexpected and may indicate
		// a protocol issue or malicious client
		logger.Warn("Failed to parse AUTH_UNIX credentials",
			"procedure", procedure,
			"error", err)
		return handlerCtx
	}

	// Log successful auth parsing at debug level
	logger.Debug("Parsed Unix auth",
		"procedure", procedure,
		"uid", unixAuth.UID,
		"gid", unixAuth.GID,
		"ngids", len(unixAuth.GIDs))

	handlerCtx.UID = &unixAuth.UID
	handlerCtx.GID = &unixAuth.GID
	handlerCtx.GIDs = unixAuth.GIDs

	return handlerCtx
}

// ExtractMountHandlerContext creates a MountHandlerContext from an RPC call message.
// This extracts authentication credentials for mount protocol requests.
//
// Parameters:
//   - ctx: The Go context for cancellation and timeout control
//   - call: The RPC call message containing authentication data
//   - clientAddr: The remote address of the client connection
//   - kerberosEnabled: Whether Kerberos authentication is available
//
// Returns:
//   - *mount.MountHandlerContext with extracted authentication information
func ExtractMountHandlerContext(
	ctx context.Context,
	call *rpc.RPCCallMessage,
	clientAddr string,
	kerberosEnabled bool,
) *mount.MountHandlerContext {
	handlerCtx := &mount.MountHandlerContext{
		Context:         ctx,
		ClientAddr:      clientAddr,
		AuthFlavor:      call.GetAuthFlavor(),
		KerberosEnabled: kerberosEnabled,
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

	return handlerCtx
}

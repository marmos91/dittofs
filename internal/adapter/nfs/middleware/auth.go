// Package middleware provides authentication extraction and future middleware
// components for the NFS adapter dispatch pipeline.
package middleware

import (
	"context"

	nfsauth "github.com/marmos91/dittofs/internal/adapter/nfs/auth"
	mount "github.com/marmos91/dittofs/internal/adapter/nfs/mount/handlers"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc/gss"
	nfs "github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers"
	"github.com/marmos91/dittofs/internal/logger"
)

// unixTranslator parses AUTH_UNIX credentials into an identity. It is
// stateless and shared, and is constructed without an identity store because
// this layer only extracts the wire credentials: the UID-to-user resolution
// that turns them into an authorization decision happens later, when the
// per-operation auth context is built (auth.BuildAuthContext).
var unixTranslator = nfsauth.NewUnixTranslator(nil)

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

	// Parse Unix auth credentials.
	//
	// decision: a malformed credential leaves the context anonymous rather
	// than failing the RPC. The wire already asserts the identity, so an
	// unparseable one carries no authorization evidence at all — which is the
	// same state as AUTH_NULL. Downstream, auth.ResolveSharePermission gates
	// that anonymous context on the share's default_permission, so it is
	// refused wherever the share is not open to guests. Withdraw this
	// fail-open only if an anonymous context can reach a share that grants
	// guests more than the share's own default.
	result, _, err := unixTranslator.Translate(ctx, authBody)
	if err != nil {
		logger.Warn("Failed to parse AUTH_UNIX credentials",
			"procedure", procedure,
			"error", err)
		return handlerCtx
	}

	identity := result.Identity
	logger.Debug("Parsed Unix auth",
		"procedure", procedure,
		"uid", *identity.UID,
		"gid", *identity.GID,
		"ngids", len(identity.GIDs))

	handlerCtx.UID = identity.UID
	handlerCtx.GID = identity.GID
	handlerCtx.GIDs = identity.GIDs

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

	// Parse Unix credentials if AUTH_UNIX. A parse failure leaves the mount
	// context anonymous, matching ExtractHandlerContext's decision above.
	if handlerCtx.AuthFlavor == rpc.AuthUnix {
		if result, _, err := unixTranslator.Translate(ctx, call.GetAuthBody()); err == nil {
			handlerCtx.UID = result.Identity.UID
			handlerCtx.GID = result.Identity.GID
			handlerCtx.GIDs = result.Identity.GIDs
		}
	}

	return handlerCtx
}

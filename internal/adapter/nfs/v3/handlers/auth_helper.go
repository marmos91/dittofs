package handlers

import (
	"context"
	"errors"
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/nfs/auth"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ErrShareAccessDenied is returned when a user doesn't have permission to
// access a share. It aliases the shared auth sentinel so existing v3 callers
// (and errors.Is checks) keep working.
var ErrShareAccessDenied = auth.ErrShareAccessDenied

// authDenialStatus maps a BuildAuthContextWithMapping / GetCachedAuthContext
// error to an NFS3 status. A share-permission denial (ErrShareAccessDenied) is an
// authorization decision → NFS3ErrAccess (EACCES "permission denied"), not a
// server fault → NFS3ErrIO. Mirrors the NFSv4 path (v4/handlers/helpers.go),
// which maps ErrShareAccessDenied to NFS4ERR_ACCESS.
func authDenialStatus(err error) uint32 {
	if errors.Is(err, ErrShareAccessDenied) {
		return types.NFS3ErrAccess
	}
	return types.NFS3ErrIO
}

// logAuthCtxError logs a failure to build the per-request auth context at the
// level appropriate to its cause: a share-permission denial (ErrShareAccessDenied)
// is an expected authorization outcome → Warn; anything else (share-not-found,
// identity-mapping failure) is a genuine fault → Error. Pairs with
// authDenialStatus so the log level matches the NFS status returned.
func logAuthCtxError(ctx context.Context, err error, operation string, args ...any) {
	if errors.Is(err, ErrShareAccessDenied) {
		logWarn(ctx, err, operation+": share access denied", args...)
		return
	}
	logError(ctx, err, operation+": failed to build auth context", args...)
}

// formatUID formats an optional UID for logging.
// Returns "nil" if the pointer is nil, otherwise the numeric value as string.
func formatUID(uid *uint32) string {
	if uid == nil {
		return "nil"
	}
	return fmt.Sprintf("%d", *uid)
}

// BuildAuthContextWithMapping creates an AuthContext with share-level identity mapping applied.
//
// This is a shared helper function used by all NFS v3 handlers to ensure consistent
// identity mapping across all operations. It:
//  1. Uses the share name from the connection layer (already extracted from file handle)
//  2. Looks up the DittoFS user by UID using reverse lookup (GetUserByUID)
//  3. Checks share-level permissions for the user
//  4. Applies identity mapping rules from the registry (all_squash, root_squash)
//  5. Returns effective credentials for permission checking
//
// Parameters:
//   - nfsCtx: The NFS handler context with client and auth information
//   - reg: Registry to apply identity mapping
//   - shareName: Share name (extracted at connection layer from file handle)
//
// Returns:
//   - *metadata.AuthContext: Auth context with effective (mapped) credentials
//   - error: If identity mapping fails, access is denied, or context is cancelled
func BuildAuthContextWithMapping(
	nfsCtx *NFSHandlerContext,
	reg nfsRuntime,
	shareName string,
) (*metadata.AuthContext, error) {
	authMethod := "anonymous"
	if nfsCtx.AuthFlavor == rpc.AuthUnix {
		authMethod = "unix"
	}

	effectiveAuthCtx, err := auth.BuildAuthContext(nfsCtx.Context, reg, shareName, auth.Credentials{
		UID:        nfsCtx.UID,
		GID:        nfsCtx.GID,
		GIDs:       nfsCtx.GIDs,
		ClientAddr: nfsCtx.ClientAddr,
		AuthMethod: authMethod,
	})
	if err != nil {
		return nil, err
	}

	logger.DebugCtx(nfsCtx.Context, "Auth context created",
		"share", shareName,
		"uid", formatUID(effectiveAuthCtx.Identity.UID),
		"gid", formatUID(effectiveAuthCtx.Identity.GID))

	return effectiveAuthCtx, nil
}

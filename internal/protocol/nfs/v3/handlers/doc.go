package handlers

import (
	"fmt"
	"sync"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metrics"
	"github.com/marmos91/dittofs/pkg/registry"
)

// Handler is the concrete implementation for NFS v3 protocol handlers.
// It processes all NFSv3 procedures (LOOKUP, READ, WRITE, etc.) and uses
// the registry to access per-share stores and configuration.
type Handler struct {
	// Registry provides access to all stores and shares
	// Exported to allow injection by the NFS adapter
	Registry *registry.Registry

	// Metrics collects observability data for NFS operations
	// Optional - may be nil to disable metrics with zero overhead
	Metrics metrics.NFSMetrics

	// authCache caches auth contexts per (share, UID, GID) to avoid
	// repeated registry lookups on every WRITE request.
	// Key: "share:uid:gid", Value: *metadata.AuthContext
	authCache sync.Map
}

// authCacheKey generates a cache key for auth context caching.
func authCacheKey(share string, uid, gid *uint32) string {
	uidVal := uint32(0xFFFFFFFF) // sentinel for nil
	gidVal := uint32(0xFFFFFFFF)
	if uid != nil {
		uidVal = *uid
	}
	if gid != nil {
		gidVal = *gid
	}
	return fmt.Sprintf("%s:%d:%d", share, uidVal, gidVal)
}

// GetCachedAuthContext returns a cached auth context or builds a new one.
// This avoids repeated BuildAuthContextWithMapping calls for the same client.
func (h *Handler) GetCachedAuthContext(
	ctx *NFSHandlerContext,
) (*metadata.AuthContext, error) {
	key := authCacheKey(ctx.Share, ctx.UID, ctx.GID)

	// Fast path: check cache
	if cached, ok := h.authCache.Load(key); ok {
		authCtx := cached.(*metadata.AuthContext)
		// Return a copy with the current request's context
		return &metadata.AuthContext{
			Context:       ctx.Context,
			ClientAddr:    ctx.ClientAddr,
			AuthMethod:    authCtx.AuthMethod,
			Identity:      authCtx.Identity,
			ShareReadOnly: authCtx.ShareReadOnly,
		}, nil
	}

	// Slow path: build auth context
	authCtx, err := BuildAuthContextWithMapping(ctx, h.Registry, ctx.Share)
	if err != nil {
		return nil, err
	}

	// Cache for future requests
	h.authCache.Store(key, authCtx)

	return authCtx, nil
}

// convertFileAttrToNFS converts metadata file attributes to NFS wire format.
// Extracts the file ID from the handle and converts the attributes.
func (h *Handler) convertFileAttrToNFS(fileHandle metadata.FileHandle, fileAttr *metadata.FileAttr) *types.NFSFileAttr {
	fileid := xdr.ExtractFileID(fileHandle)
	return xdr.MetadataToNFS(fileAttr, fileid)
}

// getFileOrError retrieves a file from the metadata store with error handling.
// Checks for context cancellation and returns appropriate NFS status codes.
//
// Returns:
//   - file: The retrieved file (nil on error)
//   - status: NFS3OK on success, NFS3ErrIO on cancellation, NFS3ErrStale on not found
//   - error: Context error if cancelled, nil otherwise
func (h *Handler) getFileOrError(
	ctx *NFSHandlerContext,
	fileHandle metadata.FileHandle,
	operationName string,
	handleBytes []byte,
) (*metadata.File, uint32, error) {
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)
	metaSvc := h.Registry.GetMetadataService()

	file, err := metaSvc.GetFile(ctx.Context, fileHandle)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.DebugCtx(ctx.Context, operationName+" cancelled during file lookup", "handle", fmt.Sprintf("%x", handleBytes), "client", clientIP, "error", ctx.Context.Err())
			return nil, types.NFS3ErrIO, ctx.Context.Err()
		}

		logger.DebugCtx(ctx.Context, operationName+" failed: handle not found", "handle", fmt.Sprintf("%x", handleBytes), "client", clientIP, "error", err)
		return nil, types.NFS3ErrStale, nil
	}

	return file, types.NFS3OK, nil
}

// buildAuthContextWithWCCError builds an auth context or returns WCC error data.
// This helper consolidates the common pattern in mutation handlers of:
//  1. Calling BuildAuthContextWithMapping
//  2. Checking for context cancellation
//  3. Logging appropriate error messages
//  4. Constructing WCC after attributes for error responses
//
// Returns:
//   - authCtx: Non-nil auth context on success, nil on error
//   - wccAfter: Nil on success, populated NFS attributes on error (for WCC response)
//   - err: Context cancellation error if cancelled, nil otherwise
//
// Usage pattern:
//
//	authCtx, wccAfter, err := h.buildAuthContextWithWCCError(ctx, handle, &file.FileAttr, "CREATE", req.Filename, req.DirHandle)
//	if authCtx == nil {
//	    return &CreateResponse{Status: types.NFS3ErrIO, DirBefore: wccBefore, DirAfter: wccAfter}, err
//	}
func (h *Handler) buildAuthContextWithWCCError(
	ctx *NFSHandlerContext,
	handle metadata.FileHandle,
	fileAttr *metadata.FileAttr,
	operation string,
	filename string,
	dirHandleBytes []byte,
) (*metadata.AuthContext, *types.NFSFileAttr, error) {
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	authCtx, err := BuildAuthContextWithMapping(ctx, h.Registry, ctx.Share)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.DebugCtx(ctx.Context, operation+" cancelled during auth context building", "name", filename, "handle", fmt.Sprintf("%x", dirHandleBytes), "client", clientIP, "error", ctx.Context.Err())

			wccAfter := h.convertFileAttrToNFS(handle, fileAttr)
			return nil, wccAfter, ctx.Context.Err()
		}

		traceError(ctx.Context, err, operation+" failed: failed to build auth context", "name", filename, "handle", fmt.Sprintf("%x", dirHandleBytes), "client", clientIP)

		wccAfter := h.convertFileAttrToNFS(handle, fileAttr)
		return nil, wccAfter, nil
	}

	return authCtx, nil, nil
}

// checkMFsymlinkByHandle checks if a file referenced by handle is an unconverted MFsymlink.
// This is used by READLINK when ReadSymlink fails to check if the file is actually
// an SMB-created MFsymlink that hasn't been converted yet.
//
// Parameters:
//   - ctx: NFS handler context containing share info
//   - fileHandle: Handle to the file to check
//
// Returns MFsymlinkResult with detection result and modified attributes.
func (h *Handler) checkMFsymlinkByHandle(ctx *NFSHandlerContext, fileHandle metadata.FileHandle) MFsymlinkResult {
	metaSvc := h.Registry.GetMetadataService()

	// Get file metadata
	file, err := metaSvc.GetFile(ctx.Context, fileHandle)
	if err != nil {
		return MFsymlinkResult{IsMFsymlink: false}
	}

	// Use the helper function to check MFsymlink
	return checkMFsymlink(ctx.Context, h.Registry, ctx.Share, file)
}

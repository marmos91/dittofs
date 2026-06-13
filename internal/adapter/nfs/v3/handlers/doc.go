package handlers

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/adapter"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// Handler is the concrete implementation for NFS v3 protocol handlers.
// It processes all NFSv3 procedures (LOOKUP, READ, WRITE, etc.) and uses
// the registry to access per-share stores and configuration.
type Handler struct {
	// Registry provides access to all stores and shares
	// Exported to allow injection by the NFS adapter
	Registry nfsRuntime

	// authCache caches auth contexts per (share, UID, GID) to avoid
	// repeated registry lookups on every WRITE request.
	// Key: "share:uid:gid", Value: *metadata.AuthContext
	authCache sync.Map
}

// authCacheKey generates a cache key for auth context caching.
//
// The key covers every input that BuildAuthContextWithMapping resolves on:
// the share, the auth flavor (which selects the AuthMethod string), and the
// full credential set (UID, primary GID, and supplementary GIDs). Including
// the supplementary GIDs and flavor avoids returning a cached context that
// was built for a different credential set that happens to share UID/GID.
func authCacheKey(ctx *NFSHandlerContext) string {
	uidVal := uint32(0xFFFFFFFF) // sentinel for nil
	gidVal := uint32(0xFFFFFFFF)
	if ctx.UID != nil {
		uidVal = *ctx.UID
	}
	if ctx.GID != nil {
		gidVal = *ctx.GID
	}

	var b strings.Builder
	b.WriteString(ctx.Share)
	appendUint(&b, uint64(ctx.AuthFlavor))
	appendUint(&b, uint64(uidVal))
	appendUint(&b, uint64(gidVal))
	for _, g := range ctx.GIDs {
		appendUint(&b, uint64(g))
	}
	return b.String()
}

// appendUint writes ":<n>" to b without the reflection overhead of fmt, since
// authCacheKey runs on every cached-op RPC.
func appendUint(b *strings.Builder, n uint64) {
	b.WriteByte(':')
	b.WriteString(strconv.FormatUint(n, 10))
}

// GetCachedAuthContext returns a cached auth context or builds a new one.
// This avoids repeated BuildAuthContextWithMapping calls for the same client.
func (h *Handler) GetCachedAuthContext(
	ctx *NFSHandlerContext,
) (*metadata.AuthContext, error) {
	key := authCacheKey(ctx)

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

	// Cache only the request-independent fields. Storing authCtx directly would
	// pin the first request's Context (and any values/cancellation attached to
	// it) and ClientAddr for the lifetime of the entry; the hit path always
	// re-derives those from the current request anyway.
	h.authCache.Store(key, &metadata.AuthContext{
		AuthMethod:    authCtx.AuthMethod,
		Identity:      authCtx.Identity,
		ShareReadOnly: authCtx.ShareReadOnly,
	})

	return authCtx, nil
}

// convertFileAttrToNFS converts metadata file attributes to NFS wire format.
// Extracts the file ID from the handle and converts the attributes.
func (h *Handler) convertFileAttrToNFS(fileHandle metadata.FileHandle, fileAttr *metadata.FileAttr) *types.NFSFileAttr {
	fileid := xdr.ExtractFileID(fileHandle)
	return xdr.MetadataToNFS(fileAttr, fileid)
}

// wccAfterOrFallback returns the post-op WCC attributes for handle by fetching
// the current file from the store, falling back to the supplied pre-op
// attributes when the fetch fails or returns nil. This guards against
// dereferencing a nil *metadata.File on the WCC error paths (a best-effort
// GetFile is intentionally ignored for its error).
func (h *Handler) wccAfterOrFallback(
	ctx *NFSHandlerContext,
	metaSvc *metadata.Service,
	handle metadata.FileHandle,
	fallback *metadata.FileAttr,
) *types.NFSFileAttr {
	if updated, err := metaSvc.GetFile(ctx.Context, handle); err == nil && updated != nil {
		return h.convertFileAttrToNFS(handle, &updated.FileAttr)
	}
	return h.convertFileAttrToNFS(handle, fallback)
}

// dirWccPair derives the WCC before/after attributes for a directory (or, for
// SETATTR, a file) from the atomic *metadata.DirWcc a mutation returned (H9).
//
// When the store captured the pre/post attributes atomically with the mutation,
// they are used directly — these are guaranteed to bracket the operation. The
// fallbackBefore (a pre-op snapshot the handler read at entry) is used only when
// the atomic Before is absent, and a fresh GetFile supplies the After only when
// the atomic After is absent. handle identifies the WCC subject.
func (h *Handler) dirWccPair(
	ctx *NFSHandlerContext,
	metaSvc *metadata.Service,
	handle metadata.FileHandle,
	wcc *metadata.DirWcc,
	fallbackBefore *types.WccAttr,
) (before *types.WccAttr, after *types.NFSFileAttr) {
	before = fallbackBefore
	if wcc != nil && wcc.Before != nil {
		before = xdr.CaptureWccAttr(wcc.Before)
	}
	if wcc != nil && wcc.After != nil {
		after = h.convertFileAttrToNFS(handle, wcc.After)
		return before, after
	}
	if file, err := metaSvc.GetFile(ctx.Context, handle); err == nil {
		after = h.convertFileAttrToNFS(handle, &file.FileAttr)
	}
	return before, after
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

		logError(ctx.Context, err, operation+" failed: failed to build auth context", "name", filename, "handle", fmt.Sprintf("%x", dirHandleBytes), "client", clientIP)

		wccAfter := h.convertFileAttrToNFS(handle, fileAttr)
		return nil, wccAfter, nil
	}

	return authCtx, nil, nil
}

// getOplockBreaker retrieves the cross-protocol OplockBreaker from the Runtime.
// Returns nil if no adapter has registered an oplock breaker (e.g., SMB not running).
func (h *Handler) getOplockBreaker() adapter.OplockBreaker {
	if h.Registry == nil {
		return nil
	}
	provider := h.Registry.GetAdapterProvider(adapter.OplockBreakerProviderKey)
	if provider == nil {
		return nil
	}
	breaker, ok := provider.(adapter.OplockBreaker)
	if !ok {
		return nil
	}
	return breaker
}

// checkMFsymlinkByHandle checks if a file referenced by handle is an unconverted MFsymlink.
// This is used by READLINK when ReadSymlink fails to check if the file is actually
// an SMB-created MFsymlink that hasn't been converted yet.
func (h *Handler) checkMFsymlinkByHandle(ctx *NFSHandlerContext, fileHandle metadata.FileHandle) MFsymlinkResult {
	metaSvc := h.Registry.GetMetadataService()

	file, err := metaSvc.GetFile(ctx.Context, fileHandle)
	if err != nil {
		return MFsymlinkResult{}
	}

	return checkMFsymlink(ctx.Context, h.Registry, fileHandle, file)
}

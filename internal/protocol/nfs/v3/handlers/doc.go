package handlers

import (
	"fmt"
	"sync"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/xdr"
	"github.com/marmos91/dittofs/pkg/metrics"
	"github.com/marmos91/dittofs/pkg/registry"
	"github.com/marmos91/dittofs/pkg/store/content"
	"github.com/marmos91/dittofs/pkg/store/metadata"
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

	// fileLocks provides per-ContentID mutexes to prevent race conditions
	// when multiple concurrent COMMIT operations target the same file.
	// This prevents session conflicts during incremental uploads.
	// Uses sync.Map for lock-free reads and minimal contention.
	fileLocks sync.Map // map[metadata.ContentID]*sync.Mutex
}

// getMetadataStore retrieves the metadata store for the share specified in the context.
// This helper consolidates the common pattern of:
//  1. Checking if the share exists
//  2. Getting the metadata store for the share
//
// Returns:
//   - metadata.MetadataStore: The metadata store for the share
//   - error: If the share doesn't exist or the store cannot be retrieved
func (h *Handler) getMetadataStore(ctx *NFSHandlerContext) (metadata.MetadataStore, error) {
	// Check if share exists
	if !h.Registry.ShareExists(ctx.Share) {
		return nil, fmt.Errorf("share not found: %s", ctx.Share)
	}

	// Get metadata store for this share
	metadataStore, err := h.Registry.GetMetadataStoreForShare(ctx.Share)
	if err != nil {
		return nil, fmt.Errorf("cannot get metadata store for share %s: %w", ctx.Share, err)
	}

	return metadataStore, nil
}

// getContentStore retrieves the content store for the share specified in the context.
// This helper is used by handlers that need to access file data (READ, WRITE, REMOVE, etc.).
//
// Returns:
//   - content.ContentStore: The content store for the share
//   - error: If the share doesn't exist or the store cannot be retrieved
func (h *Handler) getContentStore(ctx *NFSHandlerContext) (content.ContentStore, error) {
	// Check if share exists
	if !h.Registry.ShareExists(ctx.Share) {
		return nil, fmt.Errorf("share not found: %s", ctx.Share)
	}

	// Get content store for this share
	contentStore, err := h.Registry.GetContentStoreForShare(ctx.Share)
	if err != nil {
		return nil, fmt.Errorf("cannot get content store for share %s: %w", ctx.Share, err)
	}

	return contentStore, nil
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
	metadataStore metadata.MetadataStore,
	fileHandle metadata.FileHandle,
	operationName string,
	handleBytes []byte,
) (*metadata.File, uint32, error) {
	clientIP := xdr.ExtractClientIP(ctx.ClientAddr)

	file, err := metadataStore.GetFile(ctx.Context, fileHandle)
	if err != nil {
		// Check if the error is due to context cancellation
		if ctx.Context.Err() != nil {
			logger.Debug("%s cancelled during file lookup: handle=%x client=%s error=%v",
				operationName, handleBytes, clientIP, ctx.Context.Err())
			return nil, types.NFS3ErrIO, ctx.Context.Err()
		}

		logger.Debug("%s failed: handle not found: handle=%x client=%s error=%v",
			operationName, handleBytes, clientIP, err)
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
			logger.Debug("%s cancelled during auth context building: file='%s' dir=%x client=%s error=%v",
				operation, filename, dirHandleBytes, clientIP, ctx.Context.Err())

			wccAfter := h.convertFileAttrToNFS(handle, fileAttr)
			return nil, wccAfter, ctx.Context.Err()
		}

		logger.Error("%s failed: failed to build auth context: file='%s' dir=%x client=%s error=%v",
			operation, filename, dirHandleBytes, clientIP, err)

		wccAfter := h.convertFileAttrToNFS(handle, fileAttr)
		return nil, wccAfter, nil
	}

	return authCtx, nil, nil
}

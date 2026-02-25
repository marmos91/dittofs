package handlers

import (
	"bytes"
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/payload"
)

// buildV4AuthContext creates an AuthContext for NFSv4 real-FS operations.
//
// It extracts the share name from the file handle, builds an identity from
// the CompoundContext credentials, applies identity mapping rules, resolves
// permissions, and returns the effective AuthContext.
//
// Returns:
//   - *metadata.AuthContext: Auth context with effective (mapped) credentials
//   - string: The share name extracted from the handle
//   - error: If handle decoding, identity mapping, or permission resolution fails
func (h *Handler) buildV4AuthContext(ctx *types.CompoundContext, handle []byte) (*metadata.AuthContext, string, error) {
	// Decode file handle to extract share name
	shareName, _, err := metadata.DecodeFileHandle(metadata.FileHandle(handle))
	if err != nil {
		return nil, "", fmt.Errorf("decode file handle: %w", err)
	}

	// Map auth flavor to auth method string
	authMethod := "anonymous"
	if ctx.AuthFlavor == rpc.AuthUnix {
		authMethod = "unix"
	}

	// Build identity from Unix credentials (before mapping)
	originalIdentity := &metadata.Identity{
		UID:  ctx.UID,
		GID:  ctx.GID,
		GIDs: ctx.GIDs,
	}

	// Set username from UID if available (for logging/auditing)
	if originalIdentity.UID != nil {
		originalIdentity.Username = fmt.Sprintf("uid:%d", *originalIdentity.UID)
	}

	if h.Registry == nil {
		// No registry available -- return a basic auth context
		return &metadata.AuthContext{
			Context:    ctx.Context,
			ClientAddr: ctx.ClientAddr,
			AuthMethod: authMethod,
			Identity:   originalIdentity,
		}, shareName, nil
	}

	// Apply share-level identity mapping (all_squash, root_squash)
	effectiveIdentity, err := h.Registry.ApplyIdentityMapping(shareName, originalIdentity)
	if err != nil {
		logger.Debug("NFSv4 identity mapping failed",
			"share", shareName,
			"error", err,
			"client", ctx.ClientAddr)
		// Fall back to original identity if mapping fails (share may not exist yet)
		effectiveIdentity = originalIdentity
	}

	// Get share for read-only flag
	shareReadOnly := false
	share, shareErr := h.Registry.GetShare(shareName)
	if shareErr == nil && share != nil {
		shareReadOnly = share.ReadOnly
	}

	// Create auth context with the effective (mapped) identity
	authCtx := &metadata.AuthContext{
		Context:       ctx.Context,
		ClientAddr:    ctx.ClientAddr,
		AuthMethod:    authMethod,
		Identity:      effectiveIdentity,
		ShareReadOnly: shareReadOnly,
	}

	return authCtx, shareName, nil
}

// getMetadataServiceForCtx returns the MetadataService from the handler's registry.
// Returns an error if the registry is nil.
func getMetadataServiceForCtx(h *Handler) (*metadata.MetadataService, error) {
	if h.Registry == nil {
		return nil, fmt.Errorf("no registry configured")
	}
	return h.Registry.GetMetadataService(), nil
}

// getPayloadServiceForCtx returns the PayloadService from the handler's registry.
// Returns an error if the registry is nil.
func getPayloadServiceForCtx(h *Handler) (*payload.PayloadService, error) {
	if h.Registry == nil {
		return nil, fmt.Errorf("no registry configured")
	}
	return h.Registry.GetPayloadService(), nil
}

// encodeChangeInfo4 encodes a change_info4 structure into the buffer.
//
// change_info4 is used by CREATE, REMOVE, RENAME, and other operations
// to report directory change information.
//
// Wire format:
//
//	bool    atomic;      (true if before/after are atomic)
//	uint64  changeid_before;
//	uint64  changeid_after;
func encodeChangeInfo4(buf *bytes.Buffer, atomic bool, before, after uint64) {
	_ = xdr.WriteBool(buf, atomic)
	_ = xdr.WriteUint64(buf, before)
	_ = xdr.WriteUint64(buf, after)
}

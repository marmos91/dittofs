package handlers

import (
	"bytes"
	"io"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/attrs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// verifyAttributes is the shared comparison logic for VERIFY and NVERIFY.
//
// It decodes the client-provided fattr4 (bitmap + opaque attr_vals), encodes
// the server's current attributes using the same bitmap, and performs a
// byte-exact comparison of the opaque data portions.
//
// Returns:
//   - match: true if client and server opaque data are identical
//   - status: NFS4_OK if comparison succeeded, or an error status
func verifyAttributes(h *Handler, ctx *types.CompoundContext, reader io.Reader) (match bool, status uint32) {
	// Require CurrentFH
	if s := types.RequireCurrentFH(ctx); s != types.NFS4_OK {
		return false, s
	}

	// Decode client-provided fattr4: bitmap4 + opaque attr_vals
	clientBitmap, err := attrs.DecodeBitmap4(reader)
	if err != nil {
		return false, types.NFS4ERR_BADXDR
	}

	clientAttrData, err := xdr.DecodeOpaque(reader)
	if err != nil {
		return false, types.NFS4ERR_BADXDR
	}

	// Encode server attributes using the client's bitmap (intersected with supported)
	var serverAttrData []byte

	if pseudofs.IsPseudoFSHandle(ctx.CurrentFH) {
		// Pseudo-fs handle
		node, ok := h.PseudoFS.LookupByHandle(ctx.CurrentFH)
		if !ok {
			return false, types.NFS4ERR_STALE
		}

		serverAttrData, err = encodeAttrValsOnly(func(buf *bytes.Buffer, responseBitmap []uint32) error {
			return attrs.EncodePseudoFSAttrs(buf, clientBitmap, node)
		}, clientBitmap)
		if err != nil {
			return false, types.NFS4ERR_SERVERFAULT
		}
	} else {
		// Real-fs handle
		authCtx, _, authErr := h.buildV4AuthContext(ctx, ctx.CurrentFH)
		if authErr != nil {
			return false, types.NFS4ERR_SERVERFAULT
		}

		metaSvc, metaErr := getMetadataServiceForCtx(h)
		if metaErr != nil {
			return false, types.NFS4ERR_SERVERFAULT
		}

		file, getErr := metaSvc.GetFile(authCtx.Context, metadata.FileHandle(ctx.CurrentFH))
		if getErr != nil {
			return false, types.MapMetadataErrorToNFS4(getErr)
		}

		serverAttrData, err = encodeAttrValsOnly(func(buf *bytes.Buffer, responseBitmap []uint32) error {
			return attrs.EncodeRealFileAttrs(buf, clientBitmap, file, metadata.FileHandle(ctx.CurrentFH))
		}, clientBitmap)
		if err != nil {
			return false, types.NFS4ERR_SERVERFAULT
		}
	}

	// Byte-exact comparison of opaque attr_vals
	return bytes.Equal(clientAttrData, serverAttrData), types.NFS4_OK
}

// encodeAttrValsOnly calls an encode function and extracts only the opaque
// attr_vals portion, stripping the bitmap and opaque length prefix.
//
// The encode functions (EncodePseudoFSAttrs, EncodeRealFileAttrs) write:
//
//	bitmap4 (variable) + opaque(attr_vals) = [len:uint32][data:bytes][padding]
//
// We need just the raw attr_vals bytes for comparison. Strategy:
//  1. Encode to a buffer
//  2. Skip the bitmap4 (decode it to advance past it)
//  3. Read the opaque data (which is the attr_vals)
func encodeAttrValsOnly(encodeFn func(buf *bytes.Buffer, responseBitmap []uint32) error, clientBitmap []uint32) ([]byte, error) {
	var buf bytes.Buffer
	if err := encodeFn(&buf, clientBitmap); err != nil {
		return nil, err
	}

	// Parse the encoded output: skip bitmap, read opaque attr_vals
	reader := bytes.NewReader(buf.Bytes())

	// Skip the response bitmap
	if _, err := attrs.DecodeBitmap4(reader); err != nil {
		return nil, err
	}

	// Read the opaque attr_vals
	attrData, err := xdr.DecodeOpaque(reader)
	if err != nil {
		return nil, err
	}

	return attrData, nil
}

// handleVerify implements the VERIFY operation (RFC 7530 Section 16.35).
//
// VERIFY checks whether the server's current attributes match the
// client-provided fattr4. If they match, NFS4_OK is returned and
// the compound continues. If they don't match, NFS4ERR_NOT_SAME is
// returned and the compound stops (standard stop-on-error behavior).
//
// This enables conditional compound sequences like:
//
//	VERIFY(size == X) + SETATTR(mode = 0644)
//
// The SETATTR only executes if the file's size is still X.
//
// Wire format args:
//
//	obj_attributes: fattr4 (bitmap4 + opaque attr_vals)
//
// Wire format res:
//
//	nfsstat4 only (no additional data)
func (h *Handler) handleVerify(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	match, status := verifyAttributes(h, ctx, reader)

	if status != types.NFS4_OK {
		logger.Debug("NFSv4 VERIFY failed",
			"status", status,
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_VERIFY,
			Data:   encodeStatusOnly(status),
		}
	}

	if match {
		logger.Debug("NFSv4 VERIFY: attributes match",
			"client", ctx.ClientAddr)
		return &types.CompoundResult{
			Status: types.NFS4_OK,
			OpCode: types.OP_VERIFY,
			Data:   encodeStatusOnly(types.NFS4_OK),
		}
	}

	logger.Debug("NFSv4 VERIFY: attributes do NOT match",
		"client", ctx.ClientAddr)
	return &types.CompoundResult{
		Status: types.NFS4ERR_NOT_SAME,
		OpCode: types.OP_VERIFY,
		Data:   encodeStatusOnly(types.NFS4ERR_NOT_SAME),
	}
}

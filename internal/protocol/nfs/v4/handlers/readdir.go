package handlers

import (
	"bytes"
	"io"

	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/attrs"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/protocol/xdr"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// handleReadDir implements the READDIR operation (RFC 7530 Section 16.25).
//
// READDIR lists children of a directory, returning entry names, cookies,
// and requested attributes. For pseudo-fs handles, it lists virtual namespace
// children with their pseudo-fs attributes.
//
// Wire format args:
//
//	cookie:       uint64   (entry offset, 0 = start from beginning)
//	cookieverf:   opaque[8] (verifier from previous READDIR, 0 for first call)
//	dircount:     uint32   (max bytes of entry info, hint for server)
//	maxcount:     uint32   (max bytes of entire response)
//	attr_request: bitmap4  (requested attributes per entry)
//
// Wire format res:
//
//	nfsstat4:     uint32
//	cookieverf:   opaque[8] (verifier for this listing)
//	dirlist4:     entries + eof bool
func (h *Handler) handleReadDir(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	// Require current filehandle
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_READDIR,
			Data:   encodeStatusOnly(status),
		}
	}

	// Read cookie (uint64)
	cookie, err := xdr.DecodeUint64(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_READDIR,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// Read cookieverf (8 bytes, as two uint32s since XDR has no raw byte read)
	var cookieVerf [8]byte
	verfBuf := make([]byte, 8)
	if _, err := io.ReadFull(reader, verfBuf); err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_READDIR,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}
	copy(cookieVerf[:], verfBuf)

	// Read dircount (uint32)
	_, err = xdr.DecodeUint32(reader) // dircount (hint, not enforced)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_READDIR,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// Read maxcount (uint32)
	maxcount, err := xdr.DecodeUint32(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_READDIR,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// Read attr_request bitmap
	attrRequest, err := attrs.DecodeBitmap4(reader)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_BADXDR,
			OpCode: types.OP_READDIR,
			Data:   encodeStatusOnly(types.NFS4ERR_BADXDR),
		}
	}

	// Check if current FH is a pseudo-fs handle
	if pseudofs.IsPseudoFSHandle(ctx.CurrentFH) {
		return h.readDirPseudoFS(ctx, cookie, maxcount, attrRequest)
	}

	// Real filesystem handle -- list directory from metadata service
	return h.readDirRealFS(ctx, cookie, maxcount, attrRequest)
}

// readDirRealFS handles READDIR for real filesystem directories.
func (h *Handler) readDirRealFS(ctx *types.CompoundContext, cookie uint64, maxcount uint32, attrRequest []uint32) *types.CompoundResult {
	authCtx, _, err := h.buildV4AuthContext(ctx, ctx.CurrentFH)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_READDIR,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	metaSvc, err := getMetadataServiceForCtx(h)
	if err != nil {
		return &types.CompoundResult{
			Status: types.NFS4ERR_SERVERFAULT,
			OpCode: types.OP_READDIR,
			Data:   encodeStatusOnly(types.NFS4ERR_SERVERFAULT),
		}
	}

	page, err := metaSvc.ReadDirectory(authCtx, metadata.FileHandle(ctx.CurrentFH), cookie, maxcount)
	if err != nil {
		status := types.MapMetadataErrorToNFS4(err)
		return &types.CompoundResult{
			Status: status,
			OpCode: types.OP_READDIR,
			Data:   encodeStatusOnly(status),
		}
	}

	// Build response
	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, types.NFS4_OK)

	// Write cookieverf (8 zero bytes for now -- Phase 9 adds proper verifier)
	buf.Write(make([]byte, 8))

	// Encode directory entries
	encodedSize := uint32(buf.Len())
	for _, entry := range page.Entries {
		// Pre-encode the entry to check size
		var entryBuf bytes.Buffer

		// value_follows = true
		_ = xdr.WriteUint32(&entryBuf, 1)

		// cookie (uint64)
		_ = xdr.WriteUint64(&entryBuf, entry.Cookie)

		// name (XDR string)
		_ = xdr.WriteXDRString(&entryBuf, entry.Name)

		// Encode entry attributes
		if entry.Attr != nil {
			// Extract share name from handle for FSID computation
			entryShareName, _, _ := metadata.DecodeFileHandle(entry.Handle)
			file := &metadata.File{
				ShareName: entryShareName,
				FileAttr:  *entry.Attr,
			}
			_ = attrs.EncodeRealFileAttrs(&entryBuf, attrRequest, file, entry.Handle)
		} else {
			// No attrs available -- encode empty fattr4
			_ = attrs.EncodeBitmap4(&entryBuf, nil)
			_ = xdr.WriteXDROpaque(&entryBuf, nil)
		}

		// Check maxcount limit
		entrySize := uint32(entryBuf.Len())
		if maxcount > 0 && encodedSize+entrySize+4 > maxcount { // +4 for eof bool
			if encodedSize == uint32(12+8) { // status(4) + cookieverf(8)
				return &types.CompoundResult{
					Status: types.NFS4ERR_TOOSMALL,
					OpCode: types.OP_READDIR,
					Data:   encodeStatusOnly(types.NFS4ERR_TOOSMALL),
				}
			}
			break
		}

		buf.Write(entryBuf.Bytes())
		encodedSize += entrySize
	}

	// value_follows = false (no more entries in this batch)
	_ = xdr.WriteUint32(&buf, 0)

	// eof
	if !page.HasMore {
		_ = xdr.WriteUint32(&buf, 1) // true: no more entries
	} else {
		_ = xdr.WriteUint32(&buf, 0) // false: more entries available
	}

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_READDIR,
		Data:   buf.Bytes(),
	}
}

// readDirPseudoFS handles READDIR for pseudo-fs directories.
func (h *Handler) readDirPseudoFS(ctx *types.CompoundContext, cookie uint64, maxcount uint32, attrRequest []uint32) *types.CompoundResult {
	// Find the node by handle
	node, ok := h.PseudoFS.LookupByHandle(ctx.CurrentFH)
	if !ok {
		return &types.CompoundResult{
			Status: types.NFS4ERR_STALE,
			OpCode: types.OP_READDIR,
			Data:   encodeStatusOnly(types.NFS4ERR_STALE),
		}
	}

	// List children (sorted by name)
	children := h.PseudoFS.ListChildren(node)

	// Build response
	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, types.NFS4_OK)

	// Write cookieverf (8 zero bytes for pseudo-fs -- doesn't need verification)
	buf.Write(make([]byte, 8))

	// Encode directory entries
	// Per RFC 7530, dirlist4 is a linked list:
	//   entry4* entries;  (bool has_next + entry data, repeated)
	//   bool    eof;
	//
	// entry4: cookie (uint64) + name (XDR string) + attrs (fattr4)
	encodedSize := uint32(buf.Len())
	for i, child := range children {
		// Cookie is child index + 1 (0 means "start from beginning")
		entryCookie := uint64(i + 1)

		// Skip entries with cookie <= requested cookie
		if entryCookie <= cookie {
			continue
		}

		// Pre-encode the entry to check size
		var entryBuf bytes.Buffer

		// value_follows = true (there IS an entry)
		_ = xdr.WriteUint32(&entryBuf, 1)

		// cookie (uint64)
		_ = xdr.WriteUint64(&entryBuf, entryCookie)

		// name (XDR string)
		_ = xdr.WriteXDRString(&entryBuf, child.Name)

		// attrs (fattr4)
		_ = attrs.EncodePseudoFSAttrs(&entryBuf, attrRequest, child)

		// Check maxcount limit (approximate: include overhead for remaining entries)
		entrySize := uint32(entryBuf.Len())
		if maxcount > 0 && encodedSize+entrySize+4 > maxcount { // +4 for eof bool
			// Would exceed maxcount; if no entries encoded yet, return NFS4ERR_TOOSMALL
			if encodedSize == uint32(12+8) { // status(4) + cookieverf(8)
				return &types.CompoundResult{
					Status: types.NFS4ERR_TOOSMALL,
					OpCode: types.OP_READDIR,
					Data:   encodeStatusOnly(types.NFS4ERR_TOOSMALL),
				}
			}
			// Stop encoding entries; not EOF since more entries exist
			break
		}

		buf.Write(entryBuf.Bytes())
		encodedSize += entrySize
	}

	// value_follows = false (no more entries)
	_ = xdr.WriteUint32(&buf, 0)

	// eof = true (pseudo-fs directories are always fully enumerable)
	_ = xdr.WriteUint32(&buf, 1)

	return &types.CompoundResult{
		Status: types.NFS4_OK,
		OpCode: types.OP_READDIR,
		Data:   buf.Bytes(),
	}
}

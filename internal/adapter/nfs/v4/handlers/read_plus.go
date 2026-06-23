package handlers

import (
	"bytes"
	"errors"
	"io"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	xdr "github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// errNoRegistry signals READ_PLUS could not resolve a block store because no
// runtime registry is configured (mirrors the READ handler's nil-Registry guard).
var errNoRegistry = errors.New("no registry configured")

// READ_PLUS4args / READ_PLUS4res (RFC 7862 Section 15.10):
//
//	struct READ_PLUS4args {
//	    stateid4 rpa_stateid;
//	    offset4  rpa_offset;
//	    count4   rpa_count;
//	};
//	union read_plus_content switch (data_content4 rpc_content) {
//	 case NFS4_CONTENT_DATA: data4   rpc_data;   // offset4 + opaque<>
//	 case NFS4_CONTENT_HOLE: data_info4 rpc_hole; // offset4 + length4
//	};
//	struct read_plus_res4 {
//	    bool                rpr_eof;
//	    read_plus_content   rpr_contents<>;
//	};
//	union READ_PLUS4res switch (nfsstat4 rp_status) {
//	 case NFS4_OK: read_plus_res4 rp_resok4;
//	 default:      void;
//	};
//
// READ_PLUS is READ that may report holes compactly: the reply is an array of
// data segments and NFS4_CONTENT_HOLE runs. Bytes-on-wire for a sparse file are
// less than its logical size because hole runs carry only (offset, length).
//
// DittoFS derives the segmentation from the file's content-addressed block list
// (block.Segments). The always-correct fallback — a dense file or a file with
// no tracked holes — yields a single data segment, matching plain READ.
func (h *Handler) handleReadPlus(ctx *types.CompoundContext, reader io.Reader) *types.CompoundResult {
	if status := types.RequireCurrentFH(ctx); status != types.NFS4_OK {
		return readPlusErr(status)
	}
	if pseudofs.IsPseudoFSHandle(ctx.CurrentFH) {
		return readPlusErr(types.NFS4ERR_ISDIR)
	}

	stateid, err := types.DecodeStateid4(reader)
	if err != nil {
		return readPlusErr(types.NFS4ERR_BADXDR)
	}
	offset, err := xdr.DecodeUint64(reader)
	if err != nil {
		return readPlusErr(types.NFS4ERR_BADXDR)
	}
	count, err := xdr.DecodeUint32(reader)
	if err != nil {
		return readPlusErr(types.NFS4ERR_BADXDR)
	}

	// READ_PLUS shares READ's stateid semantics: special stateids are allowed,
	// a real open stateid must carry READ access.
	if openState, stateErr := h.StateManager.ValidateStateid(stateid, ctx.CurrentFH, state.StateidOpRead); stateErr != nil {
		st := mapStateError(stateErr)
		logger.Debug("NFSv4.2 READ_PLUS stateid validation failed", "error", stateErr, "nfs_status", st, "client", ctx.ClientAddr)
		return readPlusErr(st)
	} else if openState != nil && openState.ShareAccess&types.OPEN4_SHARE_ACCESS_READ == 0 {
		return readPlusErr(types.NFS4ERR_OPENMODE)
	}

	authCtx, _, err := h.buildV4AuthContext(ctx, ctx.CurrentFH)
	if err != nil {
		return readPlusErr(nfs4StatusForAuthError(err))
	}
	metaSvc, err := getMetadataServiceForCtx(h)
	if err != nil {
		return readPlusErr(types.NFS4ERR_SERVERFAULT)
	}

	file, err := metaSvc.GetFile(authCtx.Context, metadata.FileHandle(ctx.CurrentFH))
	if err != nil {
		return readPlusErr(common.MapToNFS4(err))
	}
	if file.Type != metadata.FileTypeRegular {
		return readPlusErr(types.NFS4ERR_ISDIR)
	}

	// Empty file or read entirely past EOF: an empty content array with EOF set.
	if file.Size == 0 || offset >= file.Size {
		return encodeReadPlusResok(true, nil)
	}

	// Clamp the read window to EOF.
	readEnd := offset + uint64(count)
	if readEnd > file.Size || readEnd < offset /* overflow */ {
		readEnd = file.Size
	}

	contents, err := h.buildReadPlusContents(ctx, file, offset, readEnd)
	if err != nil {
		logger.Debug("NFSv4.2 READ_PLUS content build failed", "error", err, "client", ctx.ClientAddr)
		// A missing registry is a server misconfiguration, not an I/O fault —
		// mirror READ's nil-Registry guard (NFS4ERR_SERVERFAULT). All other
		// failures are block-store read errors → NFS4ERR_IO.
		if errors.Is(err, errNoRegistry) {
			return readPlusErr(types.NFS4ERR_SERVERFAULT)
		}
		return readPlusErr(types.NFS4ERR_IO)
	}
	eof := readEnd >= file.Size

	logger.Debug("NFSv4.2 READ_PLUS", "offset", offset, "count", count,
		"end", readEnd, "segments", len(contents), "eof", eof, "client", ctx.ClientAddr)

	return encodeReadPlusResok(eof, contents)
}

// readPlusContent is one encoded read_plus_content union member: either a data
// segment (Hole=false, Data carries the bytes) or a hole run (Hole=true).
type readPlusContent struct {
	Hole   bool
	Offset uint64
	Length uint64 // hole length (Hole=true only)
	Data   []byte // data bytes (Hole=false only)
}

// buildReadPlusContents segments [offset, readEnd) of the file into data and
// hole runs using the shared hole map, reading the data runs from the block
// store. Each segment is clipped to the requested window. Returned data byte
// slices are copies (the pooled read buffers are released here), so the caller
// may retain them past encoding.
func (h *Handler) buildReadPlusContents(ctx *types.CompoundContext, file *metadata.File, offset, readEnd uint64) ([]readPlusContent, error) {
	// No backing payload: the whole window is a single hole (reads as zeros).
	if file.PayloadID == "" {
		return []readPlusContent{{Hole: true, Offset: offset, Length: readEnd - offset}}, nil
	}

	segs := block.Segments(file.Blocks, file.Size)
	var contents []readPlusContent
	var blockStore *engine.Store

	for _, seg := range segs {
		// Intersect the segment with the requested window.
		segStart := seg.Start
		if segStart < offset {
			segStart = offset
		}
		segEnd := seg.End
		if segEnd > readEnd {
			segEnd = readEnd
		}
		if segStart >= segEnd {
			continue
		}

		if seg.Kind == block.SegmentHole {
			contents = append(contents, readPlusContent{Hole: true, Offset: segStart, Length: segEnd - segStart})
			continue
		}

		// Data segment: read the bytes from the block store (lazily resolved on
		// the first data run so an all-hole window touches no block store).
		if blockStore == nil {
			if h.Registry == nil {
				return nil, errNoRegistry
			}
			bs, err := common.ResolveForRead(ctx.Context, h.Registry, metadata.FileHandle(ctx.CurrentFH))
			if err != nil {
				return nil, err
			}
			blockStore = bs
		}
		res, err := common.ReadFromBlockStore(ctx.Context, blockStore, file.PayloadID, segStart, uint32(segEnd-segStart))
		if err != nil {
			return nil, err
		}
		// Copy out of the pooled buffer so it can be released immediately; the
		// content array outlives this loop and is encoded later.
		data := append([]byte(nil), res.Data...)
		res.Release()
		contents = append(contents, readPlusContent{Hole: false, Offset: segStart, Data: data})
	}
	return contents, nil
}

// encodeReadPlusResok encodes a successful READ_PLUS reply: status, eof, and the
// content array (each member tagged NFS4_CONTENT_DATA or NFS4_CONTENT_HOLE).
func encodeReadPlusResok(eof bool, contents []readPlusContent) *types.CompoundResult {
	var buf bytes.Buffer
	_ = xdr.WriteUint32(&buf, types.NFS4_OK)
	_ = xdr.WriteBool(&buf, eof)
	_ = xdr.WriteUint32(&buf, uint32(len(contents)))
	for i := range contents {
		c := &contents[i]
		if c.Hole {
			_ = xdr.WriteUint32(&buf, types.NFS4_CONTENT_HOLE)
			_ = xdr.WriteUint64(&buf, c.Offset)
			_ = xdr.WriteUint64(&buf, c.Length)
			continue
		}
		_ = xdr.WriteUint32(&buf, types.NFS4_CONTENT_DATA)
		_ = xdr.WriteUint64(&buf, c.Offset)
		_ = xdr.WriteXDROpaque(&buf, c.Data)
	}
	return &types.CompoundResult{Status: types.NFS4_OK, OpCode: types.OP_READ_PLUS, Data: buf.Bytes()}
}

// readPlusErr builds a READ_PLUS error result (status only).
func readPlusErr(status uint32) *types.CompoundResult {
	return &types.CompoundResult{
		Status: status,
		OpCode: types.OP_READ_PLUS,
		Data:   encodeStatusOnly(status),
	}
}

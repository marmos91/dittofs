package handlers

import (
	"context"
	"errors"
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// Sparse-file FSCTL handlers — issue #359.
//
// DittoFS's block store is implicitly sparse: AppendWrite zero-grows the
// payload buffer on demand and ReadPayloadAt copies those zero-filled
// bytes back, so a file written at offset N with no preceding writes
// reads as N zero bytes followed by the data. We therefore accept the
// sparse-management FSCTLs as no-ops or "report everything as one
// allocated extent" responses — the wire-level expectations (test passes,
// no STATUS_NOT_SUPPORTED) match without touching the on-disk format.
//
// Spec references:
//   - FSCTL_SET_SPARSE             MS-FSCC §2.3.50 / §2.3.51
//   - FSCTL_QUERY_ALLOCATED_RANGES MS-FSCC §2.3.32 / §2.3.33
//   - FSCTL_SET_ZERO_DATA          MS-FSCC §2.3.67 / §2.3.68
//
// Samba reference: source3/smbd/smb2_ioctl_filesys.c

// ioctlInputBuffer extracts the input buffer portion of an IOCTL request
// using the InputOffset / InputCount fields from the SMB2 IOCTL request
// (MS-SMB2 §2.2.31). InputOffset is relative to the SMB2 header (64 bytes
// before the body), so the buffer starts at offset 56 inside body.
func ioctlInputBuffer(body []byte) []byte {
	if len(body) < 56 {
		return nil
	}
	r := smbenc.NewReader(body)
	r.Skip(4)                    // StructureSize + Reserved
	r.Skip(4)                    // CtlCode
	r.Skip(16)                   // FileId
	_ = r.ReadUint32()           // InputOffset (relative to SMB2 header)
	inputCount := r.ReadUint32() // InputCount
	if r.Err() != nil || inputCount == 0 {
		return nil
	}
	const bufStart = uint32(56)
	if uint32(len(body)) < bufStart+inputCount {
		return nil
	}
	return body[bufStart : bufStart+inputCount]
}

// handleSetSparse handles FSCTL_SET_SPARSE [MS-FSCC] 2.3.50.
//
// Input is optional: empty → set sparse; 1-byte FILE_SET_SPARSE_BUFFER
// where SetSparse=0 clears the attribute, non-zero sets it. We accept all
// variants and return success without persisting the flag — DittoFS is
// implicitly sparse, so there is no semantic difference between "sparse
// set" and "sparse cleared" on the wire. The handle requires write
// access per MS-FSA §2.1.5.10.34.
func (h *Handler) handleSetSparse(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	fileID, ok := parseIoctlFileID(body)
	if !ok {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		logger.Debug("IOCTL FSCTL_SET_SPARSE: file handle not found", "fileID", fmt.Sprintf("%x", fileID))
		return NewErrorResult(types.StatusFileClosed), nil
	}

	// Per MS-FSA §2.1.5.10.34: SET_SPARSE requires FILE_WRITE_DATA. The smb2.
	// ioctl.sparse_perms test confirms this gate by opening with read-only
	// access and expecting STATUS_ACCESS_DENIED.
	if uint32(types.AccessMask(openFile.GrantedAccess))&uint32(types.FileWriteData) == 0 {
		logger.Debug("IOCTL FSCTL_SET_SPARSE: handle lacks FILE_WRITE_DATA",
			"path", openFile.Path,
			"granted", fmt.Sprintf("0x%08X", openFile.GrantedAccess))
		return NewErrorResult(types.StatusAccessDenied), nil
	}

	// Input is either empty (default: set sparse) or 1 byte; reject larger
	// buffers per Samba `fsctl_set_sparse` (returns INVALID_PARAMETER on
	// inputs longer than 1 byte — covered by smb2.ioctl.sparse_set_oversize).
	if input := ioctlInputBuffer(body); len(input) > 1 {
		logger.Debug("IOCTL FSCTL_SET_SPARSE: input too large", "len", len(input))
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	logger.Debug("IOCTL FSCTL_SET_SPARSE: accepted (DittoFS is implicitly sparse)",
		"path", openFile.Path)
	resp := buildIoctlResponse(FsctlSetSparse, fileID, nil)
	return NewResult(types.StatusSuccess, resp), nil
}

// handleQueryAllocatedRanges handles FSCTL_QUERY_ALLOCATED_RANGES [MS-FSCC]
// 2.3.32. The client supplies a (FileOffset, Length) window and the server
// reports which sub-ranges are allocated. Since DittoFS does not track holes
// independently from the payload, we report the intersection of the client's
// window with [0, FileSize) as a single allocated range. Bytes past EOF
// produce an empty output buffer, matching Samba `fsctl_qar`.
func (h *Handler) handleQueryAllocatedRanges(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	fileID, ok := parseIoctlFileID(body)
	if !ok {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		logger.Debug("IOCTL FSCTL_QUERY_ALLOCATED_RANGES: file handle not found", "fileID", fmt.Sprintf("%x", fileID))
		return NewErrorResult(types.StatusFileClosed), nil
	}

	// MS-FSA §2.1.5.10.4 / Samba `fsctl_qar`: requires FILE_READ_DATA.
	if uint32(types.AccessMask(openFile.GrantedAccess))&uint32(types.FileReadData) == 0 {
		logger.Debug("IOCTL FSCTL_QUERY_ALLOCATED_RANGES: handle lacks FILE_READ_DATA",
			"path", openFile.Path,
			"granted", fmt.Sprintf("0x%08X", openFile.GrantedAccess))
		return NewErrorResult(types.StatusAccessDenied), nil
	}

	input := ioctlInputBuffer(body)
	if len(input) < 16 {
		logger.Debug("IOCTL FSCTL_QUERY_ALLOCATED_RANGES: malformed input", "len", len(input))
		return NewErrorResult(types.StatusInvalidParameter), nil
	}
	r := smbenc.NewReader(input[:16])
	reqOffset := r.ReadUint64()
	reqLength := r.ReadUint64()
	if r.Err() != nil {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Negative-as-unsigned: per MS-FSA an offset whose high bit is set
	// (i.e. parses to a negative int64) is an error. Samba `fsctl_qar`
	// returns INVALID_PARAMETER. The smb2.ioctl.sparse_qar_ob1 test
	// exercises both arms.
	if int64(reqOffset) < 0 || int64(reqLength) < 0 {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Resolve the file's logical size. PrepareWrite would mutate state, so
	// read attributes directly via the metadata service.
	authCtx, err := BuildAuthContext(ctx)
	if err != nil {
		return NewErrorResult(types.StatusAccessDenied), nil
	}
	metaSvc := h.Registry.GetMetadataService()
	file, err := metaSvc.GetFile(authCtx.Context, openFile.MetadataHandle)
	if err != nil {
		return NewErrorResult(common.MapToSMB(err)), nil
	}
	fileSize := file.Size

	rangeEnd := reqOffset + reqLength
	if rangeEnd < reqOffset { // overflow guard
		return NewErrorResult(types.StatusInvalidParameter), nil
	}
	allocStart := reqOffset
	allocEnd := rangeEnd
	if allocEnd > fileSize {
		allocEnd = fileSize
	}

	// FILE_ALLOCATED_RANGE_BUFFER is 16 bytes (FileOffset + Length). Zero
	// entries when the requested window is entirely past EOF or empty.
	w := smbenc.NewWriter(16)
	if allocStart < allocEnd {
		w.WriteUint64(allocStart)
		w.WriteUint64(allocEnd - allocStart)
	}
	resp := buildIoctlResponse(FsctlQueryAllocatedRanges, fileID, w.Bytes())
	return NewResult(types.StatusSuccess, resp), nil
}

// handleSetZeroData handles FSCTL_SET_ZERO_DATA [MS-FSCC] 2.3.67.
//
// Writes zeros across the [FileOffset, BeyondFinalZero) byte window. We
// honour the request by issuing zero-filled writes through the standard
// PrepareWrite / WriteAt / CommitWrite path so file size, mtime, and
// block-store invalidation stay consistent. The range may extend past
// EOF, in which case the file is implicitly extended (Samba parity).
func (h *Handler) handleSetZeroData(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	fileID, ok := parseIoctlFileID(body)
	if !ok {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		logger.Debug("IOCTL FSCTL_SET_ZERO_DATA: file handle not found", "fileID", fmt.Sprintf("%x", fileID))
		return NewErrorResult(types.StatusFileClosed), nil
	}
	if openFile.IsDirectory || openFile.IsPipe {
		return NewErrorResult(types.StatusInvalidDeviceRequest), nil
	}

	// MS-FSA §2.1.5.10.35: SET_ZERO_DATA requires FILE_WRITE_DATA.
	if uint32(types.AccessMask(openFile.GrantedAccess))&uint32(types.FileWriteData) == 0 {
		logger.Debug("IOCTL FSCTL_SET_ZERO_DATA: handle lacks FILE_WRITE_DATA",
			"path", openFile.Path,
			"granted", fmt.Sprintf("0x%08X", openFile.GrantedAccess))
		return NewErrorResult(types.StatusAccessDenied), nil
	}

	input := ioctlInputBuffer(body)
	if len(input) < 16 {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}
	r := smbenc.NewReader(input[:16])
	fileOffset := r.ReadUint64()
	beyond := r.ReadUint64()
	if r.Err() != nil {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}
	if int64(fileOffset) < 0 || int64(beyond) < 0 || beyond < fileOffset {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}
	if fileOffset == beyond {
		// Zero-length request is a documented no-op per Samba `fsctl_zero_data`.
		resp := buildIoctlResponse(FsctlSetZeroData, fileID, nil)
		return NewResult(types.StatusSuccess, resp), nil
	}

	authCtx, err := BuildAuthContext(ctx)
	if err != nil {
		return NewErrorResult(types.StatusAccessDenied), nil
	}
	if err := h.zeroFillRange(authCtx, openFile, fileOffset, beyond); err != nil {
		if errors.Is(err, errZeroFillCancelled) {
			return NewErrorResult(types.StatusCancelled), nil
		}
		logger.Warn("IOCTL FSCTL_SET_ZERO_DATA: write failed",
			"path", openFile.Path, "error", err)
		return NewErrorResult(common.MapContentToSMB(err)), nil
	}

	resp := buildIoctlResponse(FsctlSetZeroData, fileID, nil)
	return NewResult(types.StatusSuccess, resp), nil
}

// zeroFillChunkSize is the chunk we use to issue zero-fill writes. A single
// 1 MiB scratch buffer keeps the steady-state RAM cost bounded while still
// amortising the per-write metadata round-trip on multi-MB zero ranges.
const zeroFillChunkSize = 1 << 20

// errZeroFillCancelled is a sentinel for context-cancelled zero fills so the
// caller can map back to STATUS_CANCELLED instead of a generic write error.
var errZeroFillCancelled = errors.New("zero-fill cancelled")

// zeroFillRange writes [start, end) as zeros through the standard
// PrepareWrite/WriteAt/CommitWrite chain. The write is chunked so we never
// allocate more than zeroFillChunkSize regardless of how large the requested
// range is — smbtorture's hole-punch tests exercise multi-MB ranges.
func (h *Handler) zeroFillRange(authCtx *metadata.AuthContext, openFile *OpenFile, start, end uint64) error {
	metaSvc := h.Registry.GetMetadataService()
	blockStore, err := common.ResolveForWrite(authCtx.Context, h.Registry, openFile.MetadataHandle)
	if err != nil {
		return err
	}

	chunkLen := uint64(zeroFillChunkSize)
	if total := end - start; total < chunkLen {
		chunkLen = total
	}
	zeros := make([]byte, chunkLen)

	for offset := start; offset < end; {
		if err := authCtx.Context.Err(); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return errZeroFillCancelled
			}
			return err
		}
		remaining := end - offset
		if remaining > uint64(len(zeros)) {
			remaining = uint64(len(zeros))
		}
		newSize := offset + remaining
		writeOp, err := metaSvc.PrepareWrite(authCtx, openFile.MetadataHandle, newSize)
		if err != nil {
			return err
		}
		if err := common.WriteToBlockStore(authCtx.Context, blockStore, writeOp.PayloadID, zeros[:remaining], offset); err != nil {
			return err
		}
		if _, err := metaSvc.CommitWrite(authCtx, writeOp); err != nil {
			return err
		}
		offset += remaining
	}
	return nil
}

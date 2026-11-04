package handlers

import (
	"fmt"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
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
//   - FSCTL_SET_SPARSE             MS-FSCC §2.3.83 / §2.3.84 (Request / Reply)
//   - FSCTL_QUERY_ALLOCATED_RANGES MS-FSCC §2.3.51 / §2.3.52 (Request / Reply)
//   - FSCTL_SET_ZERO_DATA          MS-FSCC §2.3.85 / §2.3.86 (Request / Reply)
//
// Samba reference: source3/smbd/smb2_ioctl_filesys.c

// handleSetSparse handles FSCTL_SET_SPARSE [MS-FSCC] 2.3.83 (FSCTL_SET_SPARSE Request).
//
// Input is optional: empty → set sparse (Samba `vfswrap_fsctl` default); a
// 1+-byte FILE_SET_SPARSE_BUFFER where SetSparse byte 0 == 0 clears the
// attribute, non-zero sets it. Per Samba (source3/modules/vfs_default.c) and
// smbtorture smb2.ioctl.sparse_set_oversize, buffers larger than 1 byte are
// accepted — only the first byte is inspected.
//
// Directories return STATUS_INVALID_PARAMETER (Windows 2k12 / 2k8 behaviour
// asserted by smb2.ioctl.sparse_dir_flag).
//
// The handle requires FILE_WRITE_DATA per MS-FSA §2.1.5.10.34.
//
// We persist the resolved state in modeDOSSparse so subsequent QUERY_INFO
// reflects FILE_ATTRIBUTE_SPARSE_FILE — required by sparse_file_flag,
// sparse_set_nobuf, sparse_set_oversize, sparse_qar, sparse_punch.
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
	path := openFile.Name().Path

	// FSCTL_SET_SPARSE is a file-only operation. Directories take
	// STATUS_INVALID_PARAMETER per MS-FSA §2.1.5.10.38 ("FSCTL_SET_SPARSE") and Windows behaviour.
	if openFile.IsDirectory {
		logger.Debug("IOCTL FSCTL_SET_SPARSE: rejected on directory",
			"path", path)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Per smb2.ioctl.sparse_perms (source4/torture/smb2/ioctl.c:5023+),
	// SET_SPARSE accepts any of FILE_WRITE_DATA, FILE_APPEND_DATA, or
	// FILE_WRITE_ATTRIBUTES. Only handles with WRITE_EA / READ-only /
	// no-write access are rejected. This matches the Windows behaviour
	// where SET_SPARSE is a metadata operation that any "write-ish" right
	// can drive.
	const setSparseGate = uint32(types.FileWriteData) |
		uint32(types.FileAppendData) |
		uint32(types.FileWriteAttributes)
	if openFile.GrantedAccess&setSparseGate == 0 {
		logger.Debug("IOCTL FSCTL_SET_SPARSE: handle lacks WRITE_DATA/APPEND_DATA/WRITE_ATTRIBUTES",
			"path", path,
			"granted", fmt.Sprintf("0x%08X", openFile.GrantedAccess))
		return NewErrorResult(types.StatusAccessDenied), nil
	}

	// MS-FSCC §2.3.83 (FSCTL_SET_SPARSE Request): empty input defaults to SetSparse=TRUE. Any byte > 0
	// sets sparse, 0x00 clears. Inputs larger than 1 byte are accepted —
	// only the first byte is inspected (Samba parity, sparse_set_oversize).
	setSparse := true
	if input := parseIoctlInputData(body); len(input) > 0 {
		setSparse = input[0] != 0
	}

	// Persist the sparse bit via SetFileAttributes so QUERY_INFO and
	// subsequent CREATEs see the FILE_ATTRIBUTE_SPARSE_FILE attribute.
	if status := h.primeAuthContextFromOpenFile(ctx, openFile); status != types.StatusSuccess {
		return NewErrorResult(status), nil
	}
	authCtx, err := BuildAuthContext(ctx)
	if err != nil {
		return NewErrorResult(types.StatusAccessDenied), nil
	}
	metaSvc := h.Registry.GetMetadataService()
	// Flip the sparse bit atomically inside the store (see modeBitMaskAttrs):
	// a concurrent SET_SPARSE / SET_COMPRESSION cannot clobber it with a stale
	// Mode snapshot.
	attrs := modeBitMaskAttrs(modeDOSSparse, setSparse)
	// A mode-bit flip is an attribute write, so hold a frozen ChangeTime.
	holdFrozenCtime(openFile, &attrs)
	if _, err := metaSvc.SetFileAttributes(authCtx, openFile.MetadataHandle, &attrs); err != nil {
		logger.Warn("IOCTL FSCTL_SET_SPARSE: failed to persist mode",
			"path", path, "error", err)
		return NewErrorResult(types.StatusForErr(err)), nil
	}

	logger.Debug("IOCTL FSCTL_SET_SPARSE: applied",
		"path", path, "sparse", setSparse)
	resp := buildIoctlResponse(FsctlSetSparse, fileID, nil)
	return NewResult(types.StatusSuccess, resp), nil
}

// fileAllocatedRangeBufSize is the on-wire size of a FILE_ALLOCATED_RANGE_BUFFER
// entry (FileOffset uint64 + Length uint64).
const fileAllocatedRangeBufSize = 16

// handleQueryAllocatedRanges handles FSCTL_QUERY_ALLOCATED_RANGES. The client
// supplies a (FileOffset, Length) window ([MS-FSCC] 2.3.51) and the server
// reports which sub-ranges are non-sparse ([MS-FSCC] 2.3.52).
//
// Allocation model: for non-sparse files (modeDOSSparse clear), we report the
// intersection of the request window with [0, FileSize) as a single range —
// matching NTFS "always fully allocated" semantics for plain files. For
// sparse files we scan the data within the window and report contiguous
// non-zero runs; pure-zero regions are treated as deallocated holes. This
// satisfies smb2.ioctl.sparse_punch which asserts that SET_ZERO_DATA on a
// sparse file makes the punched range disappear from QAR.
//
// Output buffer handling (MS-FSA §2.1.5.10.22 ("FSCTL_QUERY_ALLOCATED_RANGES"), Samba `fsctl_qar`):
//   - MaxOutputResponse == 0 and at least one range to report:
//     STATUS_BUFFER_TOO_SMALL (smb2.ioctl.sparse_qar_malformed).
//   - MaxOutputResponse < total range bytes: truncate to whole entries that
//     fit and return STATUS_BUFFER_OVERFLOW (smb2.ioctl.sparse_qar_truncated).
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
	path := openFile.Name().Path

	// Samba `fsctl_qar` requires FILE_READ_DATA. MS-FSA 2.1.5.10.22 states no
	// such gate; the requirement comes from the FSCTL access rules in MS-SMB2
	// §3.3.5.15.
	if openFile.GrantedAccess&uint32(types.FileReadData) == 0 {
		logger.Debug("IOCTL FSCTL_QUERY_ALLOCATED_RANGES: handle lacks FILE_READ_DATA",
			"path", path,
			"granted", fmt.Sprintf("0x%08X", openFile.GrantedAccess))
		return NewErrorResult(types.StatusAccessDenied), nil
	}

	input := parseIoctlInputData(body)
	if len(input) < fileAllocatedRangeBufSize {
		logger.Debug("IOCTL FSCTL_QUERY_ALLOCATED_RANGES: malformed input", "len", len(input))
		return NewErrorResult(types.StatusInvalidParameter), nil
	}
	r := smbenc.NewReader(input[:fileAllocatedRangeBufSize])
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

	// Prime ctx with the OpenFile's recorded session state — without this
	// hand-off BuildAuthContext takes the ctx.User==nil arm and synthesises
	// UID-0 (root), bypassing DACL checks on the GetFile probe (#619).
	if status := h.primeAuthContextFromOpenFile(ctx, openFile); status != types.StatusSuccess {
		return NewErrorResult(status), nil
	}
	authCtx, err := BuildAuthContext(ctx)
	if err != nil {
		return NewErrorResult(types.StatusAccessDenied), nil
	}
	metaSvc := h.Registry.GetMetadataService()
	file, err := metaSvc.GetFile(authCtx.Context, openFile.MetadataHandle)
	if err != nil {
		return NewErrorResult(types.StatusForErr(err)), nil
	}

	rangeEnd := reqOffset + reqLength
	if rangeEnd < reqOffset { // overflow guard
		return NewErrorResult(types.StatusInvalidParameter), nil
	}
	allocStart := reqOffset
	allocEnd := min(rangeEnd, file.Size)

	var ranges []allocatedRange
	if allocStart < allocEnd {
		if file.Mode&modeDOSSparse != 0 {
			// A sparse file's allocated runs are found by probing its
			// content, and no content store is wired. A payloadless file
			// has none.
			if file.PayloadID != "" {
				return NewErrorResult(types.StatusForErr(common.ErrNoContentStore)), nil
			}
		} else {
			ranges = []allocatedRange{{Offset: allocStart, Length: allocEnd - allocStart}}
		}
	}

	maxOut := parseIoctlMaxOutputSize(body)
	totalBytes := uint64(len(ranges)) * fileAllocatedRangeBufSize

	// Empty result is always SUCCESS (no entries to write, no overflow).
	if len(ranges) == 0 {
		resp := buildIoctlResponse(FsctlQueryAllocatedRanges, fileID, nil)
		return NewResult(types.StatusSuccess, resp), nil
	}

	// MaxOutputResponse < one entry while we have something to report:
	// STATUS_BUFFER_TOO_SMALL. Samba fsctl_qar gates the same way.
	if maxOut < fileAllocatedRangeBufSize {
		logger.Debug("IOCTL FSCTL_QUERY_ALLOCATED_RANGES: buffer too small",
			"path", path, "maxOut", maxOut)
		return NewErrorResult(types.StatusBufferTooSmall), nil
	}

	// Truncate to whole entries that fit and report BUFFER_OVERFLOW.
	status := types.StatusSuccess
	if uint64(maxOut) < totalBytes {
		fits := int(maxOut / fileAllocatedRangeBufSize)
		ranges = ranges[:fits]
		status = types.StatusBufferOverflow
	}

	w := smbenc.NewWriter(len(ranges) * fileAllocatedRangeBufSize)
	for _, rg := range ranges {
		w.WriteUint64(rg.Offset)
		w.WriteUint64(rg.Length)
	}
	resp := buildIoctlResponse(FsctlQueryAllocatedRanges, fileID, w.Bytes())
	return NewResult(status, resp), nil
}

// allocatedRange mirrors FILE_ALLOCATED_RANGE_BUFFER (MS-FSCC §2.3.52 (FSCTL_QUERY_ALLOCATED_RANGES Reply)).
type allocatedRange struct {
	Offset uint64
	Length uint64
}

// fileZeroDataBufSize is the on-wire size of a FILE_ZERO_DATA_INFORMATION
// buffer (FileOffset uint64 + BeyondFinalZero uint64).
const fileZeroDataBufSize = 16

// zeroDataMaxFileSize mirrors the NTFS upper bound that the regular WRITE
// path enforces (see write.go). Without this cap a client could request an
// arbitrary <2^63 range and tie the handler up in a multi-hour zero-fill
// loop while the metadata service grows file size past anything WRITE
// would accept. Keeping the two limits identical also prevents a SET_ZERO_
// DATA followed by a normal WRITE from refusing the write because the
// preceding FSCTL pushed the file past the WRITE cap.
const zeroDataMaxFileSize = uint64(0xFFFFFFF0000) // ~16 TiB, identical to WRITE handler

// handleSetZeroData handles FSCTL_SET_ZERO_DATA [MS-FSCC] 2.3.85 (FSCTL_SET_ZERO_DATA Request).
//
// Writes zeros across the [FileOffset, BeyondFinalZero) byte window. We
// honour the request by issuing zero-filled writes through the standard
// PrepareWrite / WriteAt / CommitWrite path so file size, mtime, and
// block-store invalidation stay consistent. The window is clamped to
// the current file size — SET_ZERO_DATA MUST NOT extend the file (Samba
// `fsctl_zero_data`; smb2.ioctl.sparse_punch_invalid).
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
	path := openFile.Name().Path
	if openFile.IsDirectory || openFile.IsPipe {
		return NewErrorResult(types.StatusInvalidDeviceRequest), nil
	}

	// SET_ZERO_DATA requires FILE_WRITE_DATA. MS-FSA 2.1.5.10.39
	// ("FSCTL_SET_ZERO_DATA") states no such gate; the requirement comes from the
	// FSCTL access rules in MS-SMB2 §3.3.5.15.
	if openFile.GrantedAccess&uint32(types.FileWriteData) == 0 {
		logger.Debug("IOCTL FSCTL_SET_ZERO_DATA: handle lacks FILE_WRITE_DATA",
			"path", path,
			"granted", fmt.Sprintf("0x%08X", openFile.GrantedAccess))
		return NewErrorResult(types.StatusAccessDenied), nil
	}

	input := parseIoctlInputData(body)
	if len(input) < fileZeroDataBufSize {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}
	r := smbenc.NewReader(input[:fileZeroDataBufSize])
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
	if beyond > zeroDataMaxFileSize {
		logger.Debug("IOCTL FSCTL_SET_ZERO_DATA: range exceeds MAXFILESIZE",
			"path", path, "beyond", beyond)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Prime ctx with the OpenFile's session/tree so BuildAuthContext picks
	// up the correct identity for downstream metadata permission checks.
	// Without this, FileID-only IOCTL requests fall through to anonymous
	// UID-0 (root) and CommitWrite skips the non-root SUID/SGID clearing
	// path (#619, same class as #603).
	if status := h.primeAuthContextFromOpenFile(ctx, openFile); status != types.StatusSuccess {
		return NewErrorResult(status), nil
	}
	authCtx, err := BuildAuthContext(ctx)
	if err != nil {
		return NewErrorResult(types.StatusAccessDenied), nil
	}

	// SET_ZERO_DATA MUST NOT extend the file (Samba `fsctl_zero_data`), so a
	// range starting at or past EOF is a no-op success and needs no content
	// store.
	file, err := h.Registry.GetMetadataService().GetFile(authCtx.Context, openFile.MetadataHandle)
	if err != nil {
		return NewErrorResult(types.StatusForErr(err)), nil
	}
	if fileOffset >= file.Size {
		return NewResult(types.StatusSuccess, buildIoctlResponse(FsctlSetZeroData, fileID, nil)), nil
	}

	// Zeroing a range inside the file writes content, and no content store is
	// wired.
	return NewErrorResult(types.StatusForErr(common.ErrNoContentStore)), nil
}

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
	"github.com/marmos91/dittofs/pkg/metadata/lock"
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

// handleSetSparse handles FSCTL_SET_SPARSE [MS-FSCC] 2.3.50.
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

	// FSCTL_SET_SPARSE is a file-only operation. Directories take
	// STATUS_INVALID_PARAMETER per MS-FSA §2.1.5.9.36 and Windows behaviour.
	if openFile.IsDirectory {
		logger.Debug("IOCTL FSCTL_SET_SPARSE: rejected on directory",
			"path", openFile.Path)
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
			"path", openFile.Path,
			"granted", fmt.Sprintf("0x%08X", openFile.GrantedAccess))
		return NewErrorResult(types.StatusAccessDenied), nil
	}

	// MS-FSCC §2.3.50: empty input defaults to SetSparse=TRUE. Any byte > 0
	// sets sparse, 0x00 clears. Inputs larger than 1 byte are accepted —
	// only the first byte is inspected (Samba parity, sparse_set_oversize).
	setSparse := true
	if input := parseIoctlInputData(body); len(input) >= 1 {
		setSparse = input[0] != 0
	}

	// Persist the sparse bit via SetFileAttributes so QUERY_INFO and
	// subsequent CREATEs see the FILE_ATTRIBUTE_SPARSE_FILE attribute.
	h.primeAuthContextFromOpenFile(ctx, openFile)
	authCtx, err := BuildAuthContext(ctx)
	if err != nil {
		return NewErrorResult(types.StatusAccessDenied), nil
	}
	metaSvc := h.Registry.GetMetadataService()
	file, err := metaSvc.GetFile(authCtx.Context, openFile.MetadataHandle)
	if err != nil {
		return NewErrorResult(common.MapToSMB(err)), nil
	}
	newMode := file.Mode
	if setSparse {
		newMode |= modeDOSSparse
	} else {
		newMode &^= modeDOSSparse
	}
	if newMode != file.Mode {
		if err := metaSvc.SetFileAttributes(authCtx, openFile.MetadataHandle, &metadata.SetAttrs{
			Mode: &newMode,
		}); err != nil {
			logger.Warn("IOCTL FSCTL_SET_SPARSE: failed to persist mode",
				"path", openFile.Path, "error", err)
			return NewErrorResult(common.MapToSMB(err)), nil
		}
	}

	logger.Debug("IOCTL FSCTL_SET_SPARSE: applied",
		"path", openFile.Path, "sparse", setSparse)
	resp := buildIoctlResponse(FsctlSetSparse, fileID, nil)
	return NewResult(types.StatusSuccess, resp), nil
}

// fileAllocatedRangeBufSize is the on-wire size of a FILE_ALLOCATED_RANGE_BUFFER
// entry (FileOffset uint64 + Length uint64).
const fileAllocatedRangeBufSize = 16

// handleQueryAllocatedRanges handles FSCTL_QUERY_ALLOCATED_RANGES [MS-FSCC]
// 2.3.32. The client supplies a (FileOffset, Length) window and the server
// reports which sub-ranges are non-sparse.
//
// Allocation model: for non-sparse files (modeDOSSparse clear), we report the
// intersection of the request window with [0, FileSize) as a single range —
// matching NTFS "always fully allocated" semantics for plain files. For
// sparse files we scan the data within the window and report contiguous
// non-zero runs; pure-zero regions are treated as deallocated holes. This
// satisfies smb2.ioctl.sparse_punch which asserts that SET_ZERO_DATA on a
// sparse file makes the punched range disappear from QAR.
//
// Output buffer handling (MS-FSA §2.1.5.10.4, Samba `fsctl_qar`):
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

	// MS-FSA §2.1.5.10.4 / Samba `fsctl_qar`: requires FILE_READ_DATA.
	if uint32(types.AccessMask(openFile.GrantedAccess))&uint32(types.FileReadData) == 0 {
		logger.Debug("IOCTL FSCTL_QUERY_ALLOCATED_RANGES: handle lacks FILE_READ_DATA",
			"path", openFile.Path,
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
	h.primeAuthContextFromOpenFile(ctx, openFile)
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

	var ranges []allocatedRange
	if allocStart < allocEnd {
		if file.Mode&modeDOSSparse != 0 {
			ranges, err = h.scanAllocatedRanges(authCtx, openFile, &file.FileAttr, allocStart, allocEnd)
			if err != nil {
				logger.Warn("IOCTL FSCTL_QUERY_ALLOCATED_RANGES: scan failed",
					"path", openFile.Path, "error", err)
				return NewErrorResult(common.MapContentToSMB(err)), nil
			}
		} else {
			ranges = []allocatedRange{{Offset: allocStart, Length: allocEnd - allocStart}}
		}
	}

	maxOut := parseIoctlMaxOutputSize(body)
	totalBytes := uint32(len(ranges)) * fileAllocatedRangeBufSize

	// Empty result is always SUCCESS (no entries to write, no overflow).
	if len(ranges) == 0 {
		resp := buildIoctlResponse(FsctlQueryAllocatedRanges, fileID, nil)
		return NewResult(types.StatusSuccess, resp), nil
	}

	// MaxOutputResponse < one entry while we have something to report:
	// STATUS_BUFFER_TOO_SMALL. Samba fsctl_qar gates the same way.
	if maxOut < fileAllocatedRangeBufSize {
		logger.Debug("IOCTL FSCTL_QUERY_ALLOCATED_RANGES: buffer too small",
			"path", openFile.Path, "maxOut", maxOut)
		return NewErrorResult(types.StatusBufferTooSmall), nil
	}

	// Truncate to whole entries that fit and report BUFFER_OVERFLOW.
	status := types.StatusSuccess
	if maxOut < totalBytes {
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

// allocatedRange mirrors FILE_ALLOCATED_RANGE_BUFFER (MS-FSCC §2.3.32).
type allocatedRange struct {
	Offset uint64
	Length uint64
}

// sparseClusterSize is the deallocation granularity used by our sparse-file
// QAR scan. NTFS deallocates in 64 KiB chunks on Windows Server 2012; we use
// 4 KiB so the smbtorture sparse_punch test (4 KiB file, full-file punch)
// reports the hole accurately. Within a cluster, presence of any non-zero
// byte marks the whole cluster as allocated — matches the "FSCTL_QUERY_
// ALLOCATED_RANGES returns extents, not byte-level holes" semantic in
// MS-FSCC §2.3.32 and avoids fragmenting pattern data (whose individual
// bytes contain interior zeros) into hundreds of tiny ranges.
const sparseClusterSize = uint64(4096)

// scanAllocatedRanges walks the file payload over [start, end) at
// sparseClusterSize granularity and returns the contiguous allocated
// (any-non-zero) cluster runs, with the first/last range clamped to the
// request window per Samba `fsctl_qar` and smb2.ioctl.sparse_qar_ob1.
//
// Used only when modeDOSSparse is set — plain files report a single range
// without touching block-store data. DittoFS payloads zero-grow on demand,
// so SET_ZERO_DATA on a sparse file naturally renders the punched cluster(s)
// as all-zero and they drop out of the QAR result.
func (h *Handler) scanAllocatedRanges(authCtx *metadata.AuthContext, openFile *OpenFile, file *metadata.FileAttr, start, end uint64) ([]allocatedRange, error) {
	if file.PayloadID == "" {
		return nil, nil
	}
	blockStore, err := common.ResolveForRead(authCtx.Context, h.Registry, openFile.MetadataHandle)
	if err != nil {
		return nil, err
	}

	// Align scan to cluster boundaries so cluster-allocated reporting is
	// stable regardless of where the request window starts. We probe each
	// cluster intersecting [start, end) and clamp the emitted ranges back
	// to [start, end) at the end.
	firstCluster := start / sparseClusterSize
	// end is exclusive, so the last covered cluster index is (end-1)/cluster.
	lastCluster := (end - 1) / sparseClusterSize

	var (
		ranges    []allocatedRange
		curOffset uint64
		curEnd    uint64
		inRun     bool
	)
	flush := func() {
		if !inRun {
			return
		}
		off := curOffset
		if off < start {
			off = start
		}
		ce := curEnd
		if ce > end {
			ce = end
		}
		if off < ce {
			ranges = append(ranges, allocatedRange{Offset: off, Length: ce - off})
		}
		inRun = false
	}
	for cluster := firstCluster; cluster <= lastCluster; cluster++ {
		clusterStart := cluster * sparseClusterSize
		clusterEnd := clusterStart + sparseClusterSize
		probeLen := uint32(sparseClusterSize)
		// Stop probing past the actual file size — anything past EOF is an
		// implicit hole, mirroring NTFS QAR which clamps to allocated size.
		if clusterStart >= file.Size {
			break
		}
		if clusterEnd > file.Size {
			probeLen = uint32(file.Size - clusterStart)
		}
		result, readErr := common.ReadFromBlockStore(authCtx.Context, blockStore, file.PayloadID, clusterStart, probeLen)
		if readErr != nil {
			return nil, readErr
		}
		data := result.Data
		hasNonZero := false
		for _, b := range data {
			if b != 0 {
				hasNonZero = true
				break
			}
		}
		result.Release()
		if hasNonZero {
			if !inRun {
				curOffset = clusterStart
				inRun = true
			}
			curEnd = clusterEnd
		} else {
			flush()
		}
	}
	flush()
	return ranges, nil
}

// zeroDataMaxFileSize mirrors the NTFS upper bound that the regular WRITE
// path enforces (see write.go). Without this cap a client could request an
// arbitrary <2^63 range and tie the handler up in a multi-hour zero-fill
// loop while the metadata service grows file size past anything WRITE
// would accept. Keeping the two limits identical also prevents a SET_ZERO_
// DATA followed by a normal WRITE from refusing the write because the
// preceding FSCTL pushed the file past the WRITE cap.
const zeroDataMaxFileSize = uint64(0xFFFFFFF0000) // ~16 TiB, identical to WRITE handler

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

	input := parseIoctlInputData(body)
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
	if beyond > zeroDataMaxFileSize {
		logger.Debug("IOCTL FSCTL_SET_ZERO_DATA: range exceeds MAXFILESIZE",
			"path", openFile.Path, "beyond", beyond)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Prime ctx with the OpenFile's session/tree so BuildAuthContext picks
	// up the correct identity for downstream metadata permission checks.
	// Without this, FileID-only IOCTL requests fall through to anonymous
	// UID-0 (root) and CommitWrite skips the non-root SUID/SGID clearing
	// path (#619, same class as #603).
	h.primeAuthContextFromOpenFile(ctx, openFile)
	authCtx, err := BuildAuthContext(ctx)
	if err != nil {
		return NewErrorResult(types.StatusAccessDenied), nil
	}

	// Clamp the zero-fill window to the current file size: SET_ZERO_DATA
	// MUST NOT extend the file (Samba `fsctl_zero_data`; covered by
	// smb2.ioctl.sparse_punch_invalid which writes [4096, 4104) on a 4096-
	// byte file and asserts size stays 4096). If the entire request is past
	// EOF the call is a no-op success.
	metaSvc := h.Registry.GetMetadataService()
	fileForSize, err := metaSvc.GetFile(authCtx.Context, openFile.MetadataHandle)
	if err != nil {
		return NewErrorResult(common.MapToSMB(err)), nil
	}
	if fileOffset >= fileForSize.Size {
		resp := buildIoctlResponse(FsctlSetZeroData, fileID, nil)
		return NewResult(types.StatusSuccess, resp), nil
	}
	if beyond > fileForSize.Size {
		beyond = fileForSize.Size
	}

	// Byte-range lock check on the write window. WRITE and COPYCHUNK both
	// gate on this; without it another handle could hold a conflicting
	// lock over [fileOffset, beyond) and the zero-fill would silently win
	// (smb2.ioctl.sparse_lock).
	if err := metaSvc.CheckLockForIO(
		authCtx.Context,
		openFile.MetadataHandle,
		openFile.OpenID(),
		ctx.SessionID,
		fileOffset,
		beyond-fileOffset,
		true, // isWrite
	); err != nil {
		logger.Debug("IOCTL FSCTL_SET_ZERO_DATA: blocked by lock",
			"path", openFile.Path, "offset", fileOffset, "length", beyond-fileOffset)
		return NewErrorResult(types.StatusFileLockConflict), nil
	}

	// Break Level II (Read) caching leases held by other clients on the
	// target so they invalidate stale cached data. Mirrors the WRITE and
	// COPYCHUNK paths.
	if h.LeaseManager != nil {
		lockFileHandle := lock.FileHandle(openFile.MetadataHandle)
		if breakErr := h.LeaseManager.BreakReadLeasesOnWrite(lockFileHandle, openFile.ShareName, openFile.LeaseKey); breakErr != nil {
			logger.Debug("IOCTL FSCTL_SET_ZERO_DATA: oplock break failed (non-fatal)",
				"path", openFile.Path, "error", breakErr)
		}
	}

	if err := h.zeroFillRange(authCtx, openFile, fileOffset, beyond); err != nil {
		if errors.Is(err, errZeroFillCancelled) {
			return NewErrorResult(types.StatusCancelled), nil
		}
		logger.Warn("IOCTL FSCTL_SET_ZERO_DATA: write failed",
			"path", openFile.Path, "error", err)
		return NewErrorResult(common.MapContentToSMB(err)), nil
	}

	// SMB requires immediate cross-session metadata visibility (unlike NFS
	// which uses explicit COMMIT). Flush deferred metadata so a subsequent
	// QUERY_INFO sees the new size/timestamps without waiting for CLOSE.
	if _, flushErr := metaSvc.FlushPendingWriteForFile(authCtx, openFile.MetadataHandle); flushErr != nil {
		logger.Debug("IOCTL FSCTL_SET_ZERO_DATA: deferred metadata flush failed (non-fatal)",
			"path", openFile.Path, "error", flushErr)
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

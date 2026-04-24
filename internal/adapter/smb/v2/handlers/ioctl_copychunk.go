package handlers

import (
	"crypto/rand"
	"fmt"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// Server-side copy limits per [MS-SMB2] 3.3.5.15.6.
const (
	copyChunkMaxChunks   = 256
	copyChunkMaxChunkLen = 1048576  // 1 MiB
	copyChunkMaxTotalLen = 16777216 // 16 MiB
	copyChunkResponseLen = 12
	resumeKeyLen         = 24
)

// NT status codes not yet defined in types package.
const (
	statusInvalidViewSize types.Status = 0xC000001F
)

// resumeKeyStore maps opaque 24-byte resume keys to the FileID they were issued for.
// Keys are per-handler (i.e., per-server) and valid as long as the source file remains open.
// A client must call FSCTL_SRV_REQUEST_RESUME_KEY to obtain a key before using COPYCHUNK.
type resumeKeyStore struct {
	mu   sync.RWMutex
	keys map[[resumeKeyLen]byte][16]byte // resumeKey -> FileID
}

func newResumeKeyStore() *resumeKeyStore {
	return &resumeKeyStore{
		keys: make(map[[resumeKeyLen]byte][16]byte),
	}
}

// issue generates a cryptographically random 24-byte resume key, stores the
// mapping to the given FileID, and returns the key.
func (s *resumeKeyStore) issue(fileID [16]byte) ([resumeKeyLen]byte, error) {
	var key [resumeKeyLen]byte
	if _, err := rand.Read(key[:]); err != nil {
		return key, fmt.Errorf("generate resume key: %w", err)
	}
	s.mu.Lock()
	s.keys[key] = fileID
	s.mu.Unlock()
	return key, nil
}

// resolve looks up the FileID associated with a resume key.
// Returns false if the key was never issued.
func (s *resumeKeyStore) resolve(key [resumeKeyLen]byte) ([16]byte, bool) {
	s.mu.RLock()
	fileID, ok := s.keys[key]
	s.mu.RUnlock()
	return fileID, ok
}

// revoke removes all resume keys that map to the given FileID.
// Called when a file handle is closed to prevent stale key usage.
func (s *resumeKeyStore) revoke(fileID [16]byte) {
	s.mu.Lock()
	for k, v := range s.keys {
		if v == fileID {
			delete(s.keys, k)
		}
	}
	s.mu.Unlock()
}

// handleSrvRequestResumeKey handles FSCTL_SRV_REQUEST_RESUME_KEY [MS-SMB2] 2.2.32.3.
//
// Returns a 24-byte opaque resume key that identifies the source file handle
// for subsequent FSCTL_SRV_COPYCHUNK requests. The resume key is a server-generated
// random token (not the raw FileID) to prevent information leakage.
//
// Per [MS-SMB2] 3.3.5.15.6: the source handle must have FILE_READ_DATA or
// FILE_EXECUTE access; otherwise the key is not issued.
//
// Wire format of SRV_REQUEST_RESUME_KEY_RESPONSE (32 bytes):
//
//	ResumeKey(24) + ContextLength(4) + Context(4)
func (h *Handler) handleSrvRequestResumeKey(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	fileID, ok := parseIoctlFileID(body)
	if !ok {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		return NewErrorResult(types.StatusFileClosed), nil
	}

	// Note: No access check here. Per MS-SMB2 3.3.5.15.6, access validation
	// (FILE_READ_DATA or FILE_EXECUTE on source) is performed during the
	// FSCTL_SRV_COPYCHUNK operation, not at resume key issuance time.

	logger.Debug("IOCTL FSCTL_SRV_REQUEST_RESUME_KEY",
		"path", openFile.Path,
		"fileID", fmt.Sprintf("%x", fileID))

	// Generate an opaque resume key and store the mapping
	resumeKey, err := h.resumeKeys.issue(fileID)
	if err != nil {
		logger.Warn("IOCTL FSCTL_SRV_REQUEST_RESUME_KEY: key generation failed", "error", err)
		return NewErrorResult(types.StatusInternalError), nil
	}

	// Build SRV_REQUEST_RESUME_KEY_RESPONSE (32 bytes):
	// ResumeKey(24) + ContextLength(4) + Context(4)
	w := smbenc.NewWriter(32)
	w.WriteBytes(resumeKey[:]) // 24 bytes
	w.WriteUint32(0)           // ContextLength
	w.WriteUint32(0)           // Context

	resp := buildIoctlResponse(FsctlSrvRequestResumeKey, fileID, w.Bytes())
	return NewResult(types.StatusSuccess, resp), nil
}

// handleSrvCopyChunk handles FSCTL_SRV_COPYCHUNK and FSCTL_SRV_COPYCHUNK_WRITE
// [MS-SMB2] 2.2.31.1, 2.2.32.1, 3.3.5.15.6.
//
// Performs server-side data copy between files using resume key + chunk descriptors.
// The IOCTL is sent to the destination file handle; the source is identified by
// the resume key obtained from FSCTL_SRV_REQUEST_RESUME_KEY.
//
// Wire format of SRV_COPYCHUNK_COPY input:
//
//	SourceKey(24) + ChunkCount(4) + Reserved(4) + Chunks[N]
//
// Each SRV_COPYCHUNK chunk (24 bytes):
//
//	SourceOffset(8) + TargetOffset(8) + Length(4) + Reserved(4)
//
// Wire format of SRV_COPYCHUNK_RESPONSE output (12 bytes):
//
//	ChunksWritten(4) + ChunkBytesWritten(4) + TotalBytesWritten(4)
func (h *Handler) handleSrvCopyChunk(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	ctlCode := readCtlCode(body)

	// Parse destination FileID
	dstFileID, ok := parseIoctlFileID(body)
	if !ok {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	dstOpen, ok := h.GetOpenFile(dstFileID)
	if !ok {
		return NewErrorResult(types.StatusFileClosed), nil
	}

	// Parse MaxOutputResponse — must fit SRV_COPYCHUNK_RESPONSE (12 bytes)
	maxOutput := parseIoctlMaxOutputSize(body)
	if maxOutput < copyChunkResponseLen {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Parse input buffer: SRV_COPYCHUNK_COPY
	inputData := parseIoctlInputData(body)
	if len(inputData) < 32 {
		logger.Debug("COPYCHUNK: input too small", "len", len(inputData))
		return copyChunkLimitsResponse(ctlCode, dstFileID), nil
	}

	// Extract SourceKey (24 bytes) and ChunkCount (4 bytes)
	var sourceKey [resumeKeyLen]byte
	copy(sourceKey[:], inputData[0:resumeKeyLen])

	r := smbenc.NewReader(inputData[24:])
	chunkCount := r.ReadUint32()
	_ = r.ReadUint32() // Reserved
	if r.Err() != nil {
		return copyChunkLimitsResponse(ctlCode, dstFileID), nil
	}

	logger.Debug("IOCTL FSCTL_SRV_COPYCHUNK",
		"ctlCode", fmt.Sprintf("0x%08X", ctlCode),
		"dstPath", dstOpen.Path,
		"chunkCount", chunkCount)

	// Resolve source file from resume key via the opaque key store
	srcFileID, ok := h.resumeKeys.resolve(sourceKey)
	if !ok {
		logger.Debug("COPYCHUNK: invalid resume key (not found in store)")
		return NewErrorResult(types.StatusObjectNameNotFound), nil
	}

	srcOpen, ok := h.GetOpenFile(srcFileID)
	if !ok {
		logger.Debug("COPYCHUNK: source file closed since resume key was issued")
		return NewErrorResult(types.StatusObjectNameNotFound), nil
	}

	// Per [MS-SMB2] 3.3.5.15.6: source and destination must be in the same session
	if srcOpen.SessionID != dstOpen.SessionID {
		logger.Debug("COPYCHUNK: cross-session copy not allowed",
			"srcSession", srcOpen.SessionID, "dstSession", dstOpen.SessionID)
		return NewErrorResult(types.StatusObjectNameNotFound), nil
	}

	// Validate that neither source nor destination is a directory or pipe
	if srcOpen.IsDirectory || srcOpen.IsPipe {
		logger.Debug("COPYCHUNK: source is directory or pipe", "path", srcOpen.Path)
		return NewErrorResult(types.StatusInvalidDeviceRequest), nil
	}
	if dstOpen.IsDirectory || dstOpen.IsPipe {
		logger.Debug("COPYCHUNK: destination is directory or pipe", "path", dstOpen.Path)
		return NewErrorResult(types.StatusInvalidDeviceRequest), nil
	}

	// Access checks per [MS-SMB2] 3.3.5.15.6
	if status := validateCopyChunkAccess(ctlCode, srcOpen, dstOpen); status != types.StatusSuccess {
		return NewErrorResult(status), nil
	}

	// Validate chunk count before allocation to prevent DoS via huge ChunkCount.
	if chunkCount > copyChunkMaxChunks {
		return copyChunkLimitsResponse(ctlCode, dstFileID), nil
	}

	// Parse and validate chunks
	chunks, err := parseCopyChunks(inputData[32:], chunkCount)
	if err != nil || !validateCopyChunkLimits(chunks, chunkCount) {
		return copyChunkLimitsResponse(ctlCode, dstFileID), nil
	}

	// Zero-chunk request: return success with zero counts (no data to copy)
	if len(chunks) == 0 {
		return copyChunkSuccessResponse(ctlCode, dstFileID, 0, 0), nil
	}

	// Execute the copy
	return h.executeCopyChunks(ctx, ctlCode, dstFileID, srcOpen, dstOpen, chunks)
}

// validateCopyChunkAccess checks access permissions per [MS-SMB2] 3.3.5.15.6.
//
// Source requires FILE_READ_DATA or FILE_EXECUTE.
// Destination requires FILE_WRITE_DATA or FILE_APPEND_DATA.
// For FSCTL_SRV_COPYCHUNK (not COPYCHUNK_WRITE), destination also requires FILE_READ_DATA.
func validateCopyChunkAccess(ctlCode uint32, src, dst *OpenFile) types.Status {
	srcAccess := types.AccessMask(src.DesiredAccess)
	dstAccess := types.AccessMask(dst.DesiredAccess)

	// Source must have read or execute
	if srcAccess&types.FileReadData == 0 && srcAccess&types.FileExecute == 0 {
		logger.Debug("COPYCHUNK: source lacks read/execute access",
			"path", src.Path, "access", fmt.Sprintf("0x%08X", src.DesiredAccess))
		return types.StatusAccessDenied
	}

	// Destination must have write or append
	if dstAccess&types.FileWriteData == 0 && dstAccess&types.FileAppendData == 0 {
		logger.Debug("COPYCHUNK: destination lacks write access",
			"path", dst.Path, "access", fmt.Sprintf("0x%08X", dst.DesiredAccess))
		return types.StatusAccessDenied
	}

	// FSCTL_SRV_COPYCHUNK (not _WRITE) also requires read on destination
	if ctlCode == FsctlSrvCopyChunk && dstAccess&types.FileReadData == 0 {
		logger.Debug("COPYCHUNK: destination lacks read access (required for non-WRITE variant)",
			"path", dst.Path, "access", fmt.Sprintf("0x%08X", dst.DesiredAccess))
		return types.StatusAccessDenied
	}

	return types.StatusSuccess
}

// copyChunk represents a single chunk descriptor from SRV_COPYCHUNK_COPY.
type copyChunk struct {
	SourceOffset uint64
	TargetOffset uint64
	Length       uint32
}

// parseCopyChunks parses the chunk array from the SRV_COPYCHUNK_COPY input.
// Each chunk is 24 bytes: SourceOffset(8) + TargetOffset(8) + Length(4) + Reserved(4).
func parseCopyChunks(data []byte, count uint32) ([]copyChunk, error) {
	// count is pre-validated to <= copyChunkMaxChunks (256), so int conversion is safe.
	n := int(count)
	needed := n * 24
	if len(data) < needed {
		return nil, fmt.Errorf("insufficient data for %d chunks: have %d, need %d", count, len(data), needed)
	}

	chunks := make([]copyChunk, n)
	r := smbenc.NewReader(data)
	for i := uint32(0); i < count; i++ {
		chunks[i].SourceOffset = r.ReadUint64()
		chunks[i].TargetOffset = r.ReadUint64()
		chunks[i].Length = r.ReadUint32()
		_ = r.ReadUint32() // Reserved
	}
	if r.Err() != nil {
		return nil, fmt.Errorf("error parsing chunks: %w", r.Err())
	}
	return chunks, nil
}

// validateCopyChunkLimits checks that chunk count, sizes, and total are within server limits.
// Per [MS-SMB2] 3.3.5.15.6.2: zero-length chunks are invalid.
func validateCopyChunkLimits(chunks []copyChunk, count uint32) bool {
	if count > copyChunkMaxChunks {
		return false
	}

	var totalBytes uint64
	for _, c := range chunks {
		if c.Length == 0 || c.Length > copyChunkMaxChunkLen {
			return false
		}
		totalBytes += uint64(c.Length)
	}
	return totalBytes <= copyChunkMaxTotalLen
}

// copyChunkLimitsResponse returns STATUS_INVALID_PARAMETER with the server's
// copy chunk limits encoded in the SRV_COPYCHUNK_RESPONSE body.
// Per [MS-SMB2] 3.3.5.15.6.2: on limit violations, the response fields
// contain the server's max values (not the actual chunks written).
func copyChunkLimitsResponse(ctlCode uint32, fileID [16]byte) *HandlerResult {
	w := smbenc.NewWriter(copyChunkResponseLen)
	w.WriteUint32(copyChunkMaxChunks)   // ChunksWritten = server max chunks
	w.WriteUint32(copyChunkMaxChunkLen) // ChunkBytesWritten = server max chunk size
	w.WriteUint32(copyChunkMaxTotalLen) // TotalBytesWritten = server max total bytes

	resp := buildIoctlResponse(ctlCode, fileID, w.Bytes())
	return NewResult(types.StatusInvalidParameter, resp)
}

// copyChunkSuccessResponse builds a SRV_COPYCHUNK_RESPONSE with STATUS_SUCCESS.
func copyChunkSuccessResponse(ctlCode uint32, fileID [16]byte, chunksWritten uint32, totalBytesWritten uint64) *HandlerResult {
	w := smbenc.NewWriter(copyChunkResponseLen)
	w.WriteUint32(chunksWritten)
	w.WriteUint32(0) // ChunkBytesWritten (0 on full success)
	w.WriteUint32(uint32(totalBytesWritten))

	resp := buildIoctlResponse(ctlCode, fileID, w.Bytes())
	return NewResult(types.StatusSuccess, resp)
}

// executeCopyChunks performs the actual data copy chunk by chunk.
// It reads from the source block store and writes to the destination block store,
// supporting cross-share copies (different block stores for source and dest).
func (h *Handler) executeCopyChunks(
	ctx *SMBHandlerContext,
	ctlCode uint32,
	dstFileID [16]byte,
	srcOpen, dstOpen *OpenFile,
	chunks []copyChunk,
) (*HandlerResult, error) {
	metaSvc := h.Registry.GetMetadataService()

	// Get source block store and file metadata
	srcBlockStore, err := h.Registry.GetBlockStoreForHandle(ctx.Context, srcOpen.MetadataHandle)
	if err != nil {
		logger.Warn("COPYCHUNK: source block store unavailable", "path", srcOpen.Path, "error", err)
		return NewErrorResult(types.StatusInternalError), nil
	}

	srcFile, err := metaSvc.GetFile(ctx.Context, srcOpen.MetadataHandle)
	if err != nil {
		logger.Debug("COPYCHUNK: failed to get source file", "path", srcOpen.Path, "error", err)
		return NewErrorResult(common.MapToSMB(err)), nil
	}

	// Get destination block store
	dstBlockStore, err := h.Registry.GetBlockStoreForHandle(ctx.Context, dstOpen.MetadataHandle)
	if err != nil {
		logger.Warn("COPYCHUNK: destination block store unavailable", "path", dstOpen.Path, "error", err)
		return NewErrorResult(types.StatusInternalError), nil
	}

	authCtx, err := BuildAuthContext(ctx)
	if err != nil {
		logger.Warn("COPYCHUNK: failed to build auth context", "error", err)
		return NewErrorResult(types.StatusAccessDenied), nil
	}

	srcPayloadID := string(srcFile.PayloadID)
	srcSize := srcFile.Size

	// Per MS-SMB2 3.3.5.16: Break Read caching leases held by other clients on
	// the destination before writing, so they invalidate their cached data.
	if h.LeaseManager != nil {
		lockFileHandle := lock.FileHandle(dstOpen.MetadataHandle)
		if breakErr := h.LeaseManager.BreakReadLeasesOnWrite(lockFileHandle, dstOpen.ShareName, dstOpen.LeaseKey); breakErr != nil {
			logger.Debug("COPYCHUNK: oplock break failed (non-fatal)", "path", dstOpen.Path, "error", breakErr)
		}
	}

	// Pre-allocate a single buffer sized to the largest chunk to reduce GC pressure.
	var maxChunkLen uint32
	for _, c := range chunks {
		if c.Length > maxChunkLen {
			maxChunkLen = c.Length
		}
	}
	buf := make([]byte, maxChunkLen)

	var chunksWritten uint32
	var totalBytesWritten uint64
	var lastWritePayloadID metadata.PayloadID

	for i, chunk := range chunks {
		// Validate source range: SourceOffset + Length must not exceed source file size
		if chunk.SourceOffset+uint64(chunk.Length) > srcSize {
			logger.Debug("COPYCHUNK: source range exceeds file size",
				"chunk", i, "srcOff", chunk.SourceOffset,
				"len", chunk.Length, "srcSize", srcSize)
			return copyChunkPartialResponse(ctlCode, dstFileID,
				statusInvalidViewSize, chunksWritten, totalBytesWritten), nil
		}

		// Check byte-range lock on source (read operation)
		if lockErr := metaSvc.CheckLockForIO(
			ctx.Context, srcOpen.MetadataHandle, srcOpen.OpenID(),
			srcOpen.SessionID, chunk.SourceOffset, uint64(chunk.Length), false,
		); lockErr != nil {
			logger.Debug("COPYCHUNK: source locked", "chunk", i, "error", lockErr)
			return copyChunkPartialResponse(ctlCode, dstFileID,
				types.StatusFileLockConflict, chunksWritten, totalBytesWritten), nil
		}

		// Check byte-range lock on destination (write operation)
		if lockErr := metaSvc.CheckLockForIO(
			ctx.Context, dstOpen.MetadataHandle, dstOpen.OpenID(),
			dstOpen.SessionID, chunk.TargetOffset, uint64(chunk.Length), true,
		); lockErr != nil {
			logger.Debug("COPYCHUNK: destination locked", "chunk", i, "error", lockErr)
			return copyChunkPartialResponse(ctlCode, dstFileID,
				types.StatusFileLockConflict, chunksWritten, totalBytesWritten), nil
		}

		// Read from source using pre-allocated buffer
		data := buf[:chunk.Length]
		n, err := srcBlockStore.ReadAt(ctx.Context, srcPayloadID, data, chunk.SourceOffset)
		if err != nil {
			logger.Warn("COPYCHUNK: source read failed",
				"chunk", i, "srcPath", srcOpen.Path, "error", err)
			return copyChunkPartialResponse(ctlCode, dstFileID,
				types.StatusInternalError, chunksWritten, totalBytesWritten), nil
		}

		// Reject short reads (TOCTOU: source may have been truncated concurrently)
		if uint32(n) < chunk.Length {
			logger.Debug("COPYCHUNK: short read from source (possible concurrent truncation)",
				"chunk", i, "expected", chunk.Length, "got", n)
			return copyChunkPartialResponse(ctlCode, dstFileID,
				statusInvalidViewSize, chunksWritten, totalBytesWritten), nil
		}
		data = data[:n]

		// Prepare write on destination (validates permissions, updates metadata).
		// Use the declared chunk length for newSize, not the read result,
		// to ensure consistent metadata even under concurrent source modifications.
		newSize := chunk.TargetOffset + uint64(chunk.Length)
		writeOp, err := metaSvc.PrepareWrite(authCtx, dstOpen.MetadataHandle, newSize)
		if err != nil {
			logger.Warn("COPYCHUNK: prepare write failed",
				"chunk", i, "dstPath", dstOpen.Path, "error", err)
			return copyChunkPartialResponse(ctlCode, dstFileID,
				common.MapToSMB(err), chunksWritten, totalBytesWritten), nil
		}

		// Write to destination
		if err := dstBlockStore.WriteAt(ctx.Context, string(writeOp.PayloadID), data, chunk.TargetOffset); err != nil {
			logger.Warn("COPYCHUNK: destination write failed",
				"chunk", i, "dstPath", dstOpen.Path, "error", err)
			return copyChunkPartialResponse(ctlCode, dstFileID,
				types.StatusInternalError, chunksWritten, totalBytesWritten), nil
		}

		// Commit write metadata
		if _, err := metaSvc.CommitWrite(authCtx, writeOp); err != nil {
			logger.Warn("COPYCHUNK: commit write failed",
				"chunk", i, "dstPath", dstOpen.Path, "error", err)
			return copyChunkPartialResponse(ctlCode, dstFileID,
				types.StatusInternalError, chunksWritten, totalBytesWritten), nil
		}

		// Flush deferred metadata (SMB requires immediate visibility)
		if _, flushErr := metaSvc.FlushPendingWriteForFile(authCtx, dstOpen.MetadataHandle); flushErr != nil {
			logger.Debug("COPYCHUNK: deferred metadata flush failed (non-fatal)",
				"dstPath", dstOpen.Path, "error", flushErr)
		}

		lastWritePayloadID = writeOp.PayloadID
		chunksWritten++
		totalBytesWritten += uint64(chunk.Length)
	}

	// Update cached PayloadID on destination (matches write.go pattern).
	// This ensures close.go flushes block store data correctly.
	dstOpen.PayloadID = lastWritePayloadID

	// Per MS-FSA 2.1.5.14.2: restore frozen timestamps after writes
	h.restoreFrozenTimestamps(authCtx, dstOpen)

	// Per MS-FSA 2.1.5.3: update LastAccessTime on source (read) and destination (write).
	// Hoist a single timestamp for consistency (matches write.go pattern).
	now := time.Now()
	if !srcOpen.AtimeFrozen {
		_ = metaSvc.SetFileAttributes(authCtx, srcOpen.MetadataHandle, &metadata.SetAttrs{Atime: &now})
	}
	if !dstOpen.AtimeFrozen {
		_ = metaSvc.SetFileAttributes(authCtx, dstOpen.MetadataHandle, &metadata.SetAttrs{Atime: &now})
	}
	if len(dstOpen.ParentHandle) > 0 {
		_ = metaSvc.SetFileAttributes(authCtx, dstOpen.ParentHandle, &metadata.SetAttrs{Atime: &now})
		h.restoreParentDirFrozenTimestamps(authCtx, dstOpen.ParentHandle)
	}

	logger.Debug("COPYCHUNK: completed successfully",
		"srcPath", srcOpen.Path, "dstPath", dstOpen.Path,
		"chunks", chunksWritten, "totalBytes", totalBytesWritten)

	return copyChunkSuccessResponse(ctlCode, dstFileID, chunksWritten, totalBytesWritten), nil
}

// copyChunkPartialResponse builds a SRV_COPYCHUNK_RESPONSE with partial results
// and the given error status. Used when a chunk fails mid-copy.
func copyChunkPartialResponse(
	ctlCode uint32,
	fileID [16]byte,
	status types.Status,
	chunksWritten uint32,
	totalBytesWritten uint64,
) *HandlerResult {
	w := smbenc.NewWriter(copyChunkResponseLen)
	w.WriteUint32(chunksWritten)
	w.WriteUint32(0) // ChunkBytesWritten
	w.WriteUint32(uint32(totalBytesWritten))

	resp := buildIoctlResponse(ctlCode, fileID, w.Bytes())
	return NewResult(status, resp)
}

// readCtlCode extracts the CtlCode from an IOCTL request body.
func readCtlCode(body []byte) uint32 {
	if len(body) < 8 {
		return 0
	}
	r := smbenc.NewReader(body[4:])
	return r.ReadUint32()
}

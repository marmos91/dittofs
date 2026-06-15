package handlers

import (
	"fmt"
	"strings"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// handleSetReparsePoint handles FSCTL_SET_REPARSE_POINT [MS-FSCC] 2.3.69 — the
// write half of reparse-point support (the read half is handleGetReparsePoint).
//
// macOS (mount_smbfs) and Windows create symbolic links over SMB by: CREATE an
// empty file → IOCTL FSCTL_SET_REPARSE_POINT with an IO_REPARSE_TAG_SYMLINK
// buffer → CLOSE. Modern Linux cifs.ko with `reparse=nfs` uses
// IO_REPARSE_TAG_NFS instead. Without this handler the IOCTL dispatcher returns
// STATUS_NOT_SUPPORTED and symlink-bearing trees (.app bundles, .frameworks,
// node_modules, dylib version chains) fail to upload (#1179).
//
// We convert the just-created regular file into a real metadata symlink so it
// is interoperable with the NFS adapter (shared metadata.Service.CreateSymlink).
func (h *Handler) handleSetReparsePoint(ctx *SMBHandlerContext, body []byte) (*HandlerResult, error) {
	fileID, ok := parseIoctlFileID(body)
	if !ok {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	openFile, ok := h.GetOpenFile(fileID)
	if !ok {
		logger.Debug("IOCTL SET_REPARSE_POINT: file handle not found (closed)", "fileID", fmt.Sprintf("%x", fileID))
		return NewErrorResult(types.StatusFileClosed), nil
	}

	// A reparse point cannot be set on a directory handle here.
	if openFile.IsDirectory {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	input := parseIoctlInputData(body)
	if len(input) < 8 {
		logger.Debug("IOCTL SET_REPARSE_POINT: input too small", "len", len(input))
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// REPARSE_DATA_BUFFER header [MS-FSCC] 2.1.2.1: ReparseTag(4),
	// ReparseDataLength(2), Reserved(2), then the tag-specific DataBuffer.
	r := smbenc.NewReader(input)
	reparseTag := r.ReadUint32()
	reparseDataLen := int(r.ReadUint16())
	r.Skip(2) // Reserved
	if r.Err() != nil {
		return NewErrorResult(types.StatusInvalidParameter), nil
	}
	dataBuf := input[8:]
	if reparseDataLen > len(dataBuf) {
		logger.Debug("IOCTL SET_REPARSE_POINT: ReparseDataLength overflows buffer",
			"reparseDataLen", reparseDataLen, "have", len(dataBuf))
		return NewErrorResult(types.StatusInvalidParameter), nil
	}
	dataBuf = dataBuf[:reparseDataLen]

	var target string
	var perr error
	switch reparseTag {
	case IoReparseTagSymlink:
		target, perr = parseSymlinkReparseTarget(dataBuf)
	case IoReparseTagNfs:
		target, perr = parseNfsReparseTarget(dataBuf)
	default:
		logger.Debug("IOCTL SET_REPARSE_POINT: unsupported reparse tag",
			"tag", fmt.Sprintf("0x%08X", reparseTag))
		return NewErrorResult(types.StatusIoReparseTagMismatch), nil
	}
	if perr != nil {
		logger.Debug("IOCTL SET_REPARSE_POINT: malformed reparse buffer",
			"tag", fmt.Sprintf("0x%08X", reparseTag), "error", perr)
		return NewErrorResult(types.StatusInvalidParameter), nil
	}

	// Normalize Windows separators to forward slash. Symlink targets are stored
	// as opaque, client-resolved strings (the DittoFS server never follows
	// them), so we apply the permissive NFS-style validator — matching what the
	// NFS adapter already accepts via CreateSymlink — rather than the stricter
	// MFsymlink validator. See pkg/metadata/validation.go.
	target = strings.ReplaceAll(target, "\\", "/")
	if err := metadata.ValidateSymlinkTarget(target); err != nil {
		logger.Debug("IOCTL SET_REPARSE_POINT: invalid symlink target", "error", err)
		return NewErrorResult(common.MapToSMB(err)), nil
	}

	if err := h.convertOpenFileToNativeSymlink(ctx, openFile, target); err != nil {
		logger.Warn("IOCTL SET_REPARSE_POINT: failed to create symlink",
			"path", openFile.Path, "target", target, "error", err)
		return NewErrorResult(common.MapToSMB(err)), nil
	}

	logger.Debug("IOCTL SET_REPARSE_POINT: created symlink", "path", openFile.Path, "target", target)

	// FSCTL_SET_REPARSE_POINT has no output buffer.
	resp := buildIoctlResponse(FsctlSetReparsePoint, fileID, nil)
	return NewResult(types.StatusSuccess, resp), nil
}

// parseSymlinkReparseTarget decodes the target path from a
// SYMBOLIC_LINK_REPARSE_DATA_BUFFER DataBuffer [MS-FSCC] 2.1.2.4 (the bytes
// after the 8-byte REPARSE_DATA_BUFFER header):
//
//	SubstituteNameOffset(2) SubstituteNameLength(2) PrintNameOffset(2)
//	PrintNameLength(2) Flags(4) PathBuffer(var, UTF-16LE)
//
// PathBuffer begins 12 bytes into the DataBuffer; the name offsets are relative
// to the start of PathBuffer. We prefer the PrintName (human-readable, e.g.
// "../A") and fall back to the SubstituteName, stripping the Windows `\??\`
// device prefix.
func parseSymlinkReparseTarget(data []byte) (string, error) {
	if len(data) < 12 {
		return "", fmt.Errorf("symlink reparse data too small: %d", len(data))
	}
	r := smbenc.NewReader(data)
	subOff := int(r.ReadUint16())
	subLen := int(r.ReadUint16())
	printOff := int(r.ReadUint16())
	printLen := int(r.ReadUint16())
	_ = r.ReadUint32() // Flags: bit 0 = relative [MS-FSCC] 2.1.2.4; ignored, targets are opaque
	if r.Err() != nil {
		return "", fmt.Errorf("symlink reparse header truncated")
	}
	const pathBufferStart = 12
	pathBuffer := data[pathBufferStart:]

	read := func(off, length int) (string, bool) {
		if length <= 0 || length%2 != 0 {
			return "", false
		}
		if off < 0 || off+length > len(pathBuffer) {
			return "", false
		}
		return decodeUTF16LE(pathBuffer[off : off+length]), true
	}

	if s, ok := read(printOff, printLen); ok && s != "" {
		return stripReparsePrefix(s), nil
	}
	if s, ok := read(subOff, subLen); ok && s != "" {
		return stripReparsePrefix(s), nil
	}
	return "", fmt.Errorf("symlink reparse buffer has no usable name")
}

// parseNfsReparseTarget decodes the target from an IO_REPARSE_TAG_NFS DataBuffer
// [MS-FSCC] 2.1.2.6. For a symlink the DataBuffer is an 8-byte InodeType equal
// to NFS_SPECFILE_LNK followed by the target path. Linux cifs.ko (`reparse=nfs`)
// emits the target as UTF-8 bytes. This branch is best-effort Linux interop;
// the macOS/Windows path uses parseSymlinkReparseTarget. Authoritative
// validation requires a real Linux `reparse=nfs` mount.
func parseNfsReparseTarget(data []byte) (string, error) {
	if len(data) < 8 {
		return "", fmt.Errorf("nfs reparse data too small: %d", len(data))
	}
	inodeType := smbenc.NewReader(data).ReadUint64()
	if inodeType != nfsSpecfileLnk {
		return "", fmt.Errorf("nfs reparse inode type 0x%x is not a symlink", inodeType)
	}
	target := strings.TrimRight(string(data[8:]), "\x00")
	if target == "" {
		return "", fmt.Errorf("nfs reparse buffer has empty target")
	}
	return target, nil
}

// stripReparsePrefix removes the Windows `\??\` (or already-normalized `/??/`)
// NT-namespace device prefix that Windows clients prepend to absolute symlink
// SubstituteNames.
func stripReparsePrefix(s string) string {
	s = strings.TrimPrefix(s, `\??\`)
	s = strings.TrimPrefix(s, "/??/")
	return s
}

// convertOpenFileToNativeSymlink removes the just-created regular file backing
// openFile and creates a real symlink in its place, then refreshes openFile so
// the trailing CLOSE is a clean no-op.
//
// This mirrors convertToRealSymlink (the MFsymlink/CLOSE path) but (a) uses the
// permissive symlink-target validation already applied by the caller and (b)
// handles the mid-session stale-handle problem: unlike MFsymlink conversion
// (which runs at CLOSE as the handle is torn down), SET_REPARSE_POINT converts
// while the handle is still open and the client will CLOSE it afterward. After
// the remove+recreate, openFile.MetadataHandle/PayloadID point at the deleted
// regular file, so we repoint MetadataHandle at the new symlink and clear
// PayloadID. That makes CLOSE step 5 (MFsymlink re-read, gated on PayloadID)
// skip and step 6 (POSTQUERY GetFile) resolve the symlink. The client does not
// set DELETE_ON_CLOSE here, so no delete fires.
func (h *Handler) convertOpenFileToNativeSymlink(ctx *SMBHandlerContext, openFile *OpenFile, target string) error {
	if len(openFile.ParentHandle) == 0 || openFile.FileName == "" {
		return fmt.Errorf("missing parent handle or filename for symlink conversion")
	}

	// Prime ctx.User / IsGuest / TreeID from the OpenFile's recorded session
	// BEFORE BuildAuthContext — otherwise ctx.User==nil synthesises UID-0
	// (root), bypassing DACL checks on RemoveFile + CreateSymlink (#619).
	h.primeAuthContextFromOpenFile(ctx, openFile)

	authCtx, err := BuildAuthContext(ctx)
	if err != nil {
		return err
	}

	parentHandle := openFile.ParentHandle
	fileName := openFile.FileName

	metaSvc := h.Registry.GetMetadataService()
	removed, _, err := metaSvc.RemoveFile(authCtx, parentHandle, fileName)
	if err != nil {
		return fmt.Errorf("failed to remove placeholder file: %w", err)
	}

	// Delete content from block store (best-effort). RemoveFile returns an
	// empty PayloadID when content must survive (hard link / recycle).
	var removedPayloadID metadata.PayloadID
	if removed != nil {
		removedPayloadID = removed.PayloadID
	}
	h.purgeBlockStorePayload(ctx.Context, openFile.MetadataHandle, removedPayloadID, openFile.Path, "SET_REPARSE_POINT")

	symlink, _, err := metaSvc.CreateSymlink(authCtx, parentHandle, fileName, target, &metadata.FileAttr{})
	if err != nil {
		// The placeholder is already unlinked; CreateSymlink failing would
		// otherwise leave the name with neither a file nor a symlink. Best-effort
		// re-create an empty regular placeholder so the namespace entry (and the
		// still-open handle's CLOSE path) survive. Return the original error.
		if _, _, reErr := metaSvc.CreateFile(authCtx, parentHandle, fileName, &metadata.FileAttr{
			Type: metadata.FileTypeRegular, Mode: 0o644,
		}); reErr != nil {
			logger.Warn("SET_REPARSE_POINT: symlink create failed and placeholder rollback failed",
				"path", openFile.Path, "createErr", err, "rollbackErr", reErr)
		}
		return fmt.Errorf("failed to create symlink: %w", err)
	}

	// Refresh the open handle to point at the new symlink so the trailing CLOSE
	// does not operate on the deleted regular file. openFile is the same pointer
	// held in h.files, so the in-place mutation under mu is sufficient — no
	// StoreOpenFile re-insert (which could resurrect a handle a concurrent
	// session teardown just removed).
	newHandle, err := metadata.EncodeFileHandle(symlink)
	if err != nil {
		return fmt.Errorf("failed to encode new symlink handle: %w", err)
	}
	openFile.mu.Lock()
	openFile.MetadataHandle = newHandle
	openFile.PayloadID = ""
	openFile.mu.Unlock()

	// Notify directory watchers of the placeholder→symlink replacement so client
	// directory views (Finder/Explorer) refresh without a full re-enumeration.
	if h.NotifyRegistry != nil {
		parentPath := GetParentPath(openFile.Path)
		h.NotifyRegistry.NotifyChange(openFile.ShareName, parentPath, fileName, FileActionRemoved, FileNotifyChangeFileName)
		h.NotifyRegistry.NotifyChange(openFile.ShareName, parentPath, fileName, FileActionAdded, FileNotifyChangeFileName)
	}

	return nil
}

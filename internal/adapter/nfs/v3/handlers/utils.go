package handlers

import (
	"context"
	"errors"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/mfsymlink"
	"github.com/marmos91/dittofs/pkg/blockstore/engine"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// validationError represents a request validation error with an NFS status code.
// This is the single shared validation error type used by all v3 handler validators.
type validationError struct {
	message   string
	nfsStatus uint32
}

func (e *validationError) Error() string {
	return e.message
}

// ErrMetadataServiceNotInitialized is returned when the metadata service is not available.
var ErrMetadataServiceNotInitialized = errors.New("metadata service not initialized")

// getServicesForHandle returns both the metadata service and the per-share block store
// resolved from the given file handle.
// Returns an error if either service is not initialized or handle resolution fails.
func getServicesForHandle(reg *runtime.Runtime, ctx context.Context, handle metadata.FileHandle) (*metadata.MetadataService, *engine.BlockStore, error) {
	metaSvc := reg.GetMetadataService()
	if metaSvc == nil {
		return nil, nil, ErrMetadataServiceNotInitialized
	}

	blockStore, err := getBlockStoreForHandle(reg, ctx, handle)
	if err != nil {
		return nil, nil, err
	}

	return metaSvc, blockStore, nil
}

// getMetadataService returns the metadata service from the runtime.
// Returns an error if the service is not initialized.
func getMetadataService(reg *runtime.Runtime) (*metadata.MetadataService, error) {
	metaSvc := reg.GetMetadataService()
	if metaSvc == nil {
		return nil, ErrMetadataServiceNotInitialized
	}
	return metaSvc, nil
}

// getBlockStoreForHandle returns the per-share block store resolved from the given file handle.
// The handle encodes the share name, which is used to look up the share's block store.
func getBlockStoreForHandle(reg *runtime.Runtime, ctx context.Context, handle metadata.FileHandle) (*engine.BlockStore, error) {
	return reg.GetBlockStoreForHandle(ctx, handle)
}

// safeAdd performs checked addition of two uint64 values.
// Returns the sum and a boolean indicating whether overflow occurred.
func safeAdd(a, b uint64) (uint64, bool) {
	sum := a + b
	overflow := sum < a // If sum wrapped around, it will be less than a
	return sum, overflow
}

// buildWccAttr builds WCC (Weak Cache Consistency) attributes from FileAttr.
// Used in WRITE, COMMIT, and other procedures to help clients detect concurrent modifications.
//
// WCC data consists of file attributes before and after an operation, allowing clients
// to invalidate their caches if the file changed unexpectedly.
func buildWccAttr(attr *metadata.FileAttr) *types.WccAttr {
	return &types.WccAttr{
		Size: attr.Size,
		Mtime: types.TimeVal{
			Seconds:  uint32(attr.Mtime.Unix()),
			Nseconds: uint32(attr.Mtime.Nanosecond()),
		},
		Ctime: types.TimeVal{
			Seconds:  uint32(attr.Ctime.Unix()),
			Nseconds: uint32(attr.Ctime.Nanosecond()),
		},
	}
}

// ============================================================================
// Error Logging Helpers
// ============================================================================

// logError logs an error.
// Use this for actual errors that indicate something went wrong.
func logError(ctx context.Context, err error, msg string, args ...any) {
	if err != nil {
		args = append(args, "error", err)
	}
	logger.ErrorCtx(ctx, msg, args...)
}

// logWarn logs a warning.
// Use this for expected failures like "file not found" or "permission denied".
func logWarn(ctx context.Context, err error, msg string, args ...any) {
	if err != nil {
		args = append(args, "error", err)
	}
	logger.WarnCtx(ctx, msg, args...)
}

// ============================================================================
// MFsymlink Detection for NFS/SMB Interoperability
// ============================================================================
//
// MFsymlinks are 1067-byte files with "XSym\n" header used by macOS/Windows
// SMB clients for symlink creation. When accessed via NFS, these files should
// appear as symlinks for cross-protocol compatibility.
//
// Detection is performed when:
// - File is regular type (not already a symlink)
// - File size is exactly 1067 bytes (mfsymlink.Size)
// - File content starts with "XSym\n" magic marker

// MFsymlinkResult contains the result of MFsymlink detection.
type MFsymlinkResult struct {
	// IsMFsymlink indicates if the file is a valid MFsymlink
	IsMFsymlink bool

	// Target is the symlink target (only valid if IsMFsymlink is true)
	Target string

	// ModifiedAttr contains modified attributes to present the file as a symlink
	// (only valid if IsMFsymlink is true)
	ModifiedAttr *metadata.FileAttr
}

// checkMFsymlink checks if a file is an unconverted MFsymlink and returns
// the symlink target if so. This enables NFS clients to see SMB-created
// symlinks before they are converted on CLOSE.
func checkMFsymlink(
	ctx context.Context,
	reg *runtime.Runtime,
	handle metadata.FileHandle,
	file *metadata.File,
) MFsymlinkResult {
	// Quick checks first (no I/O)
	if file.Type != metadata.FileTypeRegular {
		return MFsymlinkResult{}
	}

	if file.Size != uint64(mfsymlink.Size) {
		return MFsymlinkResult{}
	}

	// File has correct size - need to check content
	content, err := readMFsymlinkContentForNFS(ctx, reg, handle, file.PayloadID)
	if err != nil {
		logger.Debug("checkMFsymlink: failed to read content",
			"payloadID", file.PayloadID,
			"error", err)
		return MFsymlinkResult{}
	}

	if !mfsymlink.IsMFsymlink(content) {
		return MFsymlinkResult{}
	}

	target, err := mfsymlink.Decode(content)
	if err != nil {
		logger.Debug("checkMFsymlink: invalid MFsymlink format",
			"payloadID", file.PayloadID,
			"error", err)
		return MFsymlinkResult{}
	}

	// Create modified attributes to present as symlink
	modifiedAttr := file.FileAttr // Copy
	modifiedAttr.Type = metadata.FileTypeSymlink
	modifiedAttr.Size = uint64(len(target))
	// Mode: symlinks typically have 0777 permissions
	modifiedAttr.Mode = modifiedAttr.Mode&^uint32(0777) | 0777

	logger.Debug("checkMFsymlink: detected MFsymlink",
		"payloadID", file.PayloadID,
		"target", target)

	return MFsymlinkResult{
		IsMFsymlink:  true,
		Target:       target,
		ModifiedAttr: &modifiedAttr,
	}
}

// readMFsymlinkContentForNFS reads content from the block store (uses local cache internally).
func readMFsymlinkContentForNFS(
	ctx context.Context,
	reg *runtime.Runtime,
	handle metadata.FileHandle,
	payloadID metadata.PayloadID,
) ([]byte, error) {
	if payloadID == "" {
		return nil, nil
	}

	blockStore, err := getBlockStoreForHandle(reg, ctx, handle)
	if err != nil {
		return nil, err
	}

	data := make([]byte, mfsymlink.Size)
	n, err := blockStore.ReadAt(ctx, string(payloadID), data, 0)
	if err != nil {
		return nil, err
	}

	return data[:n], nil
}

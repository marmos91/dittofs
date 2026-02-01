package handlers

import (
	"context"
	"errors"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/mfsymlink"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/telemetry"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/payload"
)

// ErrMetadataServiceNotInitialized is returned when the metadata service is not available.
var ErrMetadataServiceNotInitialized = errors.New("metadata service not initialized")

// ErrPayloadServiceNotInitialized is returned when the payload service is not available.
var ErrPayloadServiceNotInitialized = errors.New("payload service not initialized")

// getServices returns both the metadata and payload services from the runtime.
// Returns an error if either service is not initialized.
func getServices(reg *runtime.Runtime) (*metadata.MetadataService, *payload.PayloadService, error) {
	metaSvc := reg.GetMetadataService()
	if metaSvc == nil {
		return nil, nil, ErrMetadataServiceNotInitialized
	}

	payloadSvc := reg.GetPayloadService()
	if payloadSvc == nil {
		return nil, nil, ErrPayloadServiceNotInitialized
	}

	return metaSvc, payloadSvc, nil
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

// getPayloadService returns the payload service from the runtime.
// Returns an error if the service is not initialized.
func getPayloadService(reg *runtime.Runtime) (*payload.PayloadService, error) {
	payloadSvc := reg.GetPayloadService()
	if payloadSvc == nil {
		return nil, ErrPayloadServiceNotInitialized
	}
	return payloadSvc, nil
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
// Trace-Aware Error Logging
// ============================================================================
// These helper functions combine logging with OpenTelemetry span error recording.
// Use these instead of plain logger.ErrorCtx/WarnCtx when the error should also
// be visible in distributed traces.

// traceError logs an error and records it on the current span.
// Use this for actual errors that indicate something went wrong.
func traceError(ctx context.Context, err error, msg string, args ...any) {
	if err != nil {
		telemetry.RecordError(ctx, err)
		// Only append error to args if err is not nil
		args = append(args, "error", err)
	}
	logger.ErrorCtx(ctx, msg, args...)
}

// traceWarn logs a warning and optionally records it on the span.
// Warnings are recorded on spans as events (not errors) when err is not nil.
// Use this for expected failures like "file not found" or "permission denied".
func traceWarn(ctx context.Context, err error, msg string, args ...any) {
	if err != nil {
		// Add as event, not error - warnings shouldn't mark span as failed
		telemetry.AddEvent(ctx, msg, telemetry.Error(err))
		// Only append error to args if err is not nil
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
//
// Parameters:
//   - ctx: Context for cancellation and logging
//   - reg: Registry to get content store
//   - share: Share name to get content store
//   - file: File metadata to check
//
// Returns MFsymlinkResult with detection result and modified attributes.
func checkMFsymlink(
	ctx context.Context,
	reg *runtime.Runtime,
	share string,
	file *metadata.File,
) MFsymlinkResult {
	// Quick checks first (no I/O)
	if file.Type != metadata.FileTypeRegular {
		return MFsymlinkResult{IsMFsymlink: false}
	}

	if file.Size != uint64(mfsymlink.Size) {
		return MFsymlinkResult{IsMFsymlink: false}
	}

	// File has correct size - need to check content
	// First try cache, then content store
	content, err := readMFsymlinkContentForNFS(ctx, reg, share, file.PayloadID)
	if err != nil {
		logger.Debug("checkMFsymlink: failed to read content",
			"payloadID", file.PayloadID,
			"error", err)
		return MFsymlinkResult{IsMFsymlink: false}
	}

	// Verify MFsymlink format
	if !mfsymlink.IsMFsymlink(content) {
		return MFsymlinkResult{IsMFsymlink: false}
	}

	// Parse symlink target
	target, err := mfsymlink.Decode(content)
	if err != nil {
		logger.Debug("checkMFsymlink: invalid MFsymlink format",
			"payloadID", file.PayloadID,
			"error", err)
		return MFsymlinkResult{IsMFsymlink: false}
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

// readMFsymlinkContentForNFS reads content from ContentService (uses Cache internally).
func readMFsymlinkContentForNFS(
	ctx context.Context,
	reg *runtime.Runtime,
	_ /* share */ string,
	payloadID metadata.PayloadID,
) ([]byte, error) {
	if payloadID == "" {
		return nil, nil
	}

	// Use ContentService.ReadAt (Cache handles caching automatically)
	payloadSvc, err := getPayloadService(reg)
	if err != nil {
		return nil, err
	}

	data := make([]byte, mfsymlink.Size)
	n, err := payloadSvc.ReadAt(ctx, payloadID, data, 0)
	if err != nil {
		return nil, err
	}

	return data[:n], nil
}

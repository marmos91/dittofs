package handlers

import (
	"context"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/mfsymlink"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/telemetry"
	"github.com/marmos91/dittofs/pkg/registry"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// safeAdd performs checked addition of two uint64 values.
// Returns the sum and a boolean indicating whether overflow occurred.
func safeAdd(a, b uint64) (uint64, bool) {
	sum := a + b
	overflow := sum < a // If sum wrapped around, it will be less than a
	return sum, overflow
}

// ============================================================================
// Transient Error Retry Logic
// ============================================================================
//
// The Linux kernel NFS server (fs/nfsd) implements retry logic for transient
// errors like EAGAIN. While NFSv3 doesn't have delegations, retry logic is
// still useful for:
//   - Backend transient errors (S3 throttling, temporary unavailability)
//   - Lock conflicts when NLM is implemented
//   - Temporary resource exhaustion
//
// Example from Linux kernel fs/nfsd/vfs.c:
//   for (retries = 1;;) {
//       host_err = vfs_rename(&rd);
//       if (host_err != -EAGAIN || !retries--) break;
//       if (!nfsd_wait_for_delegreturn(...)) break;
//   }

// RetryConfig configures retry behavior for transient errors.
type RetryConfig struct {
	// MaxRetries is the maximum number of retry attempts (default: 1)
	MaxRetries int

	// RetryableErrors is a function that returns true if the error is retryable
	RetryableErrors func(error) bool
}

// DefaultRetryConfig provides sensible defaults for retry behavior.
var DefaultRetryConfig = RetryConfig{
	MaxRetries:      1,
	RetryableErrors: isTransientError,
}

// isTransientError checks if an error is transient and worth retrying.
// This matches the errors that Linux kernel NFS retries.
func isTransientError(err error) bool {
	if err == nil {
		return false
	}

	// Check for metadata store errors that indicate transient conditions
	if storeErr, ok := err.(*metadata.StoreError); ok {
		switch storeErr.Code {
		case metadata.ErrLocked:
			// Lock conflict - retry may succeed if lock is released
			return true
		case metadata.ErrIOError:
			// Some I/O errors may be transient (e.g., temporary network issues)
			// Be conservative here - only retry on specific patterns
			return false
		}
	}

	// Could also check for context.DeadlineExceeded for timeout-based retries
	// but we generally want to respect timeouts, not retry them

	return false
}

// withRetry executes an operation with retry logic for transient errors.
// The operation function returns a result and an error.
// On transient errors, the operation is retried up to MaxRetries times.
//
// Usage:
//
//	result, err := withRetry(ctx, DefaultRetryConfig, func() (*Result, error) {
//	    return store.SomeOperation(...)
//	})
func withRetry[T any](ctx context.Context, cfg RetryConfig, op func() (T, error)) (T, error) {
	var zero T
	retries := cfg.MaxRetries
	if retries < 0 {
		retries = 0
	}

	for attempt := 0; attempt <= retries; attempt++ {
		// Check context before each attempt
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}

		result, err := op()
		if err == nil {
			return result, nil
		}

		// Check if error is retryable
		if !cfg.RetryableErrors(err) {
			return result, err
		}

		// Last attempt failed - return error
		if attempt >= retries {
			return result, err
		}

		// Log retry attempt
		logger.Debug("Retrying operation due to transient error",
			"attempt", attempt+1,
			"maxRetries", retries,
			"error", err)
	}

	return zero, nil // Should not reach here
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
	reg *registry.Registry,
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
	content, err := readMFsymlinkContentForNFS(ctx, reg, share, file.ContentID)
	if err != nil {
		logger.Debug("checkMFsymlink: failed to read content",
			"contentID", file.ContentID,
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
			"contentID", file.ContentID,
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
		"contentID", file.ContentID,
		"target", target)

	return MFsymlinkResult{
		IsMFsymlink:  true,
		Target:       target,
		ModifiedAttr: &modifiedAttr,
	}
}

// readMFsymlinkContentForNFS reads content from cache or content store.
func readMFsymlinkContentForNFS(
	ctx context.Context,
	reg *registry.Registry,
	share string,
	contentID metadata.ContentID,
) ([]byte, error) {
	if contentID == "" {
		return nil, nil
	}

	// Try cache first
	fileCache := reg.GetCacheForShare(share)
	if fileCache != nil && fileCache.Size(contentID) > 0 {
		data := make([]byte, mfsymlink.Size)
		n, err := fileCache.ReadAt(ctx, contentID, data, 0)
		if err == nil && n == mfsymlink.Size {
			return data, nil
		}
	}

	// Fall back to content store
	contentStore, err := reg.GetContentStoreForShare(share)
	if err != nil {
		return nil, err
	}

	reader, err := contentStore.ReadContent(ctx, contentID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()

	data := make([]byte, mfsymlink.Size)
	totalRead := 0
	for totalRead < mfsymlink.Size {
		n, err := reader.Read(data[totalRead:])
		if err != nil {
			if totalRead > 0 {
				break
			}
			return nil, err
		}
		totalRead += n
	}

	return data[:totalRead], nil
}

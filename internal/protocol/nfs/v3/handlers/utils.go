package handlers

import (
	"context"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/telemetry"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

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

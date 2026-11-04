package handlers

import (
	"context"
	"errors"

	"github.com/marmos91/dittofs/internal/logger"
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

// getMetadataService returns the metadata service from the runtime.
// Returns an error if the service is not initialized.
func getMetadataService(reg nfsRuntime) (*metadata.Service, error) {
	metaSvc := reg.GetMetadataService()
	if metaSvc == nil {
		return nil, ErrMetadataServiceNotInitialized
	}
	return metaSvc, nil
}

// safeAdd performs checked addition of two uint64 values.
// Returns the sum and a boolean indicating whether overflow occurred.
func safeAdd(a, b uint64) (uint64, bool) {
	sum := a + b
	overflow := sum < a // If sum wrapped around, it will be less than a
	return sum, overflow
}

// Error Logging Helpers

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

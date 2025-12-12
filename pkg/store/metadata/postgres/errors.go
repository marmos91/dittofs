package postgres

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// mapPgError maps PostgreSQL errors to metadata store errors
func mapPgError(err error, operation, path string) error {
	if err == nil {
		return nil
	}

	// Handle pgx.ErrNoRows (not found)
	if errors.Is(err, pgx.ErrNoRows) {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: fmt.Sprintf("%s: not found", operation),
			Path:    path,
		}
	}

	// Handle PostgreSQL-specific errors
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return mapPgErrorCode(pgErr, operation, path)
	}

	// Unknown error - treat as I/O error
	return &metadata.StoreError{
		Code:    metadata.ErrIOError,
		Message: fmt.Sprintf("%s: %v", operation, err),
		Path:    path,
	}
}

// mapPgErrorCode maps PostgreSQL error codes to metadata store errors
func mapPgErrorCode(pgErr *pgconn.PgError, operation, path string) error {
	// PostgreSQL error codes: https://www.postgresql.org/docs/current/errcodes-appendix.html
	switch pgErr.Code {
	// 23505: unique_violation
	case "23505":
		return &metadata.StoreError{
			Code:    metadata.ErrAlreadyExists,
			Message: fmt.Sprintf("%s: already exists", operation),
			Path:    path,
		}

	// 23503: foreign_key_violation
	case "23503":
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: fmt.Sprintf("%s: referenced item not found", operation),
			Path:    path,
		}

	// 23514: check_constraint_violation
	case "23514":
		// Check constraints like non-empty directory, valid mode, etc.
		if contains(pgErr.Message, "non_empty") {
			return &metadata.StoreError{
				Code:    metadata.ErrNotEmpty,
				Message: fmt.Sprintf("%s: directory not empty", operation),
				Path:    path,
			}
		}
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: fmt.Sprintf("%s: invalid value", operation),
			Path:    path,
		}

	// 23502: not_null_violation
	case "23502":
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: fmt.Sprintf("%s: missing required field", operation),
			Path:    path,
		}

	// 40001: serialization_failure (transaction conflict)
	case "40001":
		return &metadata.StoreError{
			Code:    metadata.ErrIOError,
			Message: fmt.Sprintf("%s: transaction conflict, retry", operation),
			Path:    path,
		}

	// 40P01: deadlock_detected
	case "40P01":
		return &metadata.StoreError{
			Code:    metadata.ErrIOError,
			Message: fmt.Sprintf("%s: deadlock detected, retry", operation),
			Path:    path,
		}

	// 53100: disk_full
	case "53100":
		return &metadata.StoreError{
			Code:    metadata.ErrNoSpace,
			Message: fmt.Sprintf("%s: no space available", operation),
			Path:    path,
		}

	// 53200: out_of_memory
	case "53200":
		return &metadata.StoreError{
			Code:    metadata.ErrNoSpace,
			Message: fmt.Sprintf("%s: out of memory", operation),
			Path:    path,
		}

	// 57014: query_canceled
	case "57014":
		return &metadata.StoreError{
			Code:    metadata.ErrIOError,
			Message: fmt.Sprintf("%s: operation canceled", operation),
			Path:    path,
		}

	// 08000-08006: connection errors
	case "08000", "08003", "08006":
		return &metadata.StoreError{
			Code:    metadata.ErrIOError,
			Message: fmt.Sprintf("%s: database connection error", operation),
			Path:    path,
		}

	// Default: treat as I/O error
	default:
		return &metadata.StoreError{
			Code:    metadata.ErrIOError,
			Message: fmt.Sprintf("%s: database error [%s] %s", operation, pgErr.Code, pgErr.Message),
			Path:    path,
		}
	}
}

// contains checks if a string contains a substring (case-insensitive)
func contains(s, substr string) bool {
	return len(s) >= len(substr) &&
		(s == substr ||
			len(s) > len(substr) &&
				(s[:len(substr)] == substr ||
					s[len(s)-len(substr):] == substr ||
					findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

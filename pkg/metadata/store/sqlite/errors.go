package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// mapDBError maps SQLite/database errors to metadata store errors.
//
// modernc.org/sqlite surfaces constraint and busy conditions through the error
// string, so classification is done by inspecting the message. The high-value
// cases — not-found, unique violation (with the object_id dedup sub-case),
// foreign-key violation, NOT NULL, and busy/locked — are mapped to the same
// metadata.StoreError codes the Postgres backend emits, keeping the conformance
// suite backend-agnostic.
func mapDBError(err error, operation, path string) error {
	if err == nil {
		return nil
	}

	// Already a StoreError (e.g. from a validation helper): pass through.
	var storeErr *metadata.StoreError
	if errors.As(err, &storeErr) {
		return storeErr
	}

	// Not found.
	if errors.Is(err, sql.ErrNoRows) {
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: fmt.Sprintf("%s: not found", operation),
			Path:    path,
		}
	}

	msg := strings.ToLower(err.Error())

	switch {
	// UNIQUE / PRIMARY KEY constraint violation.
	case strings.Contains(msg, "unique constraint") ||
		strings.Contains(msg, "primary key constraint") ||
		strings.Contains(msg, "constraint failed: unique"):
		// The inodes_object_id_idx partial unique index enforces one ObjectID
		// per file. A second byte-identical file trips it; the rollup persist
		// path treats that as a BENIGN conflict (the duplicate's blocks are
		// already covered by the canonical file's manifest), so surface it as
		// ErrConflict rather than ErrAlreadyExists. The index name appears in
		// the SQLite constraint error text.
		if strings.Contains(msg, "object_id") || strings.Contains(msg, "inodes_object_id_idx") {
			return &metadata.StoreError{
				Code:    metadata.ErrConflict,
				Message: fmt.Sprintf("%s: object_id already mapped to another file", operation),
				Path:    path,
			}
		}
		return &metadata.StoreError{
			Code:    metadata.ErrAlreadyExists,
			Message: fmt.Sprintf("%s: already exists", operation),
			Path:    path,
		}

	// FOREIGN KEY constraint violation: a referenced row is missing.
	case strings.Contains(msg, "foreign key constraint"):
		return &metadata.StoreError{
			Code:    metadata.ErrNotFound,
			Message: fmt.Sprintf("%s: referenced item not found", operation),
			Path:    path,
		}

	// CHECK constraint violation.
	case strings.Contains(msg, "check constraint") || strings.Contains(msg, "constraint failed: check"):
		if strings.Contains(msg, "non_empty") {
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

	// NOT NULL constraint violation.
	case strings.Contains(msg, "not null constraint"):
		return &metadata.StoreError{
			Code:    metadata.ErrInvalidArgument,
			Message: fmt.Sprintf("%s: missing required field", operation),
			Path:    path,
		}

	// Busy / locked: contention under the single-writer engine. Mapped to an
	// I/O error and flagged retryable via Cause so WithTransaction retries.
	case strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked") ||
		strings.Contains(msg, "sqlite_busy"):
		return &metadata.StoreError{
			Code:    metadata.ErrIOError,
			Message: fmt.Sprintf("%s: database busy, retry", operation),
			Path:    path,
			Cause:   err,
		}

	// Disk full.
	case strings.Contains(msg, "database or disk is full"):
		return &metadata.StoreError{
			Code:    metadata.ErrNoSpace,
			Message: fmt.Sprintf("%s: no space available", operation),
			Path:    path,
		}

	default:
		return &metadata.StoreError{
			Code:    metadata.ErrIOError,
			Message: fmt.Sprintf("%s: database error: %v", operation, err),
			Path:    path,
		}
	}
}

// isBusyError reports whether err is a SQLite busy/locked condition that a
// transaction can retry. Mirrors the Postgres isRetryableError (deadlock /
// serialization-failure) role for the embedded engine.
func isBusyError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked") ||
		strings.Contains(msg, "sqlite_busy") ||
		// mapDBError rewrites busy/locked driver errors to this message before
		// they reach WithTransaction's retry check, so match it too.
		strings.Contains(msg, "database busy")
}

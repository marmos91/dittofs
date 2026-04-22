// Package errors defines the stable, machine-readable error taxonomy
// surfaced by backup endpoints (#414). Callers classify an arbitrary error
// into a fixed Code enum and attach a short operator-facing hint.
//
// The wire values are the JSON source of truth and must never be renumbered
// or renamed — the Pro UI keys i18n strings off them. Adding new codes is
// always safe; removing or renaming is not.
package errors

import (
	"errors"
	"io/fs"
	"net"
	"syscall"

	smithy "github.com/aws/smithy-go"

	"github.com/marmos91/dittofs/pkg/backup/destination"
)

// Code is the stable wire value for a classified backup error.
type Code string

const (
	CodeDestinationPermissionDenied   Code = "destination_permission_denied"
	CodeDestinationNotFound           Code = "destination_not_found"
	CodeDestinationNoSpace            Code = "destination_no_space"
	CodeDestinationUnreachable        Code = "destination_unreachable"
	CodeDestinationCredentialsInvalid Code = "destination_credentials_invalid"
	CodeDestinationPathConflict       Code = "destination_path_conflict"
	CodeDestinationConfigInvalid      Code = "destination_config_invalid"
	CodeSourceUnavailable             Code = "source_unavailable"
	CodeBackupAlreadyRunning          Code = "backup_already_running"
	CodeRestorePreconditionFailed     Code = "restore_precondition_failed"
	CodeInternal                      Code = "internal"
)

// BackupError carries a classified code plus the original error so
// errors.Is / errors.As continue to match the wrapped sentinels.
type BackupError struct {
	Code Code
	Hint string
	Err  error
}

func (e *BackupError) Error() string {
	if e.Err == nil {
		return string(e.Code)
	}
	return e.Err.Error()
}

func (e *BackupError) Unwrap() error { return e.Err }

// New wraps err with an explicit code (use when the caller already knows
// the classification, e.g. the path-conflict validator).
func New(code Code, err error) *BackupError {
	return &BackupError{Code: code, Hint: HintFor(code), Err: err}
}

// Classify inspects err (and anything it wraps) and returns a BackupError
// whose Code best describes the failure. Returns nil when err is nil.
//
// If err already carries a *BackupError in its chain, that classification
// wins — Classify is idempotent.
func Classify(err error) *BackupError {
	if err == nil {
		return nil
	}
	var existing *BackupError
	if errors.As(err, &existing) {
		return existing
	}

	// S3 / smithy typed codes first — the S3 destination joins its own
	// sentinel with the original SDK error, so credential vs. permission
	// vs. bucket-not-found are distinguishable via the API error code even
	// after the sentinel is attached.
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "InvalidAccessKeyId", "SignatureDoesNotMatch", "InvalidSecurityToken",
			"InvalidClientTokenId", "ExpiredToken", "TokenRefreshRequired":
			return New(CodeDestinationCredentialsInvalid, err)
		case "NoSuchBucket", "NoSuchKey", "NotFound":
			return New(CodeDestinationNotFound, err)
		case "AccessDenied", "Forbidden":
			return New(CodeDestinationPermissionDenied, err)
		}
	}

	// Filesystem-class: disk full beats permission beats not-found because
	// ENOSPC is the most actionable for an operator.
	if errors.Is(err, syscall.ENOSPC) {
		return New(CodeDestinationNoSpace, err)
	}
	if errors.Is(err, destination.ErrPermissionDenied) || errors.Is(err, fs.ErrPermission) {
		return New(CodeDestinationPermissionDenied, err)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return New(CodeDestinationNotFound, err)
	}

	// Destination sentinels (after fs-specific checks so an ENOSPC wrapped
	// in ErrDestinationUnavailable still classifies as no_space).
	if errors.Is(err, destination.ErrDestinationUnavailable) ||
		errors.Is(err, destination.ErrDestinationThrottled) {
		return New(CodeDestinationUnreachable, err)
	}
	if errors.Is(err, destination.ErrIncompatibleConfig) {
		return New(CodeDestinationNotFound, err)
	}
	// Encryption-key misconfiguration is an operator-actionable credential
	// problem, not an internal failure.
	if errors.Is(err, destination.ErrEncryptionKeyMissing) ||
		errors.Is(err, destination.ErrInvalidKeyMaterial) {
		return New(CodeDestinationCredentialsInvalid, err)
	}

	// Network-class (DNS, timeouts) — after sentinels so S3 classifier's
	// joined ErrDestinationUnavailable wins when present.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return New(CodeDestinationUnreachable, err)
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return New(CodeDestinationUnreachable, err)
	}

	return New(CodeInternal, err)
}

// HintFor returns a short, English, operator-facing hint for a code.
// The Pro UI localizes via the code itself; this value is a developer /
// CLI fallback and a hint for log readers.
func HintFor(code Code) string {
	switch code {
	case CodeDestinationPermissionDenied:
		return "ensure the DittoFS process user can write to the backup destination"
	case CodeDestinationNotFound:
		return "check that the destination bucket or directory exists"
	case CodeDestinationNoSpace:
		return "free up disk space at the backup destination or choose a different path"
	case CodeDestinationUnreachable:
		return "check network connectivity and DNS resolution to the backup destination"
	case CodeDestinationCredentialsInvalid:
		return "re-enter the S3 credentials for this backup repo"
	case CodeDestinationPathConflict:
		return "this destination path is already used by another backup repo or block store"
	case CodeDestinationConfigInvalid:
		return "check the destination config (path writable, bucket name valid, required fields set)"
	case CodeSourceUnavailable:
		return "the metadata store could not be read; check that it is running and healthy"
	case CodeBackupAlreadyRunning:
		return "wait for the in-flight backup to finish, or cancel it before retrying"
	case CodeRestorePreconditionFailed:
		return "disable the listed shares on the target store before retrying restore"
	case CodeInternal:
		return "unexpected internal failure; check server logs for details"
	}
	return ""
}

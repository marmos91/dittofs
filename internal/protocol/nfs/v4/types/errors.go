package types

import (
	goerrors "errors"
	"strings"
	"unicode/utf8"

	"github.com/marmos91/dittofs/pkg/metadata/errors"
)

// MapMetadataErrorToNFS4 maps internal metadata errors to NFSv4 status codes.
//
// This mirrors the NFSv3 error mapping pattern but uses NFSv4-specific error
// codes from RFC 7530 Section 13. The mapping uses errors.As to match
// *errors.StoreError and switches on the error code.
//
// Returns NFS4ERR_SERVERFAULT for unrecognized errors.
func MapMetadataErrorToNFS4(err error) uint32 {
	if err == nil {
		return NFS4_OK
	}

	var storeErr *errors.StoreError
	if !goerrors.As(err, &storeErr) {
		return NFS4ERR_SERVERFAULT
	}

	switch storeErr.Code {
	case errors.ErrNotFound:
		return NFS4ERR_NOENT
	case errors.ErrAccessDenied, errors.ErrAuthRequired:
		return NFS4ERR_ACCESS
	case errors.ErrPermissionDenied:
		return NFS4ERR_PERM
	case errors.ErrAlreadyExists:
		return NFS4ERR_EXIST
	case errors.ErrNotEmpty:
		return NFS4ERR_NOTEMPTY
	case errors.ErrIsDirectory:
		return NFS4ERR_ISDIR
	case errors.ErrNotDirectory:
		return NFS4ERR_NOTDIR
	case errors.ErrInvalidArgument:
		return NFS4ERR_INVAL
	case errors.ErrNoSpace:
		return NFS4ERR_NOSPC
	case errors.ErrQuotaExceeded:
		return NFS4ERR_DQUOT
	case errors.ErrReadOnly:
		return NFS4ERR_ROFS
	case errors.ErrNotSupported:
		return NFS4ERR_NOTSUPP
	case errors.ErrStaleHandle:
		return NFS4ERR_STALE
	case errors.ErrInvalidHandle:
		return NFS4ERR_BADHANDLE
	case errors.ErrNameTooLong:
		return NFS4ERR_NAMETOOLONG
	case errors.ErrLocked:
		return NFS4ERR_LOCKED
	case errors.ErrDeadlock:
		return NFS4ERR_DEADLOCK
	case errors.ErrGracePeriod:
		return NFS4ERR_GRACE
	case errors.ErrIOError:
		return NFS4ERR_IO
	default:
		return NFS4ERR_SERVERFAULT
	}
}

// ValidateUTF8Filename validates an NFSv4 filename component per RFC 7530 Section 12.7.
//
// NFSv4 requires UTF-8 encoded filenames. This function validates a single
// path component (not a full path) and returns the appropriate NFS4 error code.
//
// Returns:
//   - NFS4_OK if the filename is valid
//   - NFS4ERR_INVAL if the filename is empty
//   - NFS4ERR_BADCHAR if the filename contains invalid UTF-8 or null bytes
//   - NFS4ERR_BADNAME if the filename contains path separators ('/')
//   - NFS4ERR_NAMETOOLONG if the filename exceeds 255 bytes
func ValidateUTF8Filename(name string) uint32 {
	// Empty filename is invalid
	if len(name) == 0 {
		return NFS4ERR_INVAL
	}

	// Check valid UTF-8 encoding
	if !utf8.ValidString(name) {
		return NFS4ERR_BADCHAR
	}

	// Check for null bytes (not caught by ValidString since null is valid UTF-8)
	if strings.ContainsRune(name, 0) {
		return NFS4ERR_BADCHAR
	}

	// Path separators are not allowed in component names
	if strings.ContainsRune(name, '/') {
		return NFS4ERR_BADNAME
	}

	// Filename component length limit (255 bytes per POSIX/RFC convention)
	if len(name) > 255 {
		return NFS4ERR_NAMETOOLONG
	}

	return NFS4_OK
}

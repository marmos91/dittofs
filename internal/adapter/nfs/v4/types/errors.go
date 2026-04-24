package types

import (
	"strings"
	"unicode/utf8"
)

// Note (ADAPT-03, D-06/D-07): MapMetadataErrorToNFS4 was removed as part of
// consolidating every metadata.ErrorCode -> protocol-code translator into
// internal/adapter/common/errmap.go. NFSv4 handlers now call
// common.MapToNFS4(err) directly. Keeping a wrapper here would have created
// an import cycle (internal/adapter/common imports internal/adapter/nfs/v4/types
// for the NFS4ERR_* constants), so the cleanest resolution is to delete the
// wrapper and migrate callers. The coverage test for every ErrorCode lives
// in internal/adapter/common/errmap_test.go.

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

package types

import (
	"strings"
	"unicode/utf8"
)

// The metadata.ErrorCode -> NFS4 status translation lives in StatusFor
// (statusfor.go, same package): every ErrorCode is mapped there, with a
// full-enum walk test in statusfor_test.go.

// ValidateUTF8Filename validates an NFSv4 filename component per RFC 7530 Section 12.7.
//
// NFSv4 requires UTF-8 encoded filenames. It validates a single
// path component (not a full path) and returns the appropriate NFS4 error code.
//
// Returns:
//   - NFS4_OK if the filename is valid
//   - NFS4ERR_INVAL if the filename is empty
//   - NFS4ERR_BADCHAR if the filename contains invalid UTF-8 or null bytes
//   - NFS4ERR_BADNAME if the filename contains path separators ('/'), or is "." or ".."
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

	// "." and ".." are directory-relative references, not names a client may
	// pass as a component: the server must never resolve or create them.
	if name == "." || name == ".." {
		return NFS4ERR_BADNAME
	}

	// Filename component length limit (255 bytes per POSIX/RFC convention)
	if len(name) > 255 {
		return NFS4ERR_NAMETOOLONG
	}

	return NFS4_OK
}

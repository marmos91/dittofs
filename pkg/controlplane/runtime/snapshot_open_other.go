//go:build !windows

package runtime

import "os"

// openManifestShared is plain os.Open everywhere but Windows: POSIX already
// lets a file be renamed or unlinked while held open for read, so there is no
// sharing-violation class to guard against (see the Windows variant for the
// rationale behind the explicit share mode there).
func openManifestShared(path string) (*os.File, error) {
	return os.Open(path)
}

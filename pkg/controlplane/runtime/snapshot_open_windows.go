//go:build windows

package runtime

import (
	"os"

	"golang.org/x/sys/windows"
)

// openManifestShared opens path read-only with the full Windows sharing set
// (FILE_SHARE_READ|WRITE|DELETE). Bare os.Open omits FILE_SHARE_DELETE, so a
// read handle held over a manifest blocks — and collides with — a concurrent
// snapshot create's atomic rename (MoveFileEx replace) or a delete's RemoveAll
// on the same path, surfacing ERROR_SHARING_VIOLATION (#1332). Granting
// FILE_SHARE_DELETE lets the rename/unlink proceed against the open handle
// (POSIX-like semantics: the reader keeps reading the now-detached file),
// eliminating the collision class instead of merely retrying through it.
//
// fs.ErrNotExist still propagates unchanged: CreateFile maps a missing path to
// ERROR_FILE_NOT_FOUND/ERROR_PATH_NOT_FOUND, which windows.Errno (a
// syscall.Errno) reports as os.ErrNotExist via errors.Is.
func openManifestShared(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(
		p,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

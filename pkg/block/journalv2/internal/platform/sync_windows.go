package platform

import (
	"fmt"
	"io/fs"

	"golang.org/x/sys/windows"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// Sync is FlushFileBuffers on the file's own handle (RFC 28 Appendix A:
// "`FlushFileBuffers` is the sync"). The handle is reached through the
// file's own reference, so a concurrent Close cannot release it during the
// call, and a closed file fails before any system call. A failed flush is
// returned once and never made again: Windows has no interrupted call to
// repeat, and RFC 1 §5.2 says a failure to sync "**MUST NOT** be recorded as
// success and retried silently".
func (f *file) Sync() error {
	rc, err := f.f.SyscallConn()
	if err != nil {
		return &fs.PathError{
			Op:   "FlushFileBuffers",
			Path: f.f.Name(),
			Err:  err,
		}
	}

	// The only error Control returns for a non-nil *os.File is the one for a
	// closed file, so it is reported as fs.ErrClosed, the form the file's
	// other methods return. It is a misuse, not a failed sync.
	var serr error

	if err := rc.Control(func(fd uintptr) {
		serr = f.calls.flushFileBuffers(windows.Handle(fd))
	}); err != nil {
		return &fs.PathError{
			Op:   "FlushFileBuffers",
			Path: f.f.Name(),
			Err:  fs.ErrClosed,
		}
	}

	if serr != nil {
		return &fs.PathError{
			Op:   "FlushFileBuffers",
			Path: f.f.Name(),
			Err:  serr,
		}
	}

	return nil
}

// SyncDir is not implemented yet: the directory flush of RFC 28 Appendix A
// is still to be written.
func (o *OS) SyncDir() error {
	return &fs.PathError{
		Op:   "open",
		Path: o.dir,
		Err:  fmt.Errorf("%w: directory flush", core.ErrNotImplemented),
	}
}

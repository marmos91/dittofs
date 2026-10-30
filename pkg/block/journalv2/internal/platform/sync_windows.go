package platform

import (
	"errors"
	"io/fs"

	"golang.org/x/sys/windows"
)

const (
	// dirAccess is what SyncDir opens the directory for: FlushFileBuffers
	// needs a handle with write access.
	dirAccess = windows.GENERIC_WRITE

	// dirShare lets SyncDir's handle refuse no other open of the directory,
	// so a create, rename or remove in it is never refused while the
	// directory is being flushed.
	dirShare = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE
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

// SyncDir makes the directory's entries durable the way Windows can. There is
// no directory sync there: a create, rename or unlink is made durable by
// opening the directory with backup semantics, without which a directory
// does not open, and calling FlushFileBuffers on it (RFC 28 Appendix A: "a
// create, rename or unlink is made durable by opening the directory with
// backup semantics and calling `FlushFileBuffers` on it, which NTFS
// honours"). A new file needs it before its records are durable (RFC 28 §2.2:
// "Syncing a file makes its bytes durable, not its entry in the directory").
// SyncDir opens the directory its path names now and closes the handle before
// returning, whether the flush succeeded or not. It returns the one error
// that happened as itself, and both through errors.Join when the flush and
// the close both failed.
func (o *OS) SyncDir() error {
	p, err := windows.UTF16PtrFromString(o.dir)
	if err != nil {
		return &fs.PathError{
			Op:   "open",
			Path: o.dir,
			Err:  err,
		}
	}

	h, err := o.calls.createFile(
		p,
		dirAccess,
		dirShare,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return &fs.PathError{
			Op:   "open",
			Path: o.dir,
			Err:  err,
		}
	}

	var errs []error

	if err := o.calls.flushFileBuffers(h); err != nil {
		errs = append(errs, &fs.PathError{
			Op:   "FlushFileBuffers",
			Path: o.dir,
			Err:  err,
		})
	}

	// A close error after a good flush is reported too: a handle that does
	// not close was not the one the flush was meant for.
	if err := o.calls.closeHandle(h); err != nil {
		errs = append(errs, &fs.PathError{
			Op:   "CloseHandle",
			Path: o.dir,
			Err:  err,
		})
	}

	if len(errs) == 1 {
		return errs[0]
	}

	return errors.Join(errs...)
}

package platform

import (
	"errors"
	"io/fs"

	"golang.org/x/sys/unix"
)

// dirFlags opens the directory for SyncDir: read-only, refused if the path is
// not a directory, and not inherited by a child process. Not O_PATH, on whose
// descriptor fsync fails.
const dirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC

// Sync is fdatasync on the file's descriptor (RFC 28 Appendix A: "`fdatasync`
// on the file"): a data-only sync, which RFC 1 §5.2 finds enough on Linux, and
// never a range sync, which it does not. The descriptor is reached through
// the file's own reference, so a concurrent Close cannot release it during
// the call, and a closed file fails before any system call.
func (f *file) Sync() error {
	rc, err := f.f.SyscallConn()
	if err != nil {
		return &fs.PathError{
			Op:   "fdatasync",
			Path: f.f.Name(),
			Err:  err,
		}
	}

	// The only error Control returns for a non-nil *os.File is the one for a
	// closed file (os.rawConn.Control reaches internal/poll's FD.RawControl,
	// whose only failure is incref's errClosing), so it is reported as
	// fs.ErrClosed, the form the file's other methods return. It is a misuse,
	// not a failed sync.
	var serr error
	if err := rc.Control(func(fd uintptr) {
		serr = retryInterrupted(f.calls.fdatasync, int(fd))
	}); err != nil {
		return &fs.PathError{
			Op:   "fdatasync",
			Path: f.f.Name(),
			Err:  fs.ErrClosed,
		}
	}

	if serr != nil {
		return &fs.PathError{
			Op:   "fdatasync",
			Path: f.f.Name(),
			Err:  serr,
		}
	}

	return nil
}

// SyncDir is fsync on a descriptor of the directory (RFC 28 Appendix A: "and
// `fsync` on the directory's descriptor after a create, a rename or an
// unlink"). It opens the directory its path names now and closes it before
// returning, whether the fsync succeeded or not. It returns the one error
// that happened as itself, and both through errors.Join when the fsync and
// the close both failed.
func (o *OS) SyncDir() error {
	// An open interrupted by a signal is made again, as os.OpenFile does:
	// repeating an open hides no failed sync.
	fd, err := o.calls.open(o.dir, dirFlags, 0)
	for errors.Is(err, unix.EINTR) {
		fd, err = o.calls.open(o.dir, dirFlags, 0)
	}

	if err != nil {
		return &fs.PathError{
			Op:   "open",
			Path: o.dir,
			Err:  err,
		}
	}

	var errs []error
	if err := retryInterrupted(o.calls.fsync, fd); err != nil {
		errs = append(errs, &fs.PathError{
			Op:   "fsync",
			Path: o.dir,
			Err:  err,
		})
	}

	// The close is never repeated: Linux releases the descriptor even when
	// close reports EINTR, so a second close could close another file's. A
	// close error after a good fsync is reported too: a directory has no
	// flush, so close fails only with EBADF, which means the descriptor was
	// not ours and the fsync proved nothing.
	if err := o.calls.close(fd); err != nil {
		errs = append(errs, &fs.PathError{
			Op:   "close",
			Path: o.dir,
			Err:  err,
		})
	}

	if len(errs) == 1 {
		return errs[0]
	}

	return errors.Join(errs...)
}

// retryInterrupted makes the sync call on fd, again for as long as it returns
// EINTR, and returns the last call's result.
//
// decision: a sync interrupted by a signal is made again, as the os package's
// own File.Sync does; the golang.org/x/sys calls are raw and repeat nothing.
// Every other error is returned after one call, and what this returns is the
// repeated call's own result, so a failed sync is never recorded as success
// (RFC 1 §5.2). EINTR says a signal cut the call short, not that writeback
// failed: ext4 and XFS wait uninterruptibly and never return it from a sync,
// and FUSE and NFS return a writeback error they found before an EINTR from a
// later step. Returning EINTR as a failure would, by RFC 1 §5.3, end the
// segment when nothing failed. Withdraw it, and report EINTR as a failed
// sync, if a supported filesystem is found to return EINTR after consuming a
// writeback error.
func retryInterrupted(sync func(fd int) error, fd int) error {
	for {
		if err := sync(fd); !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

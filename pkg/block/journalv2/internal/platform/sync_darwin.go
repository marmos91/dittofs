package platform

import (
	"errors"
	"io/fs"

	"golang.org/x/sys/unix"
)

// dirFlags opens the directory for SyncDir: read-only, refused if the path is
// not a directory, and not inherited by a child process.
const dirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC

// Sync is fcntl with F_FULLFSYNC on the file's descriptor (RFC 28 Appendix A:
// "the journal uses `fcntl(F_FULLFSYNC)` for every sync"): on macOS a plain
// fsync leaves the data in the device's write cache, and RFC 1 §5.2 asks for
// the call that flushes it. The descriptor is reached through the file's own
// reference, so a concurrent Close cannot release it during the call, and a
// closed file fails before any system call.
//
// decision: a full sync the filesystem refuses, with ENOTSUP or any other
// error, is returned as a failed sync, never followed by a plain fsync as the
// os package's own File.Sync does on macOS, which is why the storage never
// calls File.Sync. A plain fsync leaves the record in the device's write
// cache, so its success would be recorded as a sync that survives power loss,
// which RFC 1 §5.2 forbids. Overturn it only if a filesystem RFC 28 Appendix A
// lists for macOS is found to refuse F_FULLFSYNC while its plain fsync
// flushes the device's cache.
func (f *file) Sync() error {
	rc, err := f.f.SyscallConn()
	if err != nil {
		return &fs.PathError{
			Op:   "F_FULLFSYNC",
			Path: f.f.Name(),
			Err:  err,
		}
	}

	// The only error Control returns for a non-nil *os.File is the one for a
	// closed file, so it is reported as fs.ErrClosed, the form the file's
	// other methods return. It is a misuse, not a failed sync.
	var serr error

	if err := rc.Control(func(fd uintptr) {
		serr = f.calls.fullSync(int(fd))
	}); err != nil {
		return &fs.PathError{
			Op:   "F_FULLFSYNC",
			Path: f.f.Name(),
			Err:  fs.ErrClosed,
		}
	}

	if serr != nil {
		return &fs.PathError{
			Op:   "F_FULLFSYNC",
			Path: f.f.Name(),
			Err:  serr,
		}
	}

	return nil
}

// SyncDir is fcntl with F_FULLFSYNC on a descriptor of the directory (RFC 28
// Appendix A: "for every sync, the directory's included"). It opens the
// directory its path names now and closes it before returning, whether the
// sync succeeded or not. It returns the one error that happened as itself,
// and both through errors.Join when the sync and the close both failed.
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

	if err := o.calls.fullSync(fd); err != nil {
		errs = append(errs, &fs.PathError{
			Op:   "F_FULLFSYNC",
			Path: o.dir,
			Err:  err,
		})
	}

	// The close is never repeated: a close that reported EINTR may already
	// have released the descriptor, so a second close could close another
	// file's. A close error after a good sync is reported too: the
	// descriptor may not have been ours, and the sync then proved nothing.
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

// fullSync makes fcntl with F_FULLFSYNC on fd, again for as long as it
// returns EINTR, and returns the last call's result.
//
// decision: a full sync interrupted by a signal is made again, as the os
// package's own File.Sync does on macOS; the golang.org/x/sys calls are raw
// and repeat nothing. Every other error is returned after one call, and what
// this returns is the repeated call's own result, so a failed sync is never
// recorded as success (RFC 1 §5.2). EINTR says a signal cut the call short,
// not that writeback failed. Whether APFS can return it after consuming a
// writeback error is not known: its source is not published. Withdraw this,
// and report EINTR as a failed sync, if it is found to.
func (c calls) fullSync(fd int) error {
	for {
		_, err := c.fcntl(uintptr(fd), unix.F_FULLFSYNC, 0)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

package platform

import (
	"errors"
	"io/fs"
	"math"

	"golang.org/x/sys/unix"
)

// Allocate is fcntl with F_PREALLOCATE over the range, then the file's size
// set to the range's end (RFC 28 Appendix A: "`fcntl(F_PREALLOCATE)` then
// setting the size on macOS"). F_ALLOCATEALL asks for all of the space or
// none. The space is asked for from the physical end of the file, the only
// mode that takes a length in the file rather than a position on the volume,
// and the length asked for is the range's end, so the space reaches it
// whatever the file already holds. A file that already holds blocks may get
// more than it needs; the journal allocates only new, empty files.
//
// No zeros are written, as RFC 28 Appendix A asks on macOS: bytes past the
// old end read as zeros once the size is set, and bytes a write had put
// inside the range stay. It does not sync: the caller's Sync makes the
// allocation durable. A range no file can hold fails with EINVAL, and an
// empty range does nothing, both before any system call. The descriptor is
// reached through the file's own reference, so a concurrent Close cannot
// release it during the calls. On a closed file a valid, non-empty range
// fails with fs.ErrClosed before any system call.
//
// decision: the size is set only when the file ends before the range's end,
// so Allocate never shrinks a file: cutting it would drop the records written
// past the range. A write past the range's end made between the fstat that
// reads the size and the ftruncate that sets it could be cut; the journal
// never writes past the end of an allocated segment. Two Allocate calls on the
// same file at once could likewise cut the larger one's size; the journal
// never does it, as it allocates each segment once, when it creates it
// (RFC 28 §2.2). Overturn it if a caller ever needs Allocate to set an exact
// size.
func (f *file) Allocate(off, n int64) error {
	if off < 0 || n < 0 || off > math.MaxInt64-n {
		return &fs.PathError{
			Op:   "F_PREALLOCATE",
			Path: f.f.Name(),
			Err:  unix.EINVAL,
		}
	}

	if n == 0 {
		return nil
	}

	rc, err := f.f.SyscallConn()
	if err != nil {
		return &fs.PathError{
			Op:   "F_PREALLOCATE",
			Path: f.f.Name(),
			Err:  err,
		}
	}

	// As in Sync, Control fails only on a closed file.
	var op string
	var aerr error

	if err := rc.Control(func(fd uintptr) {
		op, aerr = f.allocate(int(fd), off+n)
	}); err != nil {
		return &fs.PathError{
			Op:   "F_PREALLOCATE",
			Path: f.f.Name(),
			Err:  fs.ErrClosed,
		}
	}

	if aerr != nil {
		return &fs.PathError{
			Op:   op,
			Path: f.f.Name(),
			Err:  aerr,
		}
	}

	return nil
}

// allocate makes Allocate's calls on fd for a range ending at end, and
// returns the name of the call that failed and its error, if one did. A call
// interrupted by a signal is made again, as the os package does for its own
// fstat and ftruncate: each gives the same result when repeated, so a repeat
// hides no failure. Every other error is returned after one call, and no
// call follows it.
func (f *file) allocate(fd int, end int64) (string, error) {
	// The kernel refuses any offset but 0 when the space is asked for from
	// the physical end of the file.
	store := unix.Fstore_t{
		Flags:   unix.F_ALLOCATEALL,
		Posmode: unix.F_PEOFPOSMODE,
		Offset:  0,
		Length:  end,
	}

	err := f.calls.fcntlFstore(uintptr(fd), unix.F_PREALLOCATE, &store)
	for errors.Is(err, unix.EINTR) {
		err = f.calls.fcntlFstore(uintptr(fd), unix.F_PREALLOCATE, &store)
	}

	if err != nil {
		return "F_PREALLOCATE", err
	}

	var st unix.Stat_t

	err = f.calls.fstat(fd, &st)
	for errors.Is(err, unix.EINTR) {
		err = f.calls.fstat(fd, &st)
	}

	if err != nil {
		return "fstat", err
	}

	if st.Size >= end {
		return "", nil
	}

	err = f.calls.ftruncate(fd, end)
	for errors.Is(err, unix.EINTR) {
		err = f.calls.ftruncate(fd, end)
	}

	if err != nil {
		return "ftruncate", err
	}

	return "", nil
}

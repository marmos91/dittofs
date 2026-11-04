//go:build linux || darwin

package platform

import (
	"errors"

	"golang.org/x/sys/unix"
)

// retryInterrupted makes the call on fd, again for as long as it returns
// EINTR, and returns the last call's result. Every sync goes through it on
// Linux and on macOS, and so do macOS's allocation calls, F_PREALLOCATE,
// fstat and ftruncate, which give the same result when repeated.
//
// decision: a call interrupted by a signal is made again, as the os package's
// own File.Sync does on both systems; the golang.org/x/sys calls are raw and
// repeat nothing. Every other error is returned after one call, and what this
// returns is the repeated call's own result, so a failed sync is never
// recorded as success (RFC 1 §5.2). EINTR says a signal cut the call short,
// not that writeback failed: ext4 and XFS wait uninterruptibly and never
// return it from a sync, and FUSE and NFS return a writeback error they found
// before an EINTR from a later step. Whether APFS can return EINTR after
// consuming a writeback error is not known: its source is not published.
// Returning EINTR as a failure would, by RFC 1 §5.3, end the segment when
// nothing failed. Withdraw it, and report EINTR as a failed sync, if a
// supported filesystem, APFS included, is found to return EINTR after
// consuming a writeback error.
func retryInterrupted(call func(fd int) error, fd int) error {
	for {
		if err := call(fd); !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

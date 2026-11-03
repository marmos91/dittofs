package platform

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

// diskFull is a full volume as Windows reports it, ERROR_DISK_FULL or
// ERROR_HANDLE_DISK_FULL, which errors.Is also matches with syscall.ENOSPC.
//
// decision: the core NoSpaceError decision says a full filesystem surfaces
// as the platform's *fs.PathError wrapping syscall.ENOSPC, never as a
// NoSpaceError. On Windows syscall.ENOSPC is a code Go invents and no call
// returns, and syscall.Errno's Is does not match either Windows code with
// it, so the platform maps them here, on every error a call through the
// table returns: the opens, writes, size reads and sets, flushes and the
// directory's close. The error still unwraps to the code Windows returned,
// so neither the code nor its message is lost. Rename, Remove, List, ReadAt
// and Close go through the os package and keep its errors: map Rename's too
// if NTFS refuses a rename for a full volume, since RFC 28 §2.2 answers a
// spare that failed with ENOSPC as a full device.
type diskFull struct {
	errno syscall.Errno
}

func (e diskFull) Error() string {
	return e.errno.Error()
}

func (e diskFull) Unwrap() error {
	return e.errno
}

func (e diskFull) Is(target error) bool {
	return target == syscall.ENOSPC
}

// fullVolume returns err as a diskFull when it is one of the two codes
// Windows reports a full volume with, and err unchanged otherwise.
func fullVolume(err error) error {
	var errno syscall.Errno

	if !errors.As(err, &errno) {
		return err
	}

	if errno != windows.ERROR_DISK_FULL && errno != windows.ERROR_HANDLE_DISK_FULL {
		return err
	}

	return diskFull{
		errno: errno,
	}
}

package platform

import (
	"fmt"
	"io/fs"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// Allocate is not implemented yet: allocation on the operating system
// (RFC 28 Appendix A, "Allocation, per platform") is still to be written. A
// closed file still fails with fs.ErrClosed, found as Sync finds it: Control
// fails only on a closed file, and with an empty function it makes no system
// call.
func (f *file) Allocate(off, n int64) error {
	rc, err := f.f.SyscallConn()
	if err != nil {
		return &fs.PathError{
			Op:   "allocate",
			Path: f.f.Name(),
			Err:  err,
		}
	}

	if err := rc.Control(func(uintptr) {}); err != nil {
		return &fs.PathError{
			Op:   "allocate",
			Path: f.f.Name(),
			Err:  fs.ErrClosed,
		}
	}

	return &fs.PathError{
		Op:   "allocate",
		Path: f.f.Name(),
		Err:  fmt.Errorf("%w: allocation", core.ErrNotImplemented),
	}
}

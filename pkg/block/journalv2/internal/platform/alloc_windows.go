package platform

import (
	"io"
	"io/fs"
	"math"
	"unsafe"

	"golang.org/x/sys/windows"
)

// zeroChunk is the most one zero write covers.
const zeroChunk = 1 << 20

// zeros is the one buffer every zero write reads from. Nothing writes to it,
// so concurrent Allocate calls share it, and a write allocates nothing.
var zeros [zeroChunk]byte

// Allocate allocates the range and writes zeros over every byte of it. It
// reads the file's size first and, only when the file ends below the range,
// sets the allocation size and then the end of file to the range's end (RFC
// 28 Appendix A: "setting the file's allocation size, then its end of file"),
// so it never shrinks a longer file. Then it writes zeros over the whole
// range, in writes of at most 1 MiB, so bytes a write had put there read as
// zeros afterwards.
//
// It does not sync: the storage.File contract leaves that to the caller
// ("Sync makes every WriteAt, Allocate and Truncate that returned before it
// durable"). A range no file can hold fails with ERROR_INVALID_PARAMETER, and
// an empty range does nothing, both before any system call. Every call is
// made on the handle inside Control, so a concurrent Close cannot release it
// during the calls, and on a closed file a valid, non-empty range fails with
// fs.ErrClosed before any call. A failed call is returned once and no call
// follows it.
//
// decision: zeros are always written, and the valid data length is never set
// with SetFileValidData. That call skips zero filling, so the range would
// read whatever the disk's clusters held before, possibly another user's
// data, readable by any reader the segment's read sharing admits. It would
// break RFC 28 §2.2 ("An allocated region no record has reached reads as
// zeros.") and the zero slot rule of RFC 28 §3.3, which takes a header slot
// of zeros for a write that never landed. Writing the zeros also moves the
// valid data length to the end, which is what RFC 28 Appendix A sets it for:
// an append below it commits no filesystem metadata at each flush. The fill
// is paid only when the pool of spares grows, off the write path (RFC 28
// Appendix A). This differs from the current text of RFC 28 Appendix A,
// which sets the valid data length and writes zeros only where that call is
// not permitted. Revisit only if a measured fill cost on the write path
// outweighs exposing old bytes, which no setting makes safe.
func (f *file) Allocate(off, n int64) error {
	if off < 0 || n < 0 || off > math.MaxInt64-n {
		return &fs.PathError{
			Op:   "allocate",
			Path: f.f.Name(),
			Err:  windows.ERROR_INVALID_PARAMETER,
		}
	}

	if n == 0 {
		return nil
	}

	rc, err := f.f.SyscallConn()
	if err != nil {
		return &fs.PathError{
			Op:   "allocate",
			Path: f.f.Name(),
			Err:  err,
		}
	}

	// As in Sync, Control fails only on a closed file.
	var (
		op   string
		aerr error
	)

	if err := rc.Control(func(fd uintptr) {
		op, aerr = f.allocate(windows.Handle(fd), off, n)
	}); err != nil {
		return &fs.PathError{
			Op:   "allocate",
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

// allocate makes Allocate's calls on h, and returns the name of the call
// that failed and its error, if one did.
func (f *file) allocate(h windows.Handle, off, n int64) (string, error) {
	var info windows.ByHandleFileInformation

	if err := f.calls.getFileInformationByHandle(h, &info); err != nil {
		return "GetFileInformationByHandle", err
	}

	end := off + n

	if sizeOf(&info) < end {
		for _, class := range [...]uint32{windows.FileAllocationInfo, windows.FileEndOfFileInfo} {
			size := end

			err := f.calls.setFileInformationByHandle(
				h,
				class,
				(*byte)(unsafe.Pointer(&size)),
				uint32(unsafe.Sizeof(size)),
			)
			if err != nil {
				return "SetFileInformationByHandle", err
			}
		}
	}

	for off < end {
		o := windows.Overlapped{
			Offset:     uint32(off),
			OffsetHigh: uint32(off >> 32),
		}

		var done uint32

		if err := f.calls.writeFile(h, zeros[:min(end-off, zeroChunk)], &done, &o); err != nil {
			return "WriteFile", err
		}

		// A write that writes nothing and reports no error would repeat for
		// ever; the os package reports it the same way.
		if done == 0 {
			return "WriteFile", io.ErrUnexpectedEOF
		}

		off += int64(done)
	}

	return "", nil
}

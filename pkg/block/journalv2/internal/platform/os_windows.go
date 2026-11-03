package platform

import (
	"io"
	"io/fs"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/storage"
)

func (o *OS) Create(name string) (storage.File, error) {
	f, err := o.open(name, windows.CREATE_NEW)
	if err != nil {
		return nil, err
	}

	return f, nil
}

func (o *OS) Open(name string) (storage.File, error) {
	f, err := o.open(name, windows.OPEN_EXISTING)
	if err != nil {
		return nil, err
	}

	return f, nil
}

// open opens name for reading and writing, creating it or opening an
// existing file as disposition says. An existing name refused by CREATE_NEW
// matches fs.ErrExist, and a missing one refused by OPEN_EXISTING matches
// fs.ErrNotExist, as the os package's errors do.
//
// decision: the file is opened with FILE_SHARE_DELETE, which os.OpenFile does
// not set, so a segment can be renamed or removed while a reader still holds
// it (RFC 28 §2.2: "on platforms where an open file cannot be unlinked,
// segments **MUST** be opened with sharing that permits deletion"; RFC 28
// Appendix A: "segments are opened with `FILE_SHARE_DELETE`"). Without it
// Windows refuses both while any handle is open. Reads and writes are shared
// too, so a second handle on an open file is not refused. Withdraw the
// deletion sharing only if the journal never renames or removes a file it
// may hold open.
func (o *OS) open(name string, disposition uint32) (*file, error) {
	path := o.path(name)

	// ponytail: the path reaches CreateFile as it is, without the \\?\ prefix
	// the os package adds for Rename, Remove and List, so a path over MAX_PATH
	// fails here and in SyncDir while those accept it. Add the prefix when a
	// journal directory needs a longer path.
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &fs.PathError{
			Op:   "open",
			Path: path,
			Err:  err,
		}
	}

	h, err := o.calls.createFile(
		p,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		disposition,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, &fs.PathError{
			Op:   "open",
			Path: path,
			Err:  fullVolume(err),
		}
	}

	return o.newFile(os.NewFile(uintptr(h), path), name), nil
}

// maxWrite is the most one WriteFile asks for, since it counts its bytes in
// 32 bits.
const maxWrite = 1 << 30

// WriteAt writes p at off through the table, inside Control, so a concurrent
// Close cannot release the handle during the calls, and a closed file fails
// with fs.ErrClosed before any call. Each write starts at the offset its
// overlapped names. The handle is synchronous, so Windows still moves the
// file pointer to the end of each write; nothing here reads the pointer,
// since every read and write names its own offset. A short write continues
// where it stopped. A failed write is returned once, with the bytes written
// before it, and a full volume matches syscall.ENOSPC (see diskFull).
func (f *file) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, &fs.PathError{
			Op:   "WriteFile",
			Path: f.f.Name(),
			Err:  windows.ERROR_INVALID_PARAMETER,
		}
	}

	rc, err := f.f.SyscallConn()
	if err != nil {
		return 0, &fs.PathError{
			Op:   "WriteFile",
			Path: f.f.Name(),
			Err:  err,
		}
	}

	var (
		n    int
		werr error
	)

	if err := rc.Control(func(fd uintptr) {
		n, werr = f.write(windows.Handle(fd), p, off)
	}); err != nil {
		return 0, &fs.PathError{
			Op:   "WriteFile",
			Path: f.f.Name(),
			Err:  fs.ErrClosed,
		}
	}

	if werr != nil {
		return n, &fs.PathError{
			Op:   "WriteFile",
			Path: f.f.Name(),
			Err:  fullVolume(werr),
		}
	}

	return n, nil
}

// write makes WriteAt's calls on h and returns the bytes written and the
// error that stopped it, if one did.
func (f *file) write(h windows.Handle, p []byte, off int64) (int, error) {
	n := 0

	for n < len(p) {
		o := windows.Overlapped{
			Offset:     uint32(off),
			OffsetHigh: uint32(off >> 32),
		}

		var done uint32

		if err := f.calls.writeFile(h, p[n:min(len(p), n+maxWrite)], &done, &o); err != nil {
			return n, err
		}

		// A write that writes nothing and reports no error would repeat for
		// ever. It is io.ErrUnexpectedEOF, as os.File.WriteAt reports it on
		// Windows, where internal/poll's Pwrite returns that error for it.
		if done == 0 {
			return n, io.ErrUnexpectedEOF
		}

		n += int(done)
		off += int64(done)
	}

	return n, nil
}

// Truncate sets the end of file through the table, inside Control, as
// WriteAt writes. A full volume matches syscall.ENOSPC (see diskFull).
func (f *file) Truncate(size int64) error {
	if size < 0 {
		return &fs.PathError{
			Op:   "SetFileInformationByHandle",
			Path: f.f.Name(),
			Err:  windows.ERROR_INVALID_PARAMETER,
		}
	}

	rc, err := f.f.SyscallConn()
	if err != nil {
		return &fs.PathError{
			Op:   "SetFileInformationByHandle",
			Path: f.f.Name(),
			Err:  err,
		}
	}

	var serr error

	if err := rc.Control(func(fd uintptr) {
		serr = f.setSize(windows.Handle(fd), windows.FileEndOfFileInfo, size)
	}); err != nil {
		return &fs.PathError{
			Op:   "SetFileInformationByHandle",
			Path: f.f.Name(),
			Err:  fs.ErrClosed,
		}
	}

	if serr != nil {
		return &fs.PathError{
			Op:   "SetFileInformationByHandle",
			Path: f.f.Name(),
			Err:  fullVolume(serr),
		}
	}

	return nil
}

// setSize sets the size class names, the allocation size or the end of
// file, to size on h.
func (f *file) setSize(h windows.Handle, class uint32, size int64) error {
	return f.calls.setFileInformationByHandle(
		h,
		class,
		(*byte)(unsafe.Pointer(&size)),
		uint32(unsafe.Sizeof(size)),
	)
}

// Size reads the file's size on its own handle. It does not use
// os.File.Stat, which on Windows reads the handle without holding the file
// open and reports a closed file as an invalid handle: the read is made
// inside Control, so a concurrent Close cannot release the handle during
// it, and a closed file fails with fs.ErrClosed before any call.
func (f *file) Size() (int64, error) {
	rc, err := f.f.SyscallConn()
	if err != nil {
		return 0, &fs.PathError{
			Op:   "GetFileInformationByHandle",
			Path: f.f.Name(),
			Err:  err,
		}
	}

	var (
		info windows.ByHandleFileInformation
		serr error
	)

	if err := rc.Control(func(fd uintptr) {
		serr = f.calls.getFileInformationByHandle(windows.Handle(fd), &info)
	}); err != nil {
		return 0, &fs.PathError{
			Op:   "GetFileInformationByHandle",
			Path: f.f.Name(),
			Err:  fs.ErrClosed,
		}
	}

	if serr != nil {
		return 0, &fs.PathError{
			Op:   "GetFileInformationByHandle",
			Path: f.f.Name(),
			Err:  fullVolume(serr),
		}
	}

	return sizeOf(&info), nil
}

// sizeOf returns the file size info reports, from its two 32-bit halves.
func sizeOf(info *windows.ByHandleFileInformation) int64 {
	return int64(info.FileSizeHigh)<<32 | int64(info.FileSizeLow)
}

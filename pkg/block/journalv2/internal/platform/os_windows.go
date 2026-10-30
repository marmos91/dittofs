package platform

import (
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/storage"
)

// OS is a storage.FS over the directory dir, on the operating system's files.
type OS struct {
	dir   string
	calls calls
}

var _ storage.FS = (*OS)(nil)

// NewOS returns the storage over the existing directory dir.
func NewOS(dir string) *OS {
	return newOS(dir, realTable)
}

// newOS returns the storage over dir, making its system calls through c.
func newOS(dir string, c calls) *OS {
	return &OS{
		dir:   dir,
		calls: c,
	}
}

func (o *OS) path(name string) string {
	return filepath.Join(o.dir, name)
}

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
			Err:  err,
		}
	}

	return o.newFile(os.NewFile(uintptr(h), path), name), nil
}

func (o *OS) Rename(oldName, newName string) error {
	return os.Rename(o.path(oldName), o.path(newName))
}

func (o *OS) Remove(name string) error {
	return os.Remove(o.path(name))
}

func (o *OS) List() ([]string, error) {
	entries, err := os.ReadDir(o.dir)
	if err != nil {
		return nil, err
	}

	names := make([]string, len(entries))

	for i, e := range entries {
		names[i] = e.Name()
	}

	return names, nil
}

// file is one open file of an OS.
type file struct {
	f     *os.File
	name  string
	calls calls
}

var _ storage.File = (*file)(nil)

// newFile wraps f, opened under name, as a file of o.
func (o *OS) newFile(f *os.File, name string) *file {
	return &file{
		f:     f,
		name:  name,
		calls: o.calls,
	}
}

func (f *file) ReadAt(p []byte, off int64) (int, error) {
	return f.f.ReadAt(p, off)
}

// ponytail: reads through the page cache; O_DIRECT, F_NOCACHE and
// FILE_FLAG_NO_BUFFERING arrive with the open-time tail check, the first
// caller that must not trust the cache.
func (f *file) ReadAtDirect(p []byte, off int64) (int, error) {
	return f.ReadAt(p, off)
}

func (f *file) WriteAt(p []byte, off int64) (int, error) {
	return f.f.WriteAt(p, off)
}

func (f *file) Truncate(size int64) error {
	return f.f.Truncate(size)
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
			Err:  serr,
		}
	}

	return sizeOf(&info), nil
}

// sizeOf returns the file size info reports, from its two 32-bit halves.
func sizeOf(info *windows.ByHandleFileInformation) int64 {
	return int64(info.FileSizeHigh)<<32 | int64(info.FileSizeLow)
}

func (f *file) Close() error {
	return f.f.Close()
}

func (f *file) Name() string {
	return f.name
}

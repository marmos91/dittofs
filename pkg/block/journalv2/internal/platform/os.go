//go:build linux

package platform

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/storage"
)

// filePerm is the mode a created file is asked for, which the umask can only
// narrow: the journal holds its users' data, so only the process's own user
// reads it.
const filePerm = 0o600

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
	f, err := os.OpenFile(o.path(name), os.O_RDWR|os.O_CREATE|os.O_EXCL, filePerm)
	if err != nil {
		return nil, err
	}

	return o.newFile(f, name), nil
}

func (o *OS) Open(name string) (storage.File, error) {
	f, err := os.OpenFile(o.path(name), os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}

	return o.newFile(f, name), nil
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

func (f *file) Truncate(size int64) error {
	return f.f.Truncate(size)
}

func (f *file) Size() (int64, error) {
	fi, err := f.f.Stat()
	if err != nil {
		return 0, err
	}

	return fi.Size(), nil
}

func (f *file) Close() error {
	return f.f.Close()
}

func (f *file) Name() string {
	return f.name
}

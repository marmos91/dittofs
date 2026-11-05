//go:build linux || darwin || windows

package platform

import (
	"os"
	"path/filepath"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/storage"
)

// OS is a storage.FS over the directory dir, on the operating system's files.
// Create, Open, and a file's WriteAt, Truncate and Size are per system
// (os_unix.go, os_windows.go); the rest is shared here.
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

func (f *file) Close() error {
	return f.f.Close()
}

func (f *file) Name() string {
	return f.name
}

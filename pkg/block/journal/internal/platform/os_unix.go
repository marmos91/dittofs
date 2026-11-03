//go:build linux || darwin

package platform

import (
	"os"

	"github.com/marmos91/dittofs/pkg/block/journal/internal/storage"
)

// filePerm is the mode a created file is asked for, which the umask can only
// narrow: the journal holds its users' data, so only the process's own user
// reads it.
const filePerm = 0o600

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

func (f *file) WriteAt(p []byte, off int64) (int, error) {
	return f.f.WriteAt(p, off)
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

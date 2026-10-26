package journalv2test

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// openFile opens path for the seam. It takes only the two flag sets the seam
// uses, O_RDWR and O_RDWR|O_CREATE|O_EXCL, and refuses any other.
//
// decision: the file is opened with FILE_SHARE_DELETE, which os.OpenFile does
// not set, as the journal opens its segments on Windows. The seam renames and
// removes files it still holds open, to the stash and over other names, and
// without it Windows refuses both while any handle is open.
func openFile(path string, flag int, _ os.FileMode) (*os.File, error) {
	var disposition uint32
	switch flag {
	case os.O_RDWR:
		disposition = syscall.OPEN_EXISTING
	case os.O_RDWR | os.O_CREATE | os.O_EXCL:
		disposition = syscall.CREATE_NEW
	default:
		return nil, &fs.PathError{
			Op:   "open",
			Path: path,
			Err:  fmt.Errorf("journalv2test: unsupported open flags %#x", flag),
		}
	}

	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, &fs.PathError{
			Op:   "open",
			Path: path,
			Err:  err,
		}
	}

	h, err := syscall.CreateFile(p,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, disposition, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}

	return os.NewFile(uintptr(h), path), nil
}

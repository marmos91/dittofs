//go:build linux

package platform

import "golang.org/x/sys/unix"

// calls is the table of system calls the storage makes where the RFC names
// the call. Each entry is the golang.org/x/sys function itself, never a
// wrapper, and an argument the call takes as a command is passed as data, so a
// test that swaps the table sees each call with its arguments as the kernel
// would.
type calls struct {
	open           func(path string, mode int, perm uint32) (fd int, err error)
	fsync          func(fd int) error
	fdatasync      func(fd int) error
	close          func(fd int) error
	fallocate      func(fd int, mode uint32, off int64, n int64) error
	pwrite         func(fd int, p []byte, off int64) (n int, err error)
	fstatfs        func(fd int, buf *unix.Statfs_t) error
	fstat          func(fd int, stat *unix.Stat_t) error
	statx          func(dirfd int, path string, flags int, mask int, stat *unix.Statx_t) error
	ioctlGetUint32 func(fd int, req uint) (uint32, error)
	getdents       func(fd int, buf []byte) (n int, err error)
}

// realTable is the table NewOS gives every OS.
var realTable = calls{
	open:           unix.Open,
	fsync:          unix.Fsync,
	fdatasync:      unix.Fdatasync,
	close:          unix.Close,
	fallocate:      unix.Fallocate,
	pwrite:         unix.Pwrite,
	fstatfs:        unix.Fstatfs,
	fstat:          unix.Fstat,
	statx:          unix.Statx,
	getdents:       unix.Getdents,
	ioctlGetUint32: unix.IoctlGetUint32,
}

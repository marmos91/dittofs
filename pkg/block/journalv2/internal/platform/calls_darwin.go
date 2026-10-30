package platform

import "golang.org/x/sys/unix"

// calls is the table of system calls the storage makes where the RFC names
// the call. Each entry is the golang.org/x/sys function itself, never a
// wrapper, and an argument the call takes as a command is passed as data, so a
// test that swaps the table sees each call with its arguments as the kernel
// would. There is no fsync entry: on macOS every sync is fcntl with
// F_FULLFSYNC (RFC 28 Appendix A).
type calls struct {
	open        func(path string, mode int, perm uint32) (fd int, err error)
	fcntl       func(fd uintptr, cmd int, arg int) (int, error)
	fcntlFstore func(fd uintptr, cmd int, fstore *unix.Fstore_t) error
	fstat       func(fd int, stat *unix.Stat_t) error
	ftruncate   func(fd int, length int64) error
	close       func(fd int) error
}

// realTable is the table NewOS gives every OS.
var realTable = calls{
	open:        unix.Open,
	fcntl:       unix.FcntlInt,
	fcntlFstore: unix.FcntlFstore,
	fstat:       unix.Fstat,
	ftruncate:   unix.Ftruncate,
	close:       unix.Close,
}

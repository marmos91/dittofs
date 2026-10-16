// Package platform implements the storage seam on the operating system's file
// interface: durable sync of files and directories, space allocation, reads
// that bypass the page cache, and detection of copy-on-write filesystems.
//
// decision: this is the only package of the journal that may use
// golang.org/x/sys; every other package uses the standard library alone. The
// calls a durable journal needs, such as fdatasync, fallocate, F_FULLFSYNC and
// O_DIRECT, belong to the operating system's file interface the journal is
// allowed, and the syscall package covers them incompletely and is frozen.
// The import test holds the split per package.
package platform

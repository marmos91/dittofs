// Package platform implements the storage seam on the operating system's file
// interface: durable sync of files and directories, space allocation, reads
// that bypass the page cache, and detection of copy-on-write filesystems.
//
// It is the only package of the journal that may use golang.org/x/sys; every
// other package uses the standard library alone.
package platform

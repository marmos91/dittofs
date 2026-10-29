// Package platform is the journal's storage on the operating system's own
// files: the storage.FS the journal uses when no other is given, with each
// sync made by the call RFC 28 Appendix A names for the platform.
//
// decision: golang.org/x/sys is imported here and nowhere else in the journal,
// which otherwise depends on the standard library alone. The calls a sync
// needs (fdatasync, and later fallocate, F_FULLFSYNC and O_DIRECT) are the
// operating system's file interface RFC 1 §1.1 allows, and the syscall package
// covers them incompletely and is frozen. The import test holds the split per
// package. Widen it only if another package needs a call the standard library
// lacks.
package platform

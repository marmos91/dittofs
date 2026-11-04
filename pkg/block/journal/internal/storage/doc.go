// Package storage declares the seam through which the journal reaches its
// directory and files: create, open, allocate, write at an offset, read, read
// bypassing the page cache, sync, truncate, close, rename, unlink and sync the
// directory. The journal performs no file operation outside it.
//
// The seam exists so that a test can wrap the real files and simulate a crash
// beneath the journal, discarding exactly what was not yet synced, and can fail
// any single call. The filesystem underneath is always real.
package storage

package badgerkv

import (
	"os"

	"golang.org/x/sys/unix"
)

// datasync syncs f's data and the metadata needed to read it back, not its
// times, as Badger's own msync of its mapped files does.
func datasync(f *os.File) error { return unix.Fdatasync(int(f.Fd())) }

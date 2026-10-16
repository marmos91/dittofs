//go:build !linux

package badgerkv

import "os"

func datasync(f *os.File) error { return f.Sync() }

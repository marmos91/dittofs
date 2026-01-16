//go:build windows

// mmap_windows.go provides a stub for Windows where mmap persistence is not supported.

package wal

// MmapPersister is not supported on Windows.
// Use in-memory cache only or run on a Unix-like system for WAL persistence.
type MmapPersister struct{}

// NewMmapPersister returns an error on Windows as mmap persistence is not supported.
func NewMmapPersister(_ string) (*MmapPersister, error) {
	return nil, ErrUnsupportedPlatform
}

// AppendSlice is not supported on Windows.
func (p *MmapPersister) AppendSlice(_ *SliceEntry) error {
	return ErrUnsupportedPlatform
}

// AppendRemove is not supported on Windows.
func (p *MmapPersister) AppendRemove(_ string) error {
	return ErrUnsupportedPlatform
}

// Sync is not supported on Windows.
func (p *MmapPersister) Sync() error {
	return ErrUnsupportedPlatform
}

// Recover is not supported on Windows.
func (p *MmapPersister) Recover() ([]SliceEntry, error) {
	return nil, ErrUnsupportedPlatform
}

// Close is a no-op on Windows.
func (p *MmapPersister) Close() error {
	return nil
}

// IsEnabled returns false on Windows.
func (p *MmapPersister) IsEnabled() bool {
	return false
}

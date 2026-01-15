package wal

import (
	"errors"
)

// WAL errors
var (
	// ErrPersisterClosed is returned when operations are attempted on a closed persister.
	ErrPersisterClosed = errors.New("persister is closed")

	// ErrCorrupted is returned when the WAL file is corrupted.
	ErrCorrupted = errors.New("WAL file corrupted")

	// ErrVersionMismatch is returned when the WAL file version doesn't match.
	ErrVersionMismatch = errors.New("WAL file version mismatch")
)

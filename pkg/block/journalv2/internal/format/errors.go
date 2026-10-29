package format

import "errors"

var (
	// ErrDamaged is matched by a decode error when the bytes are not a
	// structure of a version this binary reads: a wrong length, magic or
	// checksum, or reserved bytes that are not zero.
	ErrDamaged = errors.New("format: damaged")

	// ErrUnknownVersion is matched by a decode error when the structure
	// names a version this binary does not recognise.
	ErrUnknownVersion = errors.New("format: unknown version")
)

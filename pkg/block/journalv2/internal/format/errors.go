package format

import "errors"

var (
	// ErrDamaged is matched by a decode error when the bytes are not a
	// structure of a version this binary reads: a wrong length, magic or
	// checksum, or a field the table forbids, such as reserved bytes that
	// are not zero or a segment id of zero.
	ErrDamaged = errors.New("format: damaged")

	// ErrUnknownVersion is matched by a decode error when the structure
	// names a version this binary does not recognise.
	ErrUnknownVersion = errors.New("format: unknown version")

	// ErrForeign is matched by a decode error when the structure verifies
	// but names another journal's identity: it is that journal's, not
	// damage.
	ErrForeign = errors.New("format: foreign")

	// ErrWrongPlace is matched by a decode error when the structure
	// verifies but names a segment or an own offset other than the ones
	// it was read at: it was written somewhere else, so it is not this
	// place's.
	ErrWrongPlace = errors.New("format: wrong place")
)

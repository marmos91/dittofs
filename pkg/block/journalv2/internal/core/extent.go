package core

// Extent is a byte range of one file: Len bytes starting at file offset Off.
// It says which bytes of the file, never where in storage they sit nor what
// content they hold. Off is never negative, and an extent with Len zero covers
// no byte.
type Extent struct {
	Off int64
	Len int64
}

// End returns the offset one past the extent's last byte, Off + Len.
func (e Extent) End() int64 {
	return 0
}

// Overlaps reports whether e and f share at least one byte. An extent with Len
// zero overlaps nothing, itself included.
func (e Extent) Overlaps(f Extent) bool {
	return false
}

// Held names the content version of the bytes a read served for one extent.
// Whether that version is current anywhere other than in the journal is the
// caller's question.
type Held struct {
	Extent  Extent
	Version Version
}

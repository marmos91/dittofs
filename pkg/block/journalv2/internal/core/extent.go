package core

// Extent is a byte range of one file: Len bytes starting at file offset Off.
// It says which bytes of the file, never where in storage they sit nor what
// content they hold. Off is never negative, and an extent with Len zero covers
// no byte.
type Extent struct {
	Off int64
	Len int64
}

// Held names the content version of the bytes a read served for one extent.
// Whether that version is current anywhere other than in the journal is the
// caller's question.
type Held struct {
	Extent  Extent
	Version Version
}

package core

// Version is the content version of a file's bytes: which bytes these are, and
// which of two is newer. It is 128 bits, an epoch half then a counter half,
// and two versions are compared as one unsigned 128-bit number, epoch first.
//
// The journal issues the counter half from one counter per journal, and every
// version it issues exceeds every version on disk and the floor it was opened
// with. In this format the epoch half is zero: a version with a non-zero epoch
// is refused wherever one is decoded, on disk or in a floor. The zero Version
// precedes every version the journal issues.
//
// A version keeps its meaning outside the journal that issued it, in metadata
// and on another journal, which is why it is not a sequence number.
type Version struct {
	Epoch   uint64
	Counter uint64
}

// Compare returns -1 if v precedes w, 0 if they are equal and +1 if v follows
// w, comparing the two as unsigned 128-bit numbers, epoch half first.
func (v Version) Compare(w Version) int {
	return 0
}

// Less reports whether v precedes w; it is Compare(w) < 0.
func (v Version) Less(w Version) bool {
	return false
}

// IsZero reports whether both halves of v are zero.
func (v Version) IsZero() bool {
	return false
}

// Bytes returns v as 16 bytes: the epoch half then the counter half, each
// big-endian, so that comparing two encodings byte by byte orders them as
// Compare does. This is the encoding a version is stored under outside the
// journal. The journal's own on-disk records encode integers little-endian,
// and that encoding belongs to the format codecs, not to this method.
func (v Version) Bytes() [16]byte {
	return [16]byte{}
}

// VersionFromBytes is the inverse of Version.Bytes: for every v,
// VersionFromBytes(v.Bytes()) == v. It accepts any 16 bytes; refusing a
// non-zero epoch is the decoder's job, not this conversion's.
func VersionFromBytes(b [16]byte) Version {
	return Version{}
}

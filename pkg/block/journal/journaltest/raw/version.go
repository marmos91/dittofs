package raw

// Version is a content version: the epoch, then the counter. On disk it is
// two little-endian 64-bit integers in that order, not one 128-bit integer.
type Version struct {
	Epoch, Counter uint64
}

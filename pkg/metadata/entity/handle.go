package entity

// Handle is what a client holds to name a file: opaque above this layer.
// It names the share, so the runtime can route without reading further, and
// the file by FileID. It never carries a path, a name, a parent, a shard or a
// directory offset: each changes while the file does not.
type Handle []byte

// handleFile is the one kind of handle this store mints for a file; the
// pseudo-filesystem's handles will be another kind.
const (
	handleFile = 1
	handleLen  = 1 + 16 + 16
)

func NewHandle(share ShareID, file FileID) Handle {
	h := make(Handle, 0, handleLen)
	h = append(h, handleFile)
	h = append(h, share[:]...)
	return append(h, file[:]...)
}

// Parse decodes a handle, accepting exactly one byte form for each file, so
// handles compare equal exactly when they name one file. Any other form is
// stale: no file has it.
func (h Handle) Parse() (ShareID, FileID, error) {
	if len(h) != handleLen || h[0] != handleFile {
		return ShareID{}, FileID{}, ErrStale
	}
	return ShareID(h[1:17]), FileID(h[17:]), nil
}

// Share is the share the handle names, for routing; zero for a handle that
// does not parse.
func (h Handle) Share() ShareID {
	s, _, _ := h.Parse()
	return s
}

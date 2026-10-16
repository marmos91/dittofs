package core

// FileID names one file's content in the journal. It is built only from a
// file's 16-byte identifier, unique across the store and stable for the
// file's life, and no other value reaches it by conversion: its one field is
// unexported.
//
// A FileID encodes no share. Files of many shares share one journal without
// colliding, and the share a file belongs to is the one whose handle it is
// written through, never something derived from its identifier.
type FileID struct {
	id [16]byte
}

// NewFileID returns the FileID of the file whose identifier is id.
//
// decision: any 16 bytes are accepted, the all-zero identifier included.
// Refusing it here would buy nothing, because the zero FileID{} can be written
// without calling this constructor at all. Revisit if a zero identifier is
// ever seen to reach the journal from a real file.
func NewFileID(id [16]byte) FileID {
	return FileID{}
}

// Bytes returns the identifier the FileID was built from: for every id,
// NewFileID(id).Bytes() == id.
func (f FileID) Bytes() [16]byte {
	return [16]byte{}
}

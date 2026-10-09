// Package codec is the frame every persisted value uses: one version byte,
// then the fields in a fixed order. The version byte is the only codec-version
// mechanism, so old and new values sit side by side and each decodes by its
// own byte. Decoding refuses an unknown version, a truncated value and a
// trailing byte with named errors, never a zero value.
package codec

import (
	"encoding/binary"
	"errors"
	"fmt"
)

var (
	ErrVersion   = errors.New("codec: unknown version")
	ErrTruncated = errors.New("codec: truncated value")
	ErrTrailing  = errors.New("codec: trailing bytes")
)

// Writer builds one value.
type Writer struct{ b []byte }

// NewWriter starts a value of the given codec version.
func NewWriter(version byte) *Writer { return &Writer{b: []byte{version}} }

func (w *Writer) Uint64(v uint64) *Writer {
	w.b = binary.BigEndian.AppendUint64(w.b, v)
	return w
}

// Bytes writes v with its length in front.
func (w *Writer) Bytes(v []byte) *Writer {
	w.b = binary.AppendUvarint(w.b, uint64(len(v)))
	w.b = append(w.b, v...)
	return w
}

func (w *Writer) Encode() []byte { return w.b }

// Reader decodes one value. The first error sticks: every later read returns a
// zero, and Done reports the error.
type Reader struct {
	b   []byte
	err error
}

// NewReader opens a value and returns its version, refusing one not in
// known.
func NewReader(b []byte, known ...byte) (*Reader, byte, error) {
	if len(b) == 0 {
		return nil, 0, ErrTruncated
	}
	for _, v := range known {
		if b[0] == v {
			return &Reader{b: b[1:]}, v, nil
		}
	}
	return nil, 0, fmt.Errorf("%w %d", ErrVersion, b[0])
}

func (r *Reader) Uint64() uint64 {
	if r.err != nil || len(r.b) < 8 {
		r.fail()
		return 0
	}
	v := binary.BigEndian.Uint64(r.b)
	r.b = r.b[8:]
	return v
}

// Bytes reads a length-prefixed field, sharing the value's bytes.
func (r *Reader) Bytes() []byte {
	if r.err != nil {
		return nil
	}
	n, k := binary.Uvarint(r.b)
	if k <= 0 || uint64(len(r.b)-k) < n {
		r.fail()
		return nil
	}
	v := r.b[k : k+int(n) : k+int(n)]
	r.b = r.b[k+int(n):]
	return v
}

// Done reports the first error, or ErrTrailing if bytes remain.
func (r *Reader) Done() error {
	if r.err == nil && len(r.b) > 0 {
		return ErrTrailing
	}
	return r.err
}

func (r *Reader) fail() {
	if r.err == nil {
		r.err = ErrTruncated
	}
}

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

// Int64 writes a signed integer as its two's complement, in 8 bytes.
func (w *Writer) Int64(v int64) *Writer { return w.Uint64(uint64(v)) }

// Bytes writes v with its length in front.
func (w *Writer) Bytes(v []byte) *Writer {
	w.b = binary.AppendUvarint(w.b, uint64(len(v)))
	w.b = append(w.b, v...)
	return w
}

func (w *Writer) Uint32(v uint32) *Writer {
	w.b = binary.BigEndian.AppendUint32(w.b, v)
	return w
}

func (w *Writer) Uint8(v uint8) *Writer {
	w.b = append(w.b, v)
	return w
}

func (w *Writer) Bool(v bool) *Writer {
	if v {
		return w.Uint8(1)
	}
	return w.Uint8(0)
}

// String writes v with its length in front, as Bytes does.
func (w *Writer) String(v string) *Writer {
	w.b = binary.AppendUvarint(w.b, uint64(len(v)))
	w.b = append(w.b, v...)
	return w
}

// Fixed writes v raw: its length is the codec's to hold fixed.
func (w *Writer) Fixed(v []byte) *Writer {
	w.b = append(w.b, v...)
	return w
}

// Len writes the count of a list whose items follow.
func (w *Writer) Len(n int) *Writer {
	w.b = binary.AppendUvarint(w.b, uint64(n))
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

func (r *Reader) Int64() int64 { return int64(r.Uint64()) }

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

func (r *Reader) Uint32() uint32 {
	b := r.Fixed(4)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint32(b)
}

func (r *Reader) Uint8() uint8 {
	b := r.Fixed(1)
	if b == nil {
		return 0
	}
	return b[0]
}

// Bool refuses any byte but 0 and 1, so a value has one encoding.
func (r *Reader) Bool() bool {
	switch r.Uint8() {
	case 0:
		return false
	case 1:
		return true
	}
	r.fail()
	return false
}

func (r *Reader) String() string { return string(r.Bytes()) }

// Fixed reads n raw bytes, sharing the value's bytes.
func (r *Reader) Fixed(n int) []byte {
	if r.err != nil || len(r.b) < n {
		r.fail()
		return nil
	}
	v := r.b[:n:n]
	r.b = r.b[n:]
	return v
}

// Len reads a list's count. Every item takes at least one byte, so a count
// above the bytes left is refused as truncated rather than trusted to size an
// allocation.
func (r *Reader) Len() int {
	if r.err != nil {
		return 0
	}
	n, k := binary.Uvarint(r.b)
	if k <= 0 || uint64(len(r.b)-k) < n {
		r.fail()
		return 0
	}
	r.b = r.b[k:]
	return int(n)
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

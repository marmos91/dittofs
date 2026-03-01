package smbenc

import (
	"encoding/binary"
	"fmt"
)

// Writer provides sequential writing of little-endian encoded SMB wire data
// with append-based growth and pre-allocated capacity.
type Writer struct {
	buf []byte
	err error
}

// NewWriter creates a new Writer with the given initial capacity.
func NewWriter(capacity int) *Writer {
	return &Writer{
		buf: make([]byte, 0, capacity),
	}
}

// WriteUint8 appends a single byte.
func (w *Writer) WriteUint8(v uint8) {
	if w.err != nil {
		return
	}
	w.buf = append(w.buf, v)
}

// WriteUint16 appends a little-endian uint16.
func (w *Writer) WriteUint16(v uint16) {
	if w.err != nil {
		return
	}
	var b [2]byte
	binary.LittleEndian.PutUint16(b[:], v)
	w.buf = append(w.buf, b[:]...)
}

// WriteUint32 appends a little-endian uint32.
func (w *Writer) WriteUint32(v uint32) {
	if w.err != nil {
		return
	}
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	w.buf = append(w.buf, b[:]...)
}

// WriteUint64 appends a little-endian uint64.
func (w *Writer) WriteUint64(v uint64) {
	if w.err != nil {
		return
	}
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	w.buf = append(w.buf, b[:]...)
}

// WriteBytes appends raw bytes.
func (w *Writer) WriteBytes(data []byte) {
	if w.err != nil {
		return
	}
	w.buf = append(w.buf, data...)
}

// WriteZeros appends n zero bytes.
func (w *Writer) WriteZeros(n int) {
	if w.err != nil {
		return
	}
	w.buf = append(w.buf, make([]byte, n)...)
}

// Pad pads the buffer to the given alignment boundary by appending zero bytes.
// For example, Pad(8) pads to the next 8-byte boundary. If already aligned,
// no padding is added.
func (w *Writer) Pad(alignment int) {
	if w.err != nil {
		return
	}
	if alignment <= 0 {
		return
	}
	remainder := len(w.buf) % alignment
	if remainder == 0 {
		return
	}
	padding := alignment - remainder
	w.buf = append(w.buf, make([]byte, padding)...)
}

// WriteAt overwrites bytes at the specified offset. Used for backpatching
// offsets in negotiate context headers after the data length is known.
// Sets error if the write extends beyond the current buffer length.
func (w *Writer) WriteAt(offset int, data []byte) {
	if w.err != nil {
		return
	}
	if offset+len(data) > len(w.buf) {
		w.err = fmt.Errorf("smbenc: WriteAt out of bounds: offset %d + %d > %d", offset, len(data), len(w.buf))
		return
	}
	copy(w.buf[offset:], data)
}

// Bytes returns the accumulated bytes.
func (w *Writer) Bytes() []byte {
	return w.buf
}

// Len returns the current length of the buffer.
func (w *Writer) Len() int {
	return len(w.buf)
}

// Err returns the first error encountered, or nil.
func (w *Writer) Err() error {
	return w.err
}

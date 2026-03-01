package smbenc

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// ErrShortRead is returned when there are insufficient bytes to complete a read.
var ErrShortRead = errors.New("smbenc: short read")

// ErrExpectMismatch is returned when ExpectUint16 finds a different value than expected.
var ErrExpectMismatch = errors.New("smbenc: expect mismatch")

// Reader provides sequential reading of little-endian encoded SMB wire data
// with error accumulation. Once an error occurs, all subsequent reads become
// no-ops returning zero values.
type Reader struct {
	data []byte
	pos  int
	err  error
}

// NewReader creates a new Reader wrapping the given byte slice with position at 0.
func NewReader(data []byte) *Reader {
	return &Reader{data: data}
}

// require checks that n bytes are available at the current position.
// Returns false and sets the error if insufficient data remains.
func (r *Reader) require(n int) bool {
	if r.err != nil {
		return false
	}
	if r.pos+n > len(r.data) {
		r.err = fmt.Errorf("%w: need %d bytes at offset %d, have %d", ErrShortRead, n, r.pos, len(r.data)-r.pos)
		return false
	}
	return true
}

// ReadUint8 reads a single byte and advances the position by 1.
// Returns 0 and sets error on short read.
func (r *Reader) ReadUint8() uint8 {
	if !r.require(1) {
		return 0
	}
	v := r.data[r.pos]
	r.pos++
	return v
}

// ReadUint16 reads a little-endian uint16 and advances the position by 2.
// Returns 0 and sets error on short read.
func (r *Reader) ReadUint16() uint16 {
	if !r.require(2) {
		return 0
	}
	v := binary.LittleEndian.Uint16(r.data[r.pos:])
	r.pos += 2
	return v
}

// ReadUint32 reads a little-endian uint32 and advances the position by 4.
// Returns 0 and sets error on short read.
func (r *Reader) ReadUint32() uint32 {
	if !r.require(4) {
		return 0
	}
	v := binary.LittleEndian.Uint32(r.data[r.pos:])
	r.pos += 4
	return v
}

// ReadUint64 reads a little-endian uint64 and advances the position by 8.
// Returns 0 and sets error on short read.
func (r *Reader) ReadUint64() uint64 {
	if !r.require(8) {
		return 0
	}
	v := binary.LittleEndian.Uint64(r.data[r.pos:])
	r.pos += 8
	return v
}

// ReadBytes reads n bytes and advances the position.
// Returns nil and sets error if insufficient data.
func (r *Reader) ReadBytes(n int) []byte {
	if !r.require(n) {
		return nil
	}
	b := make([]byte, n)
	copy(b, r.data[r.pos:r.pos+n])
	r.pos += n
	return b
}

// Skip advances the position by n bytes without reading.
// Sets error if insufficient data.
func (r *Reader) Skip(n int) {
	if !r.require(n) {
		return
	}
	r.pos += n
}

// ExpectUint16 reads a uint16 and sets error if the value does not match expected.
func (r *Reader) ExpectUint16(expected uint16) {
	v := r.ReadUint16()
	if r.err != nil {
		return
	}
	if v != expected {
		r.err = fmt.Errorf("%w: expected 0x%04X, got 0x%04X at offset %d", ErrExpectMismatch, expected, v, r.pos-2)
	}
}

// EnsureRemaining sets error if fewer than n bytes remain. Does not consume bytes.
func (r *Reader) EnsureRemaining(n int) {
	r.require(n)
}

// Err returns the first error encountered, or nil.
func (r *Reader) Err() error {
	return r.err
}

// Remaining returns the number of unread bytes.
func (r *Reader) Remaining() int {
	return max(len(r.data)-r.pos, 0)
}

// Position returns the current read position.
func (r *Reader) Position() int {
	return r.pos
}

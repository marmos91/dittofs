package smbenc

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestNewWriter(t *testing.T) {
	w := NewWriter(64)
	if w.Len() != 0 {
		t.Errorf("expected length 0, got %d", w.Len())
	}
	if w.Err() != nil {
		t.Errorf("expected no error, got %v", w.Err())
	}
}

func TestWriterWriteUint16(t *testing.T) {
	w := NewWriter(2)
	w.WriteUint16(0x0311)
	if w.Err() != nil {
		t.Fatalf("unexpected error: %v", w.Err())
	}
	b := w.Bytes()
	if len(b) != 2 {
		t.Fatalf("expected 2 bytes, got %d", len(b))
	}
	v := binary.LittleEndian.Uint16(b)
	if v != 0x0311 {
		t.Errorf("expected 0x0311, got 0x%04X", v)
	}
}

func TestWriterWriteUint32(t *testing.T) {
	w := NewWriter(4)
	w.WriteUint32(0xDEADBEEF)
	if w.Err() != nil {
		t.Fatalf("unexpected error: %v", w.Err())
	}
	b := w.Bytes()
	if len(b) != 4 {
		t.Fatalf("expected 4 bytes, got %d", len(b))
	}
	v := binary.LittleEndian.Uint32(b)
	if v != 0xDEADBEEF {
		t.Errorf("expected 0xDEADBEEF, got 0x%08X", v)
	}
}

func TestWriterWriteUint64(t *testing.T) {
	w := NewWriter(8)
	w.WriteUint64(0x0102030405060708)
	if w.Err() != nil {
		t.Fatalf("unexpected error: %v", w.Err())
	}
	b := w.Bytes()
	if len(b) != 8 {
		t.Fatalf("expected 8 bytes, got %d", len(b))
	}
	v := binary.LittleEndian.Uint64(b)
	if v != 0x0102030405060708 {
		t.Errorf("expected 0x0102030405060708, got 0x%016X", v)
	}
}

func TestWriterWriteBytes(t *testing.T) {
	w := NewWriter(4)
	w.WriteBytes([]byte{0xAA, 0xBB, 0xCC, 0xDD})
	if w.Err() != nil {
		t.Fatalf("unexpected error: %v", w.Err())
	}
	b := w.Bytes()
	expected := []byte{0xAA, 0xBB, 0xCC, 0xDD}
	if !bytes.Equal(b, expected) {
		t.Errorf("expected %v, got %v", expected, b)
	}
}

func TestWriterWriteZeros(t *testing.T) {
	w := NewWriter(4)
	w.WriteZeros(4)
	if w.Err() != nil {
		t.Fatalf("unexpected error: %v", w.Err())
	}
	b := w.Bytes()
	expected := []byte{0x00, 0x00, 0x00, 0x00}
	if !bytes.Equal(b, expected) {
		t.Errorf("expected %v, got %v", expected, b)
	}
}

func TestWriterPadAlignment8(t *testing.T) {
	tests := []struct {
		name          string
		initialBytes  int // Write this many bytes before Pad
		expectedTotal int // Expected total length after Pad(8)
	}{
		{"0 bytes (already aligned)", 0, 0},
		{"1 byte", 1, 8},
		{"2 bytes", 2, 8},
		{"3 bytes", 3, 8},
		{"4 bytes", 4, 8},
		{"5 bytes", 5, 8},
		{"6 bytes", 6, 8},
		{"7 bytes", 7, 8},
		{"8 bytes (already aligned)", 8, 8},
		{"9 bytes", 9, 16},
		{"15 bytes", 15, 16},
		{"16 bytes (already aligned)", 16, 16},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := NewWriter(32)
			for i := 0; i < tt.initialBytes; i++ {
				w.WriteBytes([]byte{0xFF})
			}
			w.Pad(8)
			if w.Err() != nil {
				t.Fatalf("unexpected error: %v", w.Err())
			}
			if w.Len() != tt.expectedTotal {
				t.Errorf("expected total %d bytes, got %d", tt.expectedTotal, w.Len())
			}
			// Verify padding bytes are zero
			b := w.Bytes()
			for i := tt.initialBytes; i < tt.expectedTotal; i++ {
				if b[i] != 0 {
					t.Errorf("expected zero padding at index %d, got 0x%02X", i, b[i])
				}
			}
		})
	}
}

func TestWriterPadAlignment4(t *testing.T) {
	w := NewWriter(8)
	w.WriteBytes([]byte{0x01, 0x02, 0x03}) // 3 bytes
	w.Pad(4)                               // Should pad to 4
	if w.Err() != nil {
		t.Fatalf("unexpected error: %v", w.Err())
	}
	if w.Len() != 4 {
		t.Errorf("expected 4 bytes, got %d", w.Len())
	}
}

func TestWriterWriteAt(t *testing.T) {
	w := NewWriter(16)
	w.WriteUint32(0) // Placeholder at offset 0
	w.WriteUint16(0x0311)
	w.WriteUint16(0x0001)

	// Backpatch the uint32 at offset 0
	backpatch := make([]byte, 4)
	binary.LittleEndian.PutUint32(backpatch, 0xCAFEBABE)
	w.WriteAt(0, backpatch)

	if w.Err() != nil {
		t.Fatalf("unexpected error: %v", w.Err())
	}

	b := w.Bytes()
	v := binary.LittleEndian.Uint32(b[0:4])
	if v != 0xCAFEBABE {
		t.Errorf("expected 0xCAFEBABE at offset 0, got 0x%08X", v)
	}
	// Original data after offset 4 should be unchanged
	v16 := binary.LittleEndian.Uint16(b[4:6])
	if v16 != 0x0311 {
		t.Errorf("expected 0x0311 at offset 4, got 0x%04X", v16)
	}
}

func TestWriterWriteAtOutOfBounds(t *testing.T) {
	w := NewWriter(4)
	w.WriteUint16(0x0001)
	// Writing 4 bytes at offset 0 when only 2 bytes exist
	w.WriteAt(0, []byte{0x01, 0x02, 0x03, 0x04})
	if w.Err() == nil {
		t.Fatal("expected error for WriteAt out of bounds")
	}
}

func TestWriterEmptyWriteBytes(t *testing.T) {
	w := NewWriter(0)
	w.WriteBytes(nil)
	if w.Err() != nil {
		t.Fatalf("unexpected error: %v", w.Err())
	}
	if w.Len() != 0 {
		t.Errorf("expected length 0, got %d", w.Len())
	}
}

func TestReaderWriterRoundtrip(t *testing.T) {
	// Write a sequence of values
	w := NewWriter(32)
	w.WriteUint16(0x0311)
	w.WriteUint32(0xDEADBEEF)
	w.WriteBytes([]byte{0xAA, 0xBB})
	w.WriteUint64(0x0102030405060708)
	if w.Err() != nil {
		t.Fatalf("write error: %v", w.Err())
	}

	// Read them back
	r := NewReader(w.Bytes())
	v16 := r.ReadUint16()
	v32 := r.ReadUint32()
	b := r.ReadBytes(2)
	v64 := r.ReadUint64()
	if r.Err() != nil {
		t.Fatalf("read error: %v", r.Err())
	}

	if v16 != 0x0311 {
		t.Errorf("uint16: expected 0x0311, got 0x%04X", v16)
	}
	if v32 != 0xDEADBEEF {
		t.Errorf("uint32: expected 0xDEADBEEF, got 0x%08X", v32)
	}
	if !bytes.Equal(b, []byte{0xAA, 0xBB}) {
		t.Errorf("bytes: expected [0xAA, 0xBB], got %v", b)
	}
	if v64 != 0x0102030405060708 {
		t.Errorf("uint64: expected 0x0102030405060708, got 0x%016X", v64)
	}
}

func TestWriterGrowsBeyondCapacity(t *testing.T) {
	w := NewWriter(2) // Start with small capacity
	w.WriteUint32(0xDEADBEEF)
	w.WriteUint32(0xCAFEBABE)
	if w.Err() != nil {
		t.Fatalf("unexpected error: %v", w.Err())
	}
	if w.Len() != 8 {
		t.Errorf("expected length 8, got %d", w.Len())
	}
	b := w.Bytes()
	v1 := binary.LittleEndian.Uint32(b[0:4])
	v2 := binary.LittleEndian.Uint32(b[4:8])
	if v1 != 0xDEADBEEF {
		t.Errorf("expected 0xDEADBEEF, got 0x%08X", v1)
	}
	if v2 != 0xCAFEBABE {
		t.Errorf("expected 0xCAFEBABE, got 0x%08X", v2)
	}
}

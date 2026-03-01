package smbenc

import (
	"testing"
)

func TestNewReader(t *testing.T) {
	data := []byte{0x01, 0x02, 0x03, 0x04}
	r := NewReader(data)
	if r.Position() != 0 {
		t.Errorf("expected position 0, got %d", r.Position())
	}
	if r.Remaining() != 4 {
		t.Errorf("expected remaining 4, got %d", r.Remaining())
	}
	if r.Err() != nil {
		t.Errorf("expected no error, got %v", r.Err())
	}
}

func TestNewReaderNilData(t *testing.T) {
	r := NewReader(nil)
	if r.Position() != 0 {
		t.Errorf("expected position 0, got %d", r.Position())
	}
	if r.Remaining() != 0 {
		t.Errorf("expected remaining 0, got %d", r.Remaining())
	}
}

func TestReaderReadUint16(t *testing.T) {
	// LE encoding of 0x0201
	data := []byte{0x01, 0x02}
	r := NewReader(data)
	v := r.ReadUint16()
	if r.Err() != nil {
		t.Fatalf("unexpected error: %v", r.Err())
	}
	if v != 0x0201 {
		t.Errorf("expected 0x0201, got 0x%04X", v)
	}
	if r.Position() != 2 {
		t.Errorf("expected position 2, got %d", r.Position())
	}
	if r.Remaining() != 0 {
		t.Errorf("expected remaining 0, got %d", r.Remaining())
	}
}

func TestReaderReadUint32(t *testing.T) {
	// LE encoding of 0x04030201
	data := []byte{0x01, 0x02, 0x03, 0x04}
	r := NewReader(data)
	v := r.ReadUint32()
	if r.Err() != nil {
		t.Fatalf("unexpected error: %v", r.Err())
	}
	if v != 0x04030201 {
		t.Errorf("expected 0x04030201, got 0x%08X", v)
	}
	if r.Position() != 4 {
		t.Errorf("expected position 4, got %d", r.Position())
	}
}

func TestReaderReadUint64(t *testing.T) {
	// LE encoding of 0x0807060504030201
	data := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	r := NewReader(data)
	v := r.ReadUint64()
	if r.Err() != nil {
		t.Fatalf("unexpected error: %v", r.Err())
	}
	if v != 0x0807060504030201 {
		t.Errorf("expected 0x0807060504030201, got 0x%016X", v)
	}
	if r.Position() != 8 {
		t.Errorf("expected position 8, got %d", r.Position())
	}
}

func TestReaderReadBytes(t *testing.T) {
	data := []byte{0xAA, 0xBB, 0xCC, 0xDD}
	r := NewReader(data)
	b := r.ReadBytes(3)
	if r.Err() != nil {
		t.Fatalf("unexpected error: %v", r.Err())
	}
	if len(b) != 3 {
		t.Fatalf("expected 3 bytes, got %d", len(b))
	}
	if b[0] != 0xAA || b[1] != 0xBB || b[2] != 0xCC {
		t.Errorf("unexpected bytes: %v", b)
	}
	if r.Position() != 3 {
		t.Errorf("expected position 3, got %d", r.Position())
	}
}

func TestReaderSkip(t *testing.T) {
	data := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06}
	r := NewReader(data)
	r.Skip(4)
	if r.Err() != nil {
		t.Fatalf("unexpected error: %v", r.Err())
	}
	if r.Position() != 4 {
		t.Errorf("expected position 4, got %d", r.Position())
	}
	v := r.ReadUint16()
	if r.Err() != nil {
		t.Fatalf("unexpected error: %v", r.Err())
	}
	if v != 0x0605 {
		t.Errorf("expected 0x0605, got 0x%04X", v)
	}
}

func TestReaderShortReadUint16(t *testing.T) {
	data := []byte{0x01} // Only 1 byte, need 2
	r := NewReader(data)
	v := r.ReadUint16()
	if r.Err() == nil {
		t.Fatal("expected error for short read")
	}
	if v != 0 {
		t.Errorf("expected 0 on error, got %d", v)
	}
}

func TestReaderShortReadUint32(t *testing.T) {
	data := []byte{0x01, 0x02} // Only 2 bytes, need 4
	r := NewReader(data)
	v := r.ReadUint32()
	if r.Err() == nil {
		t.Fatal("expected error for short read")
	}
	if v != 0 {
		t.Errorf("expected 0 on error, got %d", v)
	}
}

func TestReaderShortReadUint64(t *testing.T) {
	data := []byte{0x01, 0x02, 0x03, 0x04} // Only 4 bytes, need 8
	r := NewReader(data)
	v := r.ReadUint64()
	if r.Err() == nil {
		t.Fatal("expected error for short read")
	}
	if v != 0 {
		t.Errorf("expected 0 on error, got %d", v)
	}
}

func TestReaderShortReadBytes(t *testing.T) {
	data := []byte{0x01, 0x02}
	r := NewReader(data)
	b := r.ReadBytes(5) // Need 5, have 2
	if r.Err() == nil {
		t.Fatal("expected error for short read")
	}
	if b != nil {
		t.Errorf("expected nil on error, got %v", b)
	}
}

func TestReaderShortSkip(t *testing.T) {
	data := []byte{0x01, 0x02}
	r := NewReader(data)
	r.Skip(5) // Need 5, have 2
	if r.Err() == nil {
		t.Fatal("expected error for short skip")
	}
}

func TestReaderErrorAccumulation(t *testing.T) {
	// After the first error, all subsequent reads should return zero/nil
	data := []byte{0x01}
	r := NewReader(data)

	// First read fails (need 2 bytes)
	v16 := r.ReadUint16()
	if r.Err() == nil {
		t.Fatal("expected error")
	}
	if v16 != 0 {
		t.Errorf("expected 0, got %d", v16)
	}

	// Subsequent reads should be no-ops returning zero values
	v32 := r.ReadUint32()
	if v32 != 0 {
		t.Errorf("expected 0, got %d", v32)
	}

	v64 := r.ReadUint64()
	if v64 != 0 {
		t.Errorf("expected 0, got %d", v64)
	}

	b := r.ReadBytes(1)
	if b != nil {
		t.Errorf("expected nil, got %v", b)
	}

	r.Skip(1) // Should not panic

	// Error should still be the original error
	if r.Err() == nil {
		t.Fatal("error should still be set")
	}
}

func TestReaderExpectUint16Success(t *testing.T) {
	data := []byte{0x11, 0x03} // LE 0x0311
	r := NewReader(data)
	r.ExpectUint16(0x0311)
	if r.Err() != nil {
		t.Fatalf("unexpected error: %v", r.Err())
	}
	if r.Position() != 2 {
		t.Errorf("expected position 2, got %d", r.Position())
	}
}

func TestReaderExpectUint16Failure(t *testing.T) {
	data := []byte{0x11, 0x03} // LE 0x0311
	r := NewReader(data)
	r.ExpectUint16(0x0202) // Expect different value
	if r.Err() == nil {
		t.Fatal("expected error for mismatch")
	}
}

func TestReaderEnsureRemainingSuccess(t *testing.T) {
	data := []byte{0x01, 0x02, 0x03, 0x04}
	r := NewReader(data)
	r.EnsureRemaining(4)
	if r.Err() != nil {
		t.Fatalf("unexpected error: %v", r.Err())
	}
	// Position should NOT change
	if r.Position() != 0 {
		t.Errorf("expected position 0, got %d", r.Position())
	}
}

func TestReaderEnsureRemainingFailure(t *testing.T) {
	data := []byte{0x01, 0x02}
	r := NewReader(data)
	r.EnsureRemaining(5)
	if r.Err() == nil {
		t.Fatal("expected error for insufficient data")
	}
	// Position should NOT change
	if r.Position() != 0 {
		t.Errorf("expected position 0, got %d", r.Position())
	}
}

func TestReaderEmptyData(t *testing.T) {
	r := NewReader([]byte{})
	if r.Remaining() != 0 {
		t.Errorf("expected remaining 0, got %d", r.Remaining())
	}
	v := r.ReadUint16()
	if v != 0 {
		t.Errorf("expected 0, got %d", v)
	}
	if r.Err() == nil {
		t.Fatal("expected error reading from empty reader")
	}
}

func TestReaderMultipleReads(t *testing.T) {
	// 2 + 4 + 2 = 8 bytes
	data := []byte{
		0x01, 0x00, // uint16 = 1
		0x02, 0x00, 0x00, 0x00, // uint32 = 2
		0x03, 0x00, // uint16 = 3
	}
	r := NewReader(data)
	v1 := r.ReadUint16()
	v2 := r.ReadUint32()
	v3 := r.ReadUint16()
	if r.Err() != nil {
		t.Fatalf("unexpected error: %v", r.Err())
	}
	if v1 != 1 {
		t.Errorf("v1: expected 1, got %d", v1)
	}
	if v2 != 2 {
		t.Errorf("v2: expected 2, got %d", v2)
	}
	if v3 != 3 {
		t.Errorf("v3: expected 3, got %d", v3)
	}
	if r.Remaining() != 0 {
		t.Errorf("expected remaining 0, got %d", r.Remaining())
	}
}
